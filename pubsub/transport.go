package pubsub

import (
	"context"
	"time"
)

// Transport is the broker-agnostic interface every adapter implements.
// The core pubsub package depends only on this; pubsub/rabbit and the
// in-memory transport satisfy it. New backends (NATS, Kafka) plug in
// the same way — implement Transport, expose a UseFoo() option that
// fx.Provide's it, and bindTopics handles the rest.
type Transport interface {
	// Publish sends payload to topic. Implementations must encode the
	// optional attrs onto the wire (RabbitMQ headers, NATS Header) so
	// subscribers see them on Message.Attrs. Returning an error fails
	// the publishing call directly — at-least-once durability is the
	// caller's responsibility (typically: retry on transient errors,
	// log + drop on permanent ones).
	Publish(ctx context.Context, topic string, payload []byte, attrs map[string]string) error

	// Consume blocks until ctx is cancelled, calling deliver for each
	// received message. The dispatcher wraps deliver with the retry +
	// DLQ logic, so adapters only need to implement the lower-level
	// "next message → ack/nack/requeue" loop.
	//
	// cfg carries the per-subscription tuning (max retries, ack
	// deadline). Adapters that don't natively support a feature
	// (e.g. in-memory has no real ack deadline) MAY ignore the
	// matching field; the dispatcher does not depend on broker-side
	// enforcement.
	Consume(ctx context.Context, topic, subscription string, cfg ConsumeConfig, deliver Deliver) error

	// Close releases adapter resources (RabbitMQ connection, in-mem
	// channels). Called from fx.Stop. Idempotent — calling on an
	// already-closed transport must not panic.
	Close() error
}

// ConsumeConfig is the per-subscription tuning passed from the
// Subscribe option to Transport.Consume. Defaults are filled in by
// the Subscribe wrapper before this struct reaches the adapter.
type ConsumeConfig struct {
	// MaxRetries is the maximum number of redelivery attempts before
	// the dispatcher routes the message to the DLQ. Counted across
	// the whole subscription lifetime; broker-level redelivery counts
	// are not consulted (different adapters track them differently).
	MaxRetries int

	// AckDeadline is the per-message handler timeout. The dispatcher
	// derives a context from the parent that cancels at this deadline,
	// so a stuck handler doesn't tie up the consume loop forever.
	AckDeadline time.Duration

	// BackoffMin / BackoffMax bracket the exponential backoff between
	// retry attempts. The dispatcher computes
	//     delay = min(BackoffMax, BackoffMin * 2^(attempt-1))
	// before re-presenting the message. Zero values pick framework
	// defaults (100ms / 30s).
	BackoffMin time.Duration
	BackoffMax time.Duration
}

// Message is one delivered envelope handed to the handler dispatcher.
// Adapters fill it from their wire format; the dispatcher does not
// care which broker produced it.
type Message struct {
	Topic        string
	Subscription string

	// Payload is the raw bytes from the broker. The Subscribe wrapper
	// runs Topic[T].decode before invoking the user's handler — so
	// handlers see typed T, not bytes.
	Payload []byte

	// Attrs is the optional metadata sent by the publisher (headers,
	// content-type, idempotency key). nil when none were set.
	Attrs map[string]string

	// DeliveryAttempt is 1 on first delivery, incremented on each
	// retry. The dispatcher uses this together with cfg.MaxRetries to
	// decide retry-vs-DLQ.
	DeliveryAttempt int
}

// Deliver is the function the dispatcher hands to Transport.Consume.
// Adapters call it once per received message and react to the
// returned Disposition: Ack → broker-acknowledge, Retry → re-enqueue
// with backoff, DLQ → route to dead-letter.
type Deliver func(msg Message) Disposition

// Disposition is the dispatcher's verdict for a delivered message.
// Adapters MUST honor the verdict — there is no "soft" Ack that the
// adapter can downgrade. Misbehaving adapters are caught by the
// in-memory test transport which asserts on every disposition.
type Disposition int

const (
	// DispositionAck — handler succeeded, broker can drop the message.
	DispositionAck Disposition = iota
	// DispositionRetry — handler failed transiently; re-enqueue after
	// backoff and increment DeliveryAttempt. Becomes DLQ once attempts
	// reach MaxRetries.
	DispositionRetry
	// DispositionDLQ — handler indicated the message is unprocessable
	// (poison payload, business-rule rejection, attempts exhausted).
	// Adapter routes to dead-letter and does NOT redeliver.
	DispositionDLQ
)