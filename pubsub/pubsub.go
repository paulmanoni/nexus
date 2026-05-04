// Package pubsub is nexus's typed pub/sub primitive. Topics are
// declared as package-level variables — the same Encore-style shape
// as nexus.Cron and (proposed) nexus.Secret — so a publisher can call
// `topic.Publish(ctx, payload)` from anywhere without needing the
// transport injected.
//
// The package is transport-agnostic. Topics register into a process-
// global registry at package-init time; an option chosen at app boot
// (pubsub.UseInMemory for tests, pubsub.UseRabbit for production)
// binds the live transport to every registered topic. Subscribers are
// declared via pubsub.Subscribe(...) inside a nexus.Module — the
// option wraps nexus.AsWorker so subscriptions inherit the same
// lifecycle, panic recovery, and dashboard/registry surfacing as any
// other long-running worker.
//
// Default semantics:
//
//   - At-least-once delivery. Handler returns nil → Ack; returns error
//     → Retry with exponential backoff up to MaxRetries (default 3),
//     then DLQ. JSON-decode failures go straight to DLQ as poison.
//   - Per-topic isolation. Each Topic[T] maps to its own RabbitMQ
//     exchange (or in-memory queue); subscriptions do not cross
//     between topics.
//   - JSON codec. The payload type T is serialized via encoding/json.
//     A future Codec[T] hook can switch to protobuf/msgpack without
//     changing the API.
package pubsub

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/paulmanoni/nexus/trace"
)

// TopicConfig is the per-topic knobs. Empty values pick framework
// defaults at boot, so callers can pass the zero value for the common
// case (`pubsub.NewTopic[T]("name", pubsub.TopicConfig{})`).
type TopicConfig struct {
	// Description shows up in the manifest + dashboard so operators
	// know what the topic carries without reading source.
	Description string

	// Durable, when true, instructs the transport to persist messages
	// across broker restarts. RabbitMQ: durable exchange + persistent
	// publish. In-memory: ignored (everything is in-process anyway).
	Durable bool
}

// Topic[T] is a typed pub/sub channel. Constructed at package-init
// via NewTopic; Publish is safe to call from any goroutine after the
// app has booted.
//
// The zero value is not usable — callers must go through NewTopic so
// the registry has a record. Calling Publish on a zero Topic returns
// an explicit error rather than panicking, so a misconfigured app
// fails loudly at the first publish rather than silently dropping.
type Topic[T any] struct {
	name string
	cfg  TopicConfig

	// publisher is set by bindTopics during fx.Start, after the user's
	// chosen Use* option has provided a Transport. Held as an atomic
	// pointer so Publish is lock-free in the hot path.
	publisher atomic.Pointer[publisherBinding]

	// payloadType is reflect.TypeOf((*T)(nil)).Elem() — captured once
	// for the manifest's TopicSummary.PayloadType field. Cheap; doing
	// it eagerly avoids a generic-method-on-interface dance later.
	payloadType reflect.Type
}

// publisherBinding wraps the active Transport; a separate struct lets
// us swap it atomically (e.g. test harness calling UseInMemory twice
// across subtests would otherwise race on a bare interface field).
type publisherBinding struct {
	t Transport
}

// NewTopic registers a new topic by name and returns a typed handle.
// Call this at package level so the topic exists in the registry by
// the time nexus.Run walks it:
//
//	var UserCreated = pubsub.NewTopic[UserCreatedEvent]("user-created",
//	    pubsub.TopicConfig{Description: "Emitted when a user signs up"})
//
// Duplicate names panic at init — earlier the loud failure, the
// less surprising the eventual production outage. The registry is
// process-global because topic identity is the *name*, not any
// per-app instance: two modules referring to "user-created" must
// produce the same Topic[T] value.
func NewTopic[T any](name string, cfg TopicConfig) *Topic[T] {
	if name == "" {
		panic("pubsub: NewTopic requires a non-empty name")
	}
	t := &Topic[T]{
		name:        name,
		cfg:         cfg,
		payloadType: reflect.TypeOf((*T)(nil)).Elem(),
	}
	register(t)
	return t
}

// Name is the registered topic name (the same string passed to
// NewTopic). Stable across the lifetime of the process.
func (t *Topic[T]) Name() string { return t.name }

// Publish encodes payload and hands it to the active transport.
// Returns an error if no transport has been bound — typically because
// the app forgot to include pubsub.UseInMemory() or pubsub.UseRabbit()
// in its option chain. The error names the topic so the operator can
// trace the missing wiring without grepping.
//
// The function is goroutine-safe and does not allocate beyond the
// codec's own buffer. Hot-path callers (fan-out from a single
// request) are expected to call this directly without batching;
// transport-level batching belongs inside the Transport, not here.
//
// Trace integration: when ctx carries a trace span (typical inside
// a request handler), Publish emits a `pubsub.publish:<topic>` span
// so the publish appears as a bar on the dashboard's trace
// waterfall. The current span's W3C traceparent is also injected
// into Message.Attrs so downstream consumers can stitch their own
// spans into the publisher's trace once consumer-side trace
// propagation lands.
func (t *Topic[T]) Publish(ctx context.Context, payload T) error {
	bind := t.publisher.Load()
	if bind == nil || bind.t == nil {
		return fmt.Errorf("pubsub: topic %q has no transport bound — add pubsub.UseInMemory() or pubsub.UseRabbit(...) to your nexus.Run options", t.name)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("pubsub: topic %q encode: %w", t.name, err)
	}

	attrs := traceAttrs(ctx)

	// Span emission depends on a bus being threaded through ctx by
	// upstream middleware. When publish happens outside a request
	// (a cron tick, a worker), there's no bus — StartSpan still
	// returns a Span but it publishes nothing, so this is free.
	ctx, sp := trace.StartSpan(ctx, "pubsub.publish:"+t.name,
		trace.Str("messaging.system", "nexus.pubsub"),
		trace.Str("messaging.destination", t.name),
		trace.Int("messaging.payload_bytes", int64(len(body))),
	)
	err = bind.t.Publish(ctx, t.name, body, attrs)
	sp.End(err)
	return err
}

// traceAttrs returns a fresh attrs map carrying the W3C traceparent
// derived from ctx's current span, or nil when no span is in
// context. Adapters propagate the map onto the wire (RabbitMQ
// headers, in-memory Message.Attrs) so consumers see it.
func traceAttrs(ctx context.Context) map[string]string {
	span, ok := trace.SpanFromCtx(ctx)
	if !ok || span == nil {
		return nil
	}
	if len(span.TraceID) != 32 || len(span.SpanID) != 16 {
		return nil
	}
	return map[string]string{
		"traceparent": "00-" + span.TraceID + "-" + span.SpanID + "-01",
	}
}

// decode is used by Subscribe handlers to turn a delivered byte
// slice back into T. JSON-decode failures are surfaced separately
// from handler errors so the dispatcher can route them straight to
// DLQ as poison messages instead of retrying.
func (t *Topic[T]) decode(body []byte) (T, error) {
	var v T
	err := json.Unmarshal(body, &v)
	return v, err
}

// ── Registry ───────────────────────────────────────────────────────
//
// Process-global. Topics register at init time; the registry is
// walked at fx.Start by the chosen Use* option. Subscriptions are
// kept here too so the manifest provider can list them without
// knowing about the active fx graph.

type topicRecord interface {
	Name() string
	Description() string
	Durable() bool
	PayloadType() reflect.Type
	bindTransport(t Transport)
}

func (t *Topic[T]) Description() string         { return t.cfg.Description }
func (t *Topic[T]) Durable() bool               { return t.cfg.Durable }
func (t *Topic[T]) PayloadType() reflect.Type   { return t.payloadType }
func (t *Topic[T]) bindTransport(tr Transport)  { t.publisher.Store(&publisherBinding{t: tr}) }

type subscriptionRecord struct {
	Topic        string
	Name         string
	MaxRetries   int
	AckDeadlinMs int64
	Module       string
}

var (
	regMu     sync.RWMutex
	regTopics = map[string]topicRecord{}
	regSubs   []subscriptionRecord

	// activeTransport caches the transport currently bound by
	// UseInMemory / UseTransport so a topic declared *after* boot
	// (lazy module load, late-init package) still gets a working
	// publisher. Without this cache, late topics would Publish into
	// the void with "no transport bound", which is silently
	// confusing — the boot-time bindTopics walked the registry but
	// the new topic wasn't there yet to bind.
	activeTransport Transport
)

func register(t topicRecord) {
	regMu.Lock()
	defer regMu.Unlock()
	if existing, ok := regTopics[t.Name()]; ok {
		// Same package re-imported is impossible; the only way to hit
		// this is a real duplicate declaration, which is an authoring
		// bug worth halting boot for.
		panic(fmt.Sprintf("pubsub: topic %q declared twice (existing payload=%s, new payload=%s)",
			t.Name(), existing.PayloadType(), t.PayloadType()))
	}
	regTopics[t.Name()] = t
	// If an app has already booted and bound a transport, propagate
	// it to this late-registered topic so its Publish works on first
	// call. New apps booting after this point will rebind everything
	// — no harm in the redundant assignment.
	if activeTransport != nil {
		t.bindTransport(activeTransport)
	}
}

func registerSubscription(s subscriptionRecord) {
	regMu.Lock()
	regSubs = append(regSubs, s)
	regMu.Unlock()
}

// snapshotTopics returns the registered topics sorted by name.
// Used by bindTopics and by the manifest provider; the lock is held
// only for the copy so callers operate on a stable slice without
// blocking new declarations (which can't happen post-init anyway,
// but defensive copying is cheap).
func snapshotTopics() []topicRecord {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]topicRecord, 0, len(regTopics))
	for _, t := range regTopics {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

func snapshotSubscriptions() []subscriptionRecord {
	regMu.RLock()
	defer regMu.RUnlock()
	out := append([]subscriptionRecord(nil), regSubs...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Topic != out[j].Topic {
			return out[i].Topic < out[j].Topic
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// resetForTest clears the registry. EXPORTED ONLY in _test.go via a
// matching wrapper; the unexported form keeps the production package
// from advertising a footgun. Tests need this because topics are
// package-level globals — without a reset, one test's NewTopic
// pollutes the next test's registry.
func resetForTest() {
	regMu.Lock()
	regTopics = map[string]topicRecord{}
	regSubs = nil
	activeTransport = nil
	regMu.Unlock()
}