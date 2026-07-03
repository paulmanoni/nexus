package pubsub

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"
)

// InMemoryTransport is the zero-dep test/dev transport. Behaves like
// a real broker for the dispatcher's purposes: durable across the
// process's lifetime, fan-out to multiple subscriptions on the same
// topic, retry with exponential backoff up to MaxRetries, then DLQ.
//
// Not safe to share across processes — there is no broker, no socket,
// no persistence to disk. Useful surfaces:
//   - Unit tests: NewInMemoryTransport(), publish, assert on
//     transport.DLQ(topic, sub).
//   - `nexus dev` without RabbitMQ available locally: the framework
//     falls back to in-memory if no UseRabbit option was selected, so
//     a developer can iterate on subscriber code without standing up
//     a broker.
type InMemoryTransport struct {
	mu sync.Mutex

	// queues keyed by (topic, subscription). Each subscription gets
	// its own queue + goroutine so fan-out works: publishing to a
	// topic delivers to every subscription bound to it.
	queues map[string]*memQueue

	// subsByTopic indexes subscriptions per topic so Publish can
	// fan out without walking the full map.
	subsByTopic map[string][]string

	// dlq stores the messages each subscription has rejected, in the
	// order they arrived. Tests inspect this via DLQ(topic, sub).
	dlq map[string][]Message

	// closed flips once Close() returns; subsequent Publish calls
	// fail rather than silently buffering into a transport that
	// will never drain.
	closed bool

	// nowFn lets tests substitute a deterministic clock. Production
	// uses time.Now; tests that care about backoff timing inject a
	// fake clock through the unexported helper newTestTransport.
	nowFn func() time.Time
}

// NewInMemoryTransport returns a fresh in-memory broker. Multiple
// instances are isolated — one transport's queues do not see another's
// publishes. The fx-provided instance is created once per app boot.
func NewInMemoryTransport() *InMemoryTransport {
	return &InMemoryTransport{
		queues:      map[string]*memQueue{},
		subsByTopic: map[string][]string{},
		dlq:         map[string][]Message{},
		nowFn:       time.Now,
	}
}

// memQueue is one subscription's pending-message buffer. The Consume
// goroutine reads from ch; Publish fan-out writes into ch. cap is
// 1024 — large enough for realistic test bursts, small enough that a
// truly stuck handler trips an obvious deadlock instead of OOMing.
type memQueue struct {
	topic string
	sub   string
	ch    chan Message
	once  sync.Once
}

const memQueueCap = 1024

// queueKey is the map key for queues + dlq. "topic\x00sub" — a NUL
// separator avoids collisions if a topic name happens to contain the
// separator string.
func queueKey(topic, sub string) string { return topic + "\x00" + sub }

// Publish writes one copy of payload into each subscription queue
// bound to topic. Returns an error only if the transport is closed
// or if a queue is full (signaling backpressure that tests should
// surface rather than mask).
func (m *InMemoryTransport) Publish(_ context.Context, topic string, payload []byte, attrs map[string]string) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("pubsub: in-memory transport is closed")
	}
	subs := append([]string(nil), m.subsByTopic[topic]...)
	m.mu.Unlock()

	for _, sub := range subs {
		key := queueKey(topic, sub)
		m.mu.Lock()
		q := m.queues[key]
		m.mu.Unlock()
		if q == nil {
			continue
		}
		msg := Message{
			Topic:           topic,
			Subscription:    sub,
			Payload:         append([]byte(nil), payload...), // copy: caller may reuse buffer
			Attrs:           attrs,
			DeliveryAttempt: 1,
		}
		select {
		case q.ch <- msg:
		default:
			return errors.New("pubsub: in-memory queue full for " + key + " (raise memQueueCap or unblock the consumer)")
		}
	}
	return nil
}

// Consume registers (topic, subscription) — creating the queue + DLQ
// slot if this is the first subscriber — and runs the deliver loop
// until ctx is cancelled.
//
// On Disposition results:
//   - Ack: drop the message (tests can inspect via Acked() if needed).
//   - Retry: sleep for the computed backoff, increment DeliveryAttempt,
//     re-push onto the queue. After MaxRetries hits, the next failure
//     promotes to DLQ.
//   - DLQ: record onto m.dlq[key]. Do not redeliver.
//
// Backoff sleeps are interrupted by ctx cancellation so an app
// shutdown doesn't wait the full retry delay before exiting.
func (m *InMemoryTransport) Consume(ctx context.Context, topic, subscription string, cfg ConsumeConfig, deliver Deliver) error {
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.BackoffMin <= 0 {
		cfg.BackoffMin = 100 * time.Millisecond
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = 30 * time.Second
	}

	key := queueKey(topic, subscription)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("pubsub: in-memory transport is closed")
	}
	q, ok := m.queues[key]
	if !ok {
		q = &memQueue{topic: topic, sub: subscription, ch: make(chan Message, memQueueCap)}
		m.queues[key] = q
		m.subsByTopic[topic] = append(m.subsByTopic[topic], subscription)
	}
	m.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, open := <-q.ch:
			if !open {
				return nil
			}
			disp := deliver(msg)
			switch disp {
			case DispositionAck:
				// nothing — message consumed.
			case DispositionDLQ:
				m.mu.Lock()
				m.dlq[key] = append(m.dlq[key], msg)
				m.mu.Unlock()
			case DispositionRetry:
				if msg.DeliveryAttempt >= cfg.MaxRetries {
					// Retry budget exhausted — promote to DLQ so the
					// queue doesn't loop forever on a poison message.
					m.mu.Lock()
					m.dlq[key] = append(m.dlq[key], msg)
					m.mu.Unlock()
					continue
				}
				delay := backoff(msg.DeliveryAttempt, cfg.BackoffMin, cfg.BackoffMax)
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return nil
				case <-timer.C:
				}
				msg.DeliveryAttempt++
				select {
				case q.ch <- msg:
				default:
					// Queue full at requeue time — promote to DLQ
					// rather than blocking the consume loop.
					m.mu.Lock()
					m.dlq[key] = append(m.dlq[key], msg)
					m.mu.Unlock()
				}
			}
		}
	}
}

// Close marks the transport closed and closes every queue channel
// so any blocked Consume goroutines exit cleanly. Idempotent.
func (m *InMemoryTransport) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	for _, q := range m.queues {
		q.once.Do(func() { close(q.ch) })
	}
	return nil
}

// DLQ returns a copy of the dead-letter slice for a (topic,
// subscription) pair. Tests use this to assert that retry-exhausted
// or poison messages landed where expected.
func (m *InMemoryTransport) DLQ(topic, subscription string) []Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	src := m.dlq[queueKey(topic, subscription)]
	if len(src) == 0 {
		return nil
	}
	out := make([]Message, len(src))
	copy(out, src)
	return out
}

// backoff is a deterministic exponential schedule:
//
//	attempt 1 → min, attempt 2 → 2*min, attempt 3 → 4*min, …, capped at max.
//
// No jitter — tests want predictability, and broker-side jitter (when
// the production transport reaches retry) is the layer that needs to
// avoid herd effects, not us.
func backoff(attempt int, min, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	mult := math.Pow(2, float64(attempt-1))
	d := time.Duration(float64(min) * mult)
	if d > max || d < 0 {
		return max
	}
	return d
}
