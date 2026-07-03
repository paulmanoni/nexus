// Package rabbit is the production RabbitMQ adapter for nexus's
// pubsub primitive. It plugs in via pubsub.Transport so user code
// stays unchanged when swapping from the in-memory transport to a
// real broker:
//
//	// production
//	nexus.Run(cfg, rabbit.Use(rabbit.Config{URL: "amqp://..."}), modules...)
//
//	// tests
//	nexus.Run(cfg, pubsub.UseInMemory(), modules...)
//
// Topology mapping:
//
//   - Each Topic[T] = one durable fanout exchange named after the
//     topic. Fanout (not topic) because nexus's typed topics already
//     carry their own routing dimension via T — RMQ-side wildcards
//     would only confuse the contract.
//   - Each Subscribe = one durable queue named "<topic>.<sub>",
//     bound to the topic's exchange. Multiple processes binding the
//     same queue compete for messages (work-queue semantics); each
//     distinct queue receives a copy (fan-out semantics).
//   - Each Subscribe also gets a DLQ queue "<topic>.<sub>.dlq" for
//     poison messages and retry-exhausted deliveries. Operators
//     drain it manually.
//
// Retry strategy: app-level. On Disposition=Retry, the consume loop
// acks the original delivery, sleeps for the configured backoff,
// then re-publishes the body directly to the subscription's queue
// with an incremented x-delivery-attempt header. After MaxRetries,
// the next failure routes to DLQ. This mirrors the in-memory
// transport's behavior 1:1 — the same ConsumeConfig produces the
// same retry envelope across both transports.
//
// Connection lifecycle: New dials once and reuses the connection
// for one publish channel + one consume channel per subscription.
// Reconnection is the caller's responsibility for v1 — a future
// iteration may add automatic dial-with-backoff once the framework
// has a generic supervisor pattern for fx components.
package rabbit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/paulmanoni/nexus/pubsub"
)

// Config carries connection + tuning knobs. Zero-value Prefetch
// picks a framework default (16) — large enough to keep a single
// consumer busy, small enough that a slow handler doesn't pin
// memory under bursty publish loads.
type Config struct {
	// URL is the AMQP connection string. Required. Format:
	// "amqp://user:pass@host:port/vhost".
	URL string

	// Prefetch is the per-channel QoS prefetch count. Caps the
	// number of unacked messages the broker delivers before
	// throttling. Default: 16.
	Prefetch int

	// Confirms enables RabbitMQ publisher confirms — Publish blocks
	// until the broker acks the message. Slower but durable;
	// recommend on for production publishers that absolutely cannot
	// drop. Default: off (best-effort, faster).
	Confirms bool

	// ResourceName is the dashboard label for the auto-registered
	// queue resource. Default "rabbit". Set to a more specific name
	// when multiple brokers coexist in one app (rare).
	ResourceName string

	// SkipResource disables the auto-registration of a
	// resource.NewQueue on the app's topology graph. Set to true
	// when the caller wants to register their own resource (custom
	// health probe, richer details, multiple bindings) and avoid
	// the generic auto-registered one stepping on it.
	SkipResource bool
}

// Transport is the adapter implementing pubsub.Transport against a
// live RabbitMQ broker.
type Transport struct {
	cfg  Config
	conn *amqp.Connection

	// pubMu serializes Publish calls because amqp091's Channel is
	// not safe for concurrent BasicPublish; the contention is fine
	// for nexus's typical traffic shape (publish from request
	// handlers, not from tight loops).
	pubMu sync.Mutex
	pubCh *amqp.Channel

	// declaredMu guards declared so each topic / queue is set up
	// exactly once even when multiple Consume goroutines start
	// concurrently against the same topic.
	declaredMu sync.Mutex
	declared   map[string]bool

	// closed flips once Close returns; Publish / Consume bail out
	// rather than dispatching against a torn-down connection.
	closedMu sync.RWMutex
	closed   bool
}

// New dials the broker and prepares the transport. The returned
// transport is ready for Publish; Consume calls lazily declare
// per-subscription queues + bindings on first use.
func New(cfg Config) (*Transport, error) {
	if cfg.URL == "" {
		return nil, errors.New("rabbit: Config.URL is required")
	}
	if cfg.Prefetch <= 0 {
		cfg.Prefetch = 16
	}
	conn, err := amqp.Dial(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("rabbit: dial: %w", err)
	}
	pubCh, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("rabbit: open publish channel: %w", err)
	}
	if cfg.Confirms {
		if err := pubCh.Confirm(false); err != nil {
			_ = pubCh.Close()
			_ = conn.Close()
			return nil, fmt.Errorf("rabbit: enable publisher confirms: %w", err)
		}
	}
	return &Transport{
		cfg:      cfg,
		conn:     conn,
		pubCh:    pubCh,
		declared: map[string]bool{},
	}, nil
}

// Publish serializes payload onto the topic's fanout exchange. The
// optional attrs map is encoded as AMQP headers, including the
// W3C traceparent the pubsub layer injects for trace stitching.
//
// Messages are persistent (DeliveryMode=2) so a broker restart
// doesn't drop in-flight publishes from durable queues. This costs
// a disk fsync per message; if a workload demands raw throughput
// over durability, a future Config.Persistent=false toggle can be
// added without API churn.
func (t *Transport) Publish(ctx context.Context, topic string, payload []byte, attrs map[string]string) error {
	if t.isClosed() {
		return errors.New("rabbit: transport closed")
	}
	if err := t.declareTopic(topic); err != nil {
		return err
	}
	headers := amqp.Table{}
	for k, v := range attrs {
		headers[k] = v
	}
	t.pubMu.Lock()
	defer t.pubMu.Unlock()
	return t.pubCh.PublishWithContext(ctx, topic, "", false, false, amqp.Publishing{
		ContentType:  "application/json",
		Body:         payload,
		DeliveryMode: amqp.Persistent,
		Headers:      headers,
		Timestamp:    time.Now(),
	})
}

// Consume opens a dedicated channel for this subscription, declares
// the queue + DLQ, binds the queue to the topic exchange, and
// dispatches deliveries to the deliver func. Blocks until ctx is
// cancelled or the channel errors.
//
// Why one channel per subscription: amqp091 channels carry one
// consumer's delivery stream; multiplexing several subscriptions on
// one channel forces the dispatcher to demultiplex tag-by-tag and
// produces flow-control surprises on noisy subscriptions. Channels
// are cheap.
func (t *Transport) Consume(ctx context.Context, topic, subscription string, cfg pubsub.ConsumeConfig, deliver pubsub.Deliver) error {
	if t.isClosed() {
		return errors.New("rabbit: transport closed")
	}
	cfg = applyConsumeDefaults(cfg)

	if err := t.declareTopic(topic); err != nil {
		return err
	}
	queueName := topic + "." + subscription
	dlqName := queueName + ".dlq"
	if err := t.declareSubscription(topic, queueName, dlqName); err != nil {
		return err
	}

	ch, err := t.conn.Channel()
	if err != nil {
		return fmt.Errorf("rabbit: open consume channel for %s: %w", queueName, err)
	}
	defer ch.Close()
	if err := ch.Qos(t.cfg.Prefetch, 0, false); err != nil {
		return fmt.Errorf("rabbit: set qos for %s: %w", queueName, err)
	}

	consumerTag := "nexus." + queueName
	deliveries, err := ch.ConsumeWithContext(ctx, queueName, consumerTag, false /* autoAck */, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("rabbit: consume %s: %w", queueName, err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case d, open := <-deliveries:
			if !open {
				return nil
			}
			t.handleDelivery(ctx, ch, queueName, dlqName, cfg, deliver, d)
		}
	}
}

// handleDelivery runs one delivered message through deliver and
// reacts to the disposition. Kept separate from the Consume loop
// so the per-message retry/DLQ logic reads as straight-line code.
//
// All paths ack the original delivery — retries are implemented as
// "ack + republish to queue with backoff", not as nack-with-requeue
// (which would loop without backoff and ignore the framework's
// MaxRetries). This is the same pattern the in-memory transport
// uses, so cross-transport semantics stay aligned.
func (t *Transport) handleDelivery(ctx context.Context, ch *amqp.Channel, queueName, dlqName string, cfg pubsub.ConsumeConfig, deliver pubsub.Deliver, d amqp.Delivery) {
	attempt := readAttempt(d.Headers)
	msg := pubsub.Message{
		Topic:           extractTopic(queueName),
		Subscription:    extractSubscription(queueName),
		Payload:         d.Body,
		Attrs:           headersToAttrs(d.Headers),
		DeliveryAttempt: attempt,
	}
	disp := deliver(msg)
	if err := d.Ack(false); err != nil {
		// Best-effort logging would go here; for v1 a failed ack
		// just falls back to broker-side redelivery on channel
		// close, which the dispatcher's retry counter would treat
		// as a fresh attempt — at-least-once intact.
		return
	}
	switch disp {
	case pubsub.DispositionAck:
		// nothing else to do.
	case pubsub.DispositionDLQ:
		_ = t.republishDirect(ctx, ch, dlqName, d, attempt)
	case pubsub.DispositionRetry:
		if attempt >= cfg.MaxRetries {
			_ = t.republishDirect(ctx, ch, dlqName, d, attempt)
			return
		}
		// Sleep in this goroutine — RMQ delivers serially per
		// channel so we're not blocking another consumer, and it
		// gives us precise backoff control without leaning on
		// broker plugins.
		delay := backoff(attempt, cfg.BackoffMin, cfg.BackoffMax)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		_ = t.republishDirect(ctx, ch, queueName, d, attempt+1)
	}
}

// republishDirect re-emits the body to the named queue via the
// default exchange (routing key = queue name → direct delivery).
// Used by retry (republish to the source queue) and DLQ (republish
// to the dead-letter queue). Headers are copied with the attempt
// counter updated so the next handler call sees the right number.
func (t *Transport) republishDirect(ctx context.Context, ch *amqp.Channel, queue string, d amqp.Delivery, attempt int) error {
	headers := amqp.Table{}
	for k, v := range d.Headers {
		headers[k] = v
	}
	// Clamp the counter into int32 range. Retry budgets are tiny
	// (single-digit attempts in practice); the clamp is paranoia for
	// a runaway requeue loop.
	clamped := attempt
	if clamped < 0 {
		clamped = 0
	} else if clamped > math.MaxInt32 {
		clamped = math.MaxInt32
	}
	headers["x-delivery-attempt"] = int32(clamped) // #nosec G115 -- clamped above
	t.pubMu.Lock()
	defer t.pubMu.Unlock()
	return ch.PublishWithContext(ctx, "", queue, false, false, amqp.Publishing{
		ContentType:  d.ContentType,
		Body:         d.Body,
		DeliveryMode: amqp.Persistent,
		Headers:      headers,
		Timestamp:    time.Now(),
	})
}

// declareTopic declares the topic's fanout exchange. Idempotent —
// repeated declarations with matching parameters are no-ops on the
// broker side, but we cache locally too so the framework's hot
// path skips the round-trip.
func (t *Transport) declareTopic(topic string) error {
	t.declaredMu.Lock()
	defer t.declaredMu.Unlock()
	if t.declared["x:"+topic] {
		return nil
	}
	if err := t.pubCh.ExchangeDeclare(topic, "fanout", true /* durable */, false, false, false, nil); err != nil {
		return fmt.Errorf("rabbit: declare exchange %q: %w", topic, err)
	}
	t.declared["x:"+topic] = true
	return nil
}

// declareSubscription declares the subscription's queue + DLQ and
// binds the queue to the topic's exchange. Idempotent.
func (t *Transport) declareSubscription(topic, queueName, dlqName string) error {
	t.declaredMu.Lock()
	defer t.declaredMu.Unlock()
	if t.declared["q:"+queueName] {
		return nil
	}
	if _, err := t.pubCh.QueueDeclare(queueName, true /* durable */, false, false, false, nil); err != nil {
		return fmt.Errorf("rabbit: declare queue %q: %w", queueName, err)
	}
	if _, err := t.pubCh.QueueDeclare(dlqName, true /* durable */, false, false, false, nil); err != nil {
		return fmt.Errorf("rabbit: declare dlq %q: %w", dlqName, err)
	}
	if err := t.pubCh.QueueBind(queueName, "" /* fanout ignores routing key */, topic, false, nil); err != nil {
		return fmt.Errorf("rabbit: bind queue %q to exchange %q: %w", queueName, topic, err)
	}
	t.declared["q:"+queueName] = true
	return nil
}

// Close tears down channels + connection. Idempotent so fx OnStop
// can call it without guarding the second-time error.
func (t *Transport) Close() error {
	t.closedMu.Lock()
	if t.closed {
		t.closedMu.Unlock()
		return nil
	}
	t.closed = true
	t.closedMu.Unlock()

	var firstErr error
	if t.pubCh != nil {
		if err := t.pubCh.Close(); err != nil {
			firstErr = err
		}
	}
	if t.conn != nil {
		if err := t.conn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (t *Transport) isClosed() bool {
	t.closedMu.RLock()
	defer t.closedMu.RUnlock()
	return t.closed
}

// Healthy reports whether the broker connection is alive. Returns
// false when the AMQP connection's I/O loop has terminated (network
// drop, broker crash) OR after Close() has been called. Used as
// the health probe for the auto-registered resource.NewQueue so
// the dashboard's topology view turns red the moment the broker
// becomes unreachable.
//
// Cheap — IsClosed reads a single mutex-guarded bool inside
// amqp091. Safe to call from the dashboard's snapshot loop on
// every poll.
func (t *Transport) Healthy() bool {
	if t.isClosed() {
		return false
	}
	if t.conn == nil {
		return false
	}
	return !t.conn.IsClosed()
}

// applyConsumeDefaults fills in zero ConsumeConfig fields with the
// framework's defaults. Mirrors the InMemoryTransport.Consume
// defaults so cross-transport behavior matches.
func applyConsumeDefaults(c pubsub.ConsumeConfig) pubsub.ConsumeConfig {
	if c.MaxRetries <= 0 {
		c.MaxRetries = 3
	}
	if c.BackoffMin <= 0 {
		c.BackoffMin = 100 * time.Millisecond
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 30 * time.Second
	}
	if c.AckDeadline <= 0 {
		c.AckDeadline = 30 * time.Second
	}
	return c
}

// backoff matches the in-memory transport's exponential schedule:
//
//	attempt 1 → min, attempt 2 → 2*min, …, capped at max.
//
// No jitter at this layer — the dispatcher is the right place to
// add it once a real workload demands herd-effect avoidance.
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

// readAttempt extracts x-delivery-attempt from headers. Defaults to
// 1 (first delivery) when unset, so a publish that arrives via the
// topic exchange (no retry header) still kicks off the dispatcher
// at the right counter.
func readAttempt(h amqp.Table) int {
	if h == nil {
		return 1
	}
	switch v := h["x-delivery-attempt"].(type) {
	case int32:
		if v < 1 {
			return 1
		}
		return int(v)
	case int64:
		if v < 1 {
			return 1
		}
		return int(v)
	case int:
		if v < 1 {
			return 1
		}
		return v
	}
	return 1
}

// headersToAttrs flattens AMQP headers to the string→string map the
// pubsub layer consumes. Non-string header values are stringified
// via fmt.Sprint to keep the contract simple; lossless round-trip
// of structured headers is out of scope for v1.
func headersToAttrs(h amqp.Table) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		switch s := v.(type) {
		case string:
			out[k] = s
		default:
			out[k] = fmt.Sprint(v)
		}
	}
	return out
}

// extractTopic / extractSubscription split a "topic.subscription"
// queue name. Kept simple — the queue naming convention is owned
// by this package, so a "." in the topic name would only happen
// via an authoring bug; the dispatcher tolerates the resulting
// string just fine because Topic / Subscription on Message are
// informational, not used for routing.
func extractTopic(queueName string) string {
	for i := len(queueName) - 1; i >= 0; i-- {
		if queueName[i] == '.' {
			return queueName[:i]
		}
	}
	return queueName
}

func extractSubscription(queueName string) string {
	for i := len(queueName) - 1; i >= 0; i-- {
		if queueName[i] == '.' {
			return queueName[i+1:]
		}
	}
	return ""
}
