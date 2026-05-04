package pubsub

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/fx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/trace"
)

// Subscribe registers a subscription against topic. Returns a
// nexus.Option intended to live inside a nexus.Module (or directly in
// nexus.Run's option list). The option resolves to an fx.Invoke that
// starts a long-running consumer once the app boots — same lifecycle
// shape as nexus.AsWorker.
//
// Handler is called once per delivered message. On nil return →
// Ack. On error return → Retry with exponential backoff up to
// cfg.MaxRetries, then DLQ. JSON-decode failures (poison payloads)
// skip retries and go straight to DLQ.
//
//	var module = nexus.Module("email",
//	    nexus.Provide(NewEmailService),
//	    pubsub.Subscribe(UserCreated, "send-welcome",
//	        func(ctx context.Context, e UserCreatedEvent) error {
//	            return mailer.Send(ctx, e.UserID, welcomeTemplate)
//	        },
//	        pubsub.SubscriptionConfig{MaxRetries: 5}),
//	)
//
// The subscription's worker name is `pubsub:<topic>:<subscription>`,
// which the dashboard surfaces in its Workers panel — operators see
// status (running / stopped / failed) and last error without any
// extra wiring.
func Subscribe[T any](topic *Topic[T], name string, handler func(ctx context.Context, payload T) error, cfg SubscriptionConfig) nexus.Option {
	if topic == nil {
		return failOption(fmt.Errorf("pubsub: Subscribe called with nil topic"))
	}
	if name == "" {
		return failOption(fmt.Errorf("pubsub: Subscribe(%q, ...) requires a non-empty subscription name", topic.Name()))
	}
	if handler == nil {
		return failOption(fmt.Errorf("pubsub: Subscribe(%q, %q) requires a non-nil handler", topic.Name(), name))
	}
	cfg = cfg.withDefaults()

	registerSubscription(subscriptionRecord{
		Topic:        topic.Name(),
		Name:         name,
		MaxRetries:   cfg.MaxRetries,
		AckDeadlinMs: cfg.AckDeadline.Milliseconds(),
	})

	workerName := "pubsub:" + topic.Name() + ":" + name
	return nexus.AsWorker(workerName, func(ctx context.Context, t Transport) error {
		return t.Consume(ctx, topic.Name(), name, ConsumeConfig{
			MaxRetries:  cfg.MaxRetries,
			AckDeadline: cfg.AckDeadline,
			BackoffMin:  cfg.BackoffMin,
			BackoffMax:  cfg.BackoffMax,
		}, func(msg Message) Disposition {
			return dispatch(ctx, topic, name, cfg, msg, handler)
		})
	})
}

// dispatch runs a single delivered message through the user's
// handler with the right ctx + trace shape. Extracted from Subscribe
// so tests can drive the dispatch path directly without booting an
// fx app.
//
// Behavior:
//   - JSON-decode failure → straight to DLQ (poison; retrying
//     against the same bytes is futile).
//   - Else: derive a per-message ctx that
//       (1) inherits the worker's bus (AsWorker stashed it),
//       (2) installs a remote-parent span when Attrs["traceparent"]
//           is a valid W3C header, so the consume span links into
//           the publisher's trace,
//       (3) bounds handler runtime by AckDeadline.
//   - Emit pubsub.consume:<topic>:<sub> span; End it with the
//     handler's error (nil = ack, error = retry).
func dispatch[T any](
	workerCtx context.Context,
	topic *Topic[T],
	subName string,
	cfg SubscriptionConfig,
	msg Message,
	handler func(context.Context, T) error,
) Disposition {
	payload, err := topic.decode(msg.Payload)
	if err != nil {
		return DispositionDLQ
	}
	msgCtx := workerCtx
	if tp := msg.Attrs["traceparent"]; tp != "" {
		if tid, pid, ok := trace.ParseTraceparent(tp); ok {
			msgCtx = trace.WithRemoteParent(msgCtx, tid, pid,
				"pubsub", topic.Name()+":"+subName)
		}
	}
	msgCtx, cancel := context.WithTimeout(msgCtx, cfg.AckDeadline)
	defer cancel()
	msgCtx, sp := trace.StartSpan(msgCtx, "pubsub.consume:"+topic.Name()+":"+subName,
		trace.Str("messaging.system", "nexus.pubsub"),
		trace.Str("messaging.destination", topic.Name()),
		trace.Str("messaging.subscription", subName),
		trace.Int("messaging.delivery_attempt", int64(msg.DeliveryAttempt)),
	)
	err = handler(msgCtx, payload)
	sp.End(err)
	if err != nil {
		return DispositionRetry
	}
	return DispositionAck
}

// SubscriptionConfig configures a subscription's retry + ack
// semantics. Zero values pick framework defaults — typical callers
// pass `pubsub.SubscriptionConfig{}` and only override when they
// need different behavior (e.g. high-cost handlers want fewer
// retries).
type SubscriptionConfig struct {
	// MaxRetries is the number of redeliveries the dispatcher
	// attempts before promoting to DLQ. Default: 3.
	MaxRetries int

	// AckDeadline bounds how long a single handler call may run
	// before the dispatcher cancels its context. The handler can
	// still take longer — Go doesn't preempt — but the cancellation
	// signals downstream calls to abort. Default: 30s.
	AckDeadline time.Duration

	// BackoffMin / BackoffMax are the exponential-backoff envelope
	// between retry attempts. Defaults: 100ms / 30s.
	BackoffMin time.Duration
	BackoffMax time.Duration
}

func (c SubscriptionConfig) withDefaults() SubscriptionConfig {
	if c.MaxRetries == 0 {
		c.MaxRetries = 3
	}
	if c.AckDeadline == 0 {
		c.AckDeadline = 30 * time.Second
	}
	if c.BackoffMin == 0 {
		c.BackoffMin = 100 * time.Millisecond
	}
	if c.BackoffMax == 0 {
		c.BackoffMax = 30 * time.Second
	}
	return c
}

// UseInMemory binds an InMemoryTransport to every registered topic.
// Returned option is intended for tests and `nexus dev` runs that
// don't have a broker available. Production code should use
// pubsub.UseRabbit (or similar) instead.
//
// Wiring sequence at boot:
//
//   1. fx constructs *InMemoryTransport (Provide).
//   2. fx Provide also exposes it as Transport (the interface
//      Subscribe's worker depends on).
//   3. An Invoke walks the topic registry and calls bindTransport
//      on each — so Topic[T].Publish has a live transport ref by
//      the time fx.Start unblocks.
//   4. fx.Stop closes the transport, which closes every queue and
//      lets blocked Consume goroutines exit.
func UseInMemory() nexus.Option {
	return nexus.Options(
		nexus.Provide(func(lc fx.Lifecycle) (Transport, *InMemoryTransport) {
			t := NewInMemoryTransport()
			lc.Append(fx.Hook{
				OnStop: func(_ context.Context) error { return t.Close() },
			})
			return t, t
		}),
		nexus.Invoke(bindTopics),
	)
}

// UseTransport is the escape hatch for adapters defined outside this
// package. The returned option provides the user-supplied transport
// to fx and runs bindTopics. The pubsub/rabbit subpackage uses this
// internally so adapter code doesn't need to duplicate the binding
// boilerplate.
//
// The transport's lifecycle is the caller's responsibility — they
// own the fx.Lifecycle hook for OnStart/OnStop.
func UseTransport(t Transport) nexus.Option {
	return nexus.Options(
		nexus.Supply(t),
		nexus.Invoke(bindTopics),
	)
}

// bindTopics is the boot-time fx.Invoke that walks the topic
// registry and points every Topic[T].publisher at the live
// transport. Called by UseInMemory / UseTransport so user code
// never has to write this plumbing.
//
// Also caches t as the registry's active transport so any topic
// registered after this point (lazy module init) gets bound on
// registration via register(). Without that cache, a topic
// declared in a package not imported until a later module load
// would Publish into the void.
//
// Idempotent: calling twice (e.g. test that toggles UseInMemory
// across subtests) replaces the binding atomically. The previous
// transport's Close is the caller's responsibility.
func bindTopics(t Transport) {
	regMu.Lock()
	activeTransport = t
	regMu.Unlock()
	for _, rec := range snapshotTopics() {
		rec.bindTransport(t)
	}
}

// failOption surfaces a constructor-time error through fx so a
// misuse like `pubsub.Subscribe(nil, ...)` halts boot at fx.New
// with a readable message instead of nil-pointer-panicking when
// the worker tries to consume.
func failOption(err error) nexus.Option {
	return nexus.Raw(fx.Error(err))
}