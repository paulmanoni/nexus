// Package jobsamqp runs background jobs on RabbitMQ — the "rabbitmq" driver
// of extension/jobs. Jobs are persistent messages in quorum queues: an
// enqueue returns once the broker confirms it, a worker acknowledges only
// after the job finishes (so a crashed worker's job is redelivered), and
// every process consuming the queue shares the work.
//
//	nexus.Boot(
//	    jobsamqp.Bind(jobsamqp.Config{}),   // [jobs.rabbitmq] url, else RABBIT_URL
//	    jobs.Module(jobs.Config{}),
//	    ExportReport,
//	)
//
// For each job queue it declares:
//
//	nexus.jobs.<queue>                 quorum queue: the work
//	nexus.jobs.<queue>.failed          quorum queue: jobs that failed for good
//	nexus.jobs.<queue>.delay.<ms>      TTL queues holding delayed jobs and
//	                                   retries until due (auto-deleted when idle)
//
// Long jobs: RabbitMQ closes a channel whose delivery stays unacknowledged
// past the consumer timeout (30 minutes by default), and the job would run
// again. The work queues are declared with x-consumer-timeout =
// Config.ConsumerTimeout (default 8h; RabbitMQ 3.12+) — keep it above your
// longest jobs.Timeout. The broker keeps no per-job state, so Manager.Get,
// Cancel and List are unsupported with this driver.
//
// A separate module, so an app that doesn't use it links no AMQP client.
package jobsamqp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/extension/jobs"
)

// Config says where the queues live and how they are declared. Zero values
// read [jobs.rabbitmq] in nexus.toml (url, prefix, consumer_timeout,
// delivery_limit), then RABBIT_URL, then amqp://guest:guest@localhost:5672/.
type Config struct {
	URL string

	// Prefix names the queues (default "nexus.jobs.").
	Prefix string

	// ConsumerTimeout is how long a delivery may stay unacknowledged — the
	// longest a job may run (default 8h). Negative leaves the broker's
	// default. Changing it later means deleting the queues first: RabbitMQ
	// refuses to redeclare a queue with different arguments.
	ConsumerTimeout time.Duration

	// DeliveryLimit dead-letters a message delivered this many times without
	// being settled — a job that keeps crashing its worker (default 20).
	DeliveryLimit int
}

func (c Config) resolve() Config {
	if c.URL == "" {
		c.URL = nexus.Get("jobs.rabbitmq.url", os.Getenv("RABBIT_URL"))
	}
	if c.URL == "" {
		c.URL = "amqp://guest:guest@localhost:5672/"
	}
	if c.Prefix == "" {
		c.Prefix = nexus.Get("jobs.rabbitmq.prefix", "nexus.jobs.")
	}
	if c.ConsumerTimeout == 0 {
		c.ConsumerTimeout = nexus.Get("jobs.rabbitmq.consumer_timeout", 8*time.Hour)
	}
	if c.DeliveryLimit == 0 {
		c.DeliveryLimit = nexus.Get("jobs.rabbitmq.delivery_limit", 20)
	}
	return c
}

// Bind provides the jobs.Broker; with it in DI, jobs.Module uses the
// rabbitmq driver. The connection opens on first use and closes on shutdown.
func Bind(cfg Config) nexus.Option {
	return nexus.Provide(func(lc nexus.Lifecycle) (jobs.Broker, error) {
		b, err := New(cfg)
		if err != nil {
			return nil, err
		}
		lc.Append(nexus.Hook{OnStop: func(context.Context) error { return b.Close() }})
		return b, nil
	})
}

// Broker is the RabbitMQ jobs.Broker.
type Broker struct {
	cfg Config

	mu       sync.Mutex
	conn     *amqp.Connection
	pub      *amqp.Channel
	declared map[string]bool // queues declared on the current connection
}

// New returns a Broker for cfg; it connects on first use.
func New(cfg Config) (*Broker, error) {
	cfg = cfg.resolve()
	if _, err := amqp.ParseURI(cfg.URL); err != nil {
		return nil, fmt.Errorf("jobsamqp: url: %w", err)
	}
	return &Broker{cfg: cfg, declared: map[string]bool{}}, nil
}

func (b *Broker) Driver() string { return "rabbitmq" }

// Close closes the connection.
func (b *Broker) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil && !b.conn.IsClosed() {
		return b.conn.Close()
	}
	return nil
}

// connection returns a live connection, dialing when there is none.
// Callers hold b.mu.
func (b *Broker) connection() (*amqp.Connection, error) {
	if b.conn != nil && !b.conn.IsClosed() {
		return b.conn, nil
	}
	conn, err := amqp.DialConfig(b.cfg.URL, amqp.Config{Properties: amqp.Table{"connection_name": "nexus jobs"}})
	if err != nil {
		return nil, fmt.Errorf("jobsamqp: connecting: %w", err)
	}
	b.conn, b.pub, b.declared = conn, nil, map[string]bool{}
	return conn, nil
}

// publisher returns the confirm-mode publishing channel. Callers hold b.mu.
func (b *Broker) publisher() (*amqp.Channel, error) {
	conn, err := b.connection()
	if err != nil {
		return nil, err
	}
	if b.pub != nil && !b.pub.IsClosed() {
		return b.pub, nil
	}
	ch, err := conn.Channel()
	if err != nil {
		return nil, err
	}
	if err := ch.Confirm(false); err != nil {
		_ = ch.Close()
		return nil, err
	}
	b.pub, b.declared = ch, map[string]bool{}
	return ch, nil
}

func (b *Broker) workQueue(q string) string   { return b.cfg.Prefix + q }
func (b *Broker) failedQueue(q string) string { return b.cfg.Prefix + q + ".failed" }
func (b *Broker) delayQueue(q string, d time.Duration) string {
	return b.cfg.Prefix + q + ".delay." + strconv.FormatInt(d.Milliseconds(), 10)
}

// declareWork declares a job queue's work and failed queues on ch.
func (b *Broker) declareWork(ch *amqp.Channel, q string) error {
	if _, err := ch.QueueDeclare(b.failedQueue(q), true, false, false, false,
		amqp.Table{"x-queue-type": "quorum"}); err != nil {
		return fmt.Errorf("jobsamqp: declaring %s: %w", b.failedQueue(q), err)
	}
	args := amqp.Table{
		"x-queue-type":              "quorum",
		"x-delivery-limit":          int64(b.cfg.DeliveryLimit),
		"x-dead-letter-exchange":    "",
		"x-dead-letter-routing-key": b.failedQueue(q),
	}
	if b.cfg.ConsumerTimeout > 0 {
		args["x-consumer-timeout"] = b.cfg.ConsumerTimeout.Milliseconds()
	}
	if _, err := ch.QueueDeclare(b.workQueue(q), true, false, false, false, args); err != nil {
		return fmt.Errorf("jobsamqp: declaring %s (a queue declared earlier with other arguments must be deleted first): %w", b.workQueue(q), err)
	}
	return nil
}

// ensureDeclared declares name once per connection. Callers hold b.mu.
func (b *Broker) ensureDeclared(ch *amqp.Channel, name string, declare func() error) error {
	if b.declared[name] {
		return nil
	}
	if err := declare(); err != nil {
		return err
	}
	b.declared[name] = true
	return nil
}

// publish sends body to queue and waits for the broker's confirm.
func (b *Broker) publish(ctx context.Context, route func(ch *amqp.Channel) (string, error), rec jobs.Record) error {
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	b.mu.Lock()
	ch, err := b.publisher()
	if err != nil {
		b.mu.Unlock()
		return err
	}
	queue, err := route(ch)
	if err != nil {
		b.pub = nil // a failed declare closes the channel
		b.mu.Unlock()
		return err
	}
	dc, err := ch.PublishWithDeferredConfirmWithContext(ctx, "", queue, true, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		MessageId:    string(rec.ID),
		Timestamp:    time.Now(),
		Type:         rec.Name,
		Body:         body,
	})
	b.mu.Unlock()
	if err != nil {
		return fmt.Errorf("jobsamqp: publishing to %s: %w", queue, err)
	}
	ok, err := dc.WaitContext(ctx)
	if err != nil {
		return fmt.Errorf("jobsamqp: waiting for the broker to confirm: %w", err)
	}
	if !ok {
		return fmt.Errorf("jobsamqp: the broker refused the message for %s", queue)
	}
	return nil
}

// Publish sends rec to its queue — through a delay queue when rec.RunAt is
// ahead (rounded up to the second, to keep the number of delay queues small).
func (b *Broker) Publish(ctx context.Context, rec jobs.Record) error {
	return b.publish(ctx, func(ch *amqp.Channel) (string, error) {
		q := rec.Queue
		if err := b.ensureDeclared(ch, b.workQueue(q), func() error { return b.declareWork(ch, q) }); err != nil {
			return "", err
		}
		wait := time.Until(rec.RunAt)
		if wait <= 0 {
			return b.workQueue(q), nil
		}
		wait = (wait + time.Second - 1).Truncate(time.Second)
		name := b.delayQueue(q, wait)
		err := b.ensureDeclared(ch, name, func() error {
			_, err := ch.QueueDeclare(name, true, false, false, false, amqp.Table{
				"x-message-ttl":             wait.Milliseconds(),
				"x-dead-letter-exchange":    "",
				"x-dead-letter-routing-key": b.workQueue(q),
				"x-expires":                 (wait + 10*time.Minute).Milliseconds(),
			})
			return err
		})
		return name, err
	}, rec)
}

// Bury moves rec to its queue's failed queue.
func (b *Broker) Bury(ctx context.Context, rec jobs.Record) error {
	return b.publish(ctx, func(ch *amqp.Channel) (string, error) {
		q := rec.Queue
		err := b.ensureDeclared(ch, b.workQueue(q), func() error { return b.declareWork(ch, q) })
		return b.failedQueue(q), err
	}, rec)
}

// Depth is the number of jobs waiting in queue's work queue.
func (b *Broker) Depth(_ context.Context, queue string) (int, error) {
	b.mu.Lock()
	conn, err := b.connection()
	b.mu.Unlock()
	if err != nil {
		return 0, err
	}
	ch, err := conn.Channel()
	if err != nil {
		return 0, err
	}
	defer ch.Close()
	q, err := ch.QueueDeclarePassive(b.workQueue(queue), true, false, false, false, nil)
	if err != nil {
		var ae *amqp.Error
		if errors.As(err, &ae) && ae.Code == amqp.NotFound {
			return 0, nil // declared on first enqueue
		}
		return 0, err
	}
	return q.Messages, nil
}

// Consume delivers queue's jobs to handle with at most concurrency
// unacknowledged at a time, until ctx ends or the connection drops. On ctx's
// end it stops taking deliveries but keeps the channel open until the
// handlers running settle theirs — closing it would hand them to another
// consumer while they still run.
func (b *Broker) Consume(ctx context.Context, queue string, concurrency int, handle func(jobs.Delivery)) error {
	b.mu.Lock()
	conn, err := b.connection()
	b.mu.Unlock()
	if err != nil {
		return err
	}
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()
	if err := b.declareWork(ch, queue); err != nil {
		return err
	}
	if err := ch.Qos(concurrency, 0, false); err != nil {
		return err
	}
	tag := fmt.Sprintf("nexus-jobs-%d-%d", os.Getpid(), time.Now().UnixNano())
	deliveries, err := ch.ConsumeWithContext(context.Background(), b.workQueue(queue), tag, false, false, false, false, nil)
	if err != nil {
		return err
	}
	closed := ch.NotifyClose(make(chan *amqp.Error, 1))
	// On a lost connection the broker has already handed the unacknowledged
	// jobs to other consumers; the handlers still running here finish (their
	// acks fail harmlessly) while this returns to reconnect.
	var running sync.WaitGroup
	for {
		select {
		case <-ctx.Done():
			_ = ch.Cancel(tag, false)
			running.Wait()
			return ctx.Err()
		case e := <-closed:
			if e == nil {
				return errors.New("jobsamqp: channel closed")
			}
			return e
		case d, ok := <-deliveries:
			if !ok {
				return errors.New("jobsamqp: delivery channel closed")
			}
			var rec jobs.Record
			if err := json.Unmarshal(d.Body, &rec); err != nil {
				// Not a job we can read: dead-letter it rather than loop.
				_ = d.Nack(false, false)
				continue
			}
			rec.Attempt++
			rec.State = jobs.StateRunning
			rec.StartedAt = time.Now()
			running.Add(1)
			go func() {
				defer running.Done()
				handle(delivery{d: d, rec: rec})
			}()
		}
	}
}

type delivery struct {
	d   amqp.Delivery
	rec jobs.Record
}

func (d delivery) Record() jobs.Record { return d.rec }
func (d delivery) Ack() error          { return d.d.Ack(false) }
func (d delivery) Requeue() error      { return d.d.Nack(false, true) }
