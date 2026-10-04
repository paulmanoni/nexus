package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// Broker is a message-broker driver (jobsamqp for RabbitMQ): jobs live as
// messages in the broker's queues rather than as records in a store. That
// gives push delivery and the broker's own durability, but a message can't
// be looked up, changed or cancelled by ID — so with a broker, Manager.Get,
// Cancel and List return ErrUnsupported, progress and results aren't kept
// (checkpoints travel with the message into the next attempt), and Unique
// is refused at boot.
type Broker interface {
	// Driver names the driver ("rabbitmq").
	Driver() string

	// Publish sends rec to rec.Queue. A RunAt in the future delays delivery
	// until then.
	Publish(ctx context.Context, rec Record) error

	// Bury sets aside a job that failed for good (a dead-letter queue), with
	// rec.Error saying why.
	Bury(ctx context.Context, rec Record) error

	// Consume delivers queue's jobs to handle — at most concurrency at a
	// time — until ctx ends; a lost connection returns an error (the manager
	// reconnects). Every delivery is settled once, by Ack or Requeue.
	Consume(ctx context.Context, queue string, concurrency int, handle func(Delivery)) error

	// Depth reports the messages waiting in queue, for the dashboard.
	Depth(ctx context.Context, queue string) (int, error)
}

// Delivery is one job handed over by a Broker.
type Delivery interface {
	// Record is the job; its Attempt already counts this delivery.
	Record() Record
	// Ack settles the delivery: the job is done with (it succeeded, or was
	// republished or buried).
	Ack() error
	// Requeue returns the message to its queue as it was.
	Requeue() error
}

// ErrUnsupported is returned by Manager.Get, Cancel and List when the
// driver is a message broker, which keeps no per-job state.
var ErrUnsupported = errors.New("jobs: not supported by a message-broker driver (it keeps no per-job state)")

// brokerCounts counts what this process did, for the dashboard.
type brokerCounts struct {
	running, succeeded, failed, retried atomic.Int64
}

// consume keeps a queue's consumer running until shutdown, reconnecting
// after a lost connection.
func (m *Manager) consume(queue string, concurrency int) {
	defer m.workers.Done()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-m.stopping; cancel() }()
	for {
		err := m.broker.Consume(ctx, queue, concurrency, m.handleDelivery)
		if ctx.Err() != nil {
			m.deliveries.Wait()
			return
		}
		m.log.Warn("jobs: consumer stopped, reconnecting", "queue", queue, "error", err)
		select {
		case <-ctx.Done():
			m.deliveries.Wait()
			return
		case <-time.After(m.cfg.PollInterval):
		}
	}
}

// handleDelivery runs one delivered job and settles the message.
func (m *Manager) handleDelivery(d Delivery) {
	m.deliveries.Add(1)
	defer m.deliveries.Done()
	rec := d.Record()
	m.mu.Lock()
	def, call := m.defs[rec.Name], m.calls[rec.Name]
	m.mu.Unlock()
	if def == nil || call == nil {
		m.bury(d, rec, fmt.Sprintf("no job named %q is registered in this process", rec.Name))
		return
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if def.timeout > 0 {
		ctx, cancel = context.WithTimeout(m.runCtx, def.timeout)
	} else {
		ctx, cancel = context.WithCancel(m.runCtx)
	}
	defer cancel()

	m.counts.running.Add(1)
	local := rec
	run := &Run{m: m, id: rec.ID, attempt: rec.Attempt, actor: rec.Actor, impersonator: rec.Impersonator, system: def.system, ctx: ctx, local: &local}
	err := m.call(call, ctx, run, rec.Args)
	m.counts.running.Add(-1)
	m.invalidateStats()

	run.mu.Lock()
	rec.Checkpoint = local.Checkpoint
	run.mu.Unlock()
	switch {
	case err == nil:
		m.counts.succeeded.Add(1)
		m.settleDelivery(d, rec, "ack")
	case m.runCtx.Err() != nil:
		// Interrupted by shutdown: back to the queue, the attempt uncounted.
		rec.Attempt--
		rec.RunAt = time.Now()
		m.republish(d, rec)
	default:
		var perm permanentError
		if rec.Attempt < def.maxAttempts && !errors.As(err, &perm) {
			wait := def.backoff(rec.Attempt)
			rec.Error, rec.RunAt = err.Error(), time.Now().Add(wait)
			m.counts.retried.Add(1)
			m.log.Warn("jobs: attempt failed, retrying", "job", rec.Name, "id", rec.ID,
				"attempt", rec.Attempt, "retryIn", wait, "error", err)
			m.republish(d, rec)
			return
		}
		m.log.Error("jobs: job failed", "job", rec.Name, "id", rec.ID, "attempts", rec.Attempt, "error", err)
		m.bury(d, rec, err.Error())
	}
}

// republish sends the job on again (a retry or a requeue after shutdown)
// and settles this delivery; if the broker won't take it, the original
// message goes back instead.
func (m *Manager) republish(d Delivery, rec Record) {
	ctx, cancel := m.storeCtx()
	defer cancel()
	if err := m.broker.Publish(ctx, rec); err != nil {
		m.log.Error("jobs: republishing failed — returning the message", "job", rec.Name, "id", rec.ID, "error", err)
		m.settleDelivery(d, rec, "requeue")
		return
	}
	m.settleDelivery(d, rec, "ack")
}

func (m *Manager) bury(d Delivery, rec Record, reason string) {
	m.counts.failed.Add(1)
	rec.State, rec.Error, rec.FinishedAt = StateFailed, reason, time.Now()
	ctx, cancel := m.storeCtx()
	defer cancel()
	if err := m.broker.Bury(ctx, rec); err != nil {
		m.log.Error("jobs: burying a failed job failed — returning the message", "job", rec.Name, "id", rec.ID, "error", err)
		m.settleDelivery(d, rec, "requeue")
		return
	}
	m.settleDelivery(d, rec, "ack")
}

func (m *Manager) settleDelivery(d Delivery, rec Record, how string) {
	var err error
	if how == "requeue" {
		err = d.Requeue()
	} else {
		err = d.Ack()
	}
	if err != nil {
		m.log.Warn("jobs: settling a delivery failed (it will be redelivered)", "job", rec.Name, "id", rec.ID, "error", err)
	}
}

// brokerStats reads queue depths from the broker; running, done and failed
// are this process's counts.
func (m *Manager) brokerStats(ctx context.Context) (Stats, error) {
	m.mu.Lock()
	queues := make([]string, 0, len(m.wake))
	for q := range m.wake {
		queues = append(queues, q)
	}
	m.mu.Unlock()
	st := Stats{Queues: map[string]QueueStats{}}
	for _, q := range queues {
		n, err := m.broker.Depth(ctx, q)
		if err != nil {
			return st, err
		}
		st.Queues[q] = QueueStats{Queued: n}
	}
	return st, nil
}
