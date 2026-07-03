package pubsub

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/trace"
)

// encodeForTest mirrors what Topic.Publish does internally so tests
// can craft Messages with valid bytes. Kept tiny; if the codec ever
// becomes pluggable, swap this to call the topic's codec.
func encodeForTest(v any) ([]byte, error) { return json.Marshal(v) }

// traceCtxWithSpan returns a ctx carrying a freshly-started span
// (32-hex traceID, 16-hex spanID — the shape Publish needs to
// produce a valid W3C traceparent). No bus is attached, so
// StartSpan on this ctx publishes nothing — the test only cares
// about the ID shape.
func traceCtxWithSpan() (context.Context, *trace.Span) {
	return trace.StartSpan(context.Background(), "test.parent")
}

// ResetForTest is the test-only escape hatch for the package-global
// registry. Production code never has reason to clear topics; tests
// need it because every NewTopic at package init pollutes subsequent
// tests' registry view.
func ResetForTest() { resetForTest() }

// runConsumer starts a Consume loop on the given transport in a
// goroutine and waits until the subscription's queue is registered
// before returning. Without the wait the test can race: Publish
// fans out only to queues that exist at publish time, so a test
// that publishes before Consume has reached its registration
// section silently drops the message.
//
// Tests use this instead of going through nexus.AsWorker so they
// can drive the dispatcher directly without booting an fx app.
func runConsumer(t *testing.T, tr *InMemoryTransport, topic, sub string, cfg ConsumeConfig, deliver Deliver) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tr.Consume(ctx, topic, sub, cfg, deliver)
	}()
	waitFor(t, time.Second, func() bool {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		for _, name := range tr.subsByTopic[topic] {
			if name == sub {
				return true
			}
		}
		return false
	})
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("consumer %s/%s did not stop within 2s", topic, sub)
		}
	}
}

// waitFor polls fn until it returns true or the deadline elapses.
// Test asserts use this rather than time.Sleep so a slow CI box
// doesn't flake — and a deadlock surfaces as a clear timeout, not
// a passing test that should have observed the side effect.
func waitFor(t *testing.T, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("waitFor: condition not met within %s", d)
}

type testEvent struct {
	ID  int    `json:"id"`
	Msg string `json:"msg"`
}

func TestInMemory_Roundtrip(t *testing.T) {
	ResetForTest()
	topic := NewTopic[testEvent]("rt", TopicConfig{})
	tr := NewInMemoryTransport()
	t.Cleanup(func() { _ = tr.Close() })
	BindTopics(tr)

	var got testEvent
	var seen int32
	stop := runConsumer(t, tr, "rt", "consumer-1",
		ConsumeConfig{MaxRetries: 3, AckDeadline: time.Second, BackoffMin: 5 * time.Millisecond, BackoffMax: 50 * time.Millisecond},
		func(msg Message) Disposition {
			payload, err := topic.decode(msg.Payload)
			if err != nil {
				return DispositionDLQ
			}
			got = payload
			atomic.AddInt32(&seen, 1)
			return DispositionAck
		})
	defer stop()

	if err := topic.Publish(context.Background(), testEvent{ID: 7, Msg: "hi"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&seen) == 1 })

	if got.ID != 7 || got.Msg != "hi" {
		t.Errorf("payload not delivered: got %+v", got)
	}
}

func TestInMemory_FanOut(t *testing.T) {
	ResetForTest()
	topic := NewTopic[testEvent]("fanout", TopicConfig{})
	tr := NewInMemoryTransport()
	t.Cleanup(func() { _ = tr.Close() })
	BindTopics(tr)

	var seenA, seenB int32
	stopA := runConsumer(t, tr, "fanout", "sub-a",
		ConsumeConfig{MaxRetries: 1, AckDeadline: time.Second},
		func(msg Message) Disposition { atomic.AddInt32(&seenA, 1); return DispositionAck })
	defer stopA()
	stopB := runConsumer(t, tr, "fanout", "sub-b",
		ConsumeConfig{MaxRetries: 1, AckDeadline: time.Second},
		func(msg Message) Disposition { atomic.AddInt32(&seenB, 1); return DispositionAck })
	defer stopB()

	// Both subscriptions must register before we publish, otherwise
	// the fan-out only reaches whichever queues exist at publish time.
	waitFor(t, time.Second, func() bool {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		return len(tr.subsByTopic["fanout"]) == 2
	})

	if err := topic.Publish(context.Background(), testEvent{ID: 1}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	waitFor(t, time.Second, func() bool {
		return atomic.LoadInt32(&seenA) == 1 && atomic.LoadInt32(&seenB) == 1
	})
}

func TestInMemory_RetryThenSucceed(t *testing.T) {
	ResetForTest()
	topic := NewTopic[testEvent]("retry-ok", TopicConfig{})
	tr := NewInMemoryTransport()
	t.Cleanup(func() { _ = tr.Close() })
	BindTopics(tr)

	var attempts int32
	stop := runConsumer(t, tr, "retry-ok", "sub",
		ConsumeConfig{MaxRetries: 5, AckDeadline: time.Second, BackoffMin: time.Millisecond, BackoffMax: 10 * time.Millisecond},
		func(msg Message) Disposition {
			n := atomic.AddInt32(&attempts, 1)
			if n < 3 {
				return DispositionRetry
			}
			return DispositionAck
		})
	defer stop()

	if err := topic.Publish(context.Background(), testEvent{ID: 1}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&attempts) >= 3 })

	if dlq := tr.DLQ("retry-ok", "sub"); len(dlq) != 0 {
		t.Errorf("DLQ should be empty after eventual success, got %d", len(dlq))
	}
}

func TestInMemory_RetryExhaustedToDLQ(t *testing.T) {
	ResetForTest()
	topic := NewTopic[testEvent]("retry-bad", TopicConfig{})
	tr := NewInMemoryTransport()
	t.Cleanup(func() { _ = tr.Close() })
	BindTopics(tr)

	stop := runConsumer(t, tr, "retry-bad", "sub",
		ConsumeConfig{MaxRetries: 3, AckDeadline: time.Second, BackoffMin: time.Millisecond, BackoffMax: 5 * time.Millisecond},
		func(msg Message) Disposition { return DispositionRetry })
	defer stop()

	if err := topic.Publish(context.Background(), testEvent{ID: 1}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool { return len(tr.DLQ("retry-bad", "sub")) == 1 })
	dlq := tr.DLQ("retry-bad", "sub")
	if dlq[0].DeliveryAttempt != 3 {
		t.Errorf("DLQ message DeliveryAttempt: want 3, got %d", dlq[0].DeliveryAttempt)
	}
}

func TestInMemory_PoisonGoesStraightToDLQ(t *testing.T) {
	// Subscribe is the layer that turns decode failures into DLQ.
	// Drive it through a synthetic Deliver that mimics what Subscribe's
	// closure does, so this test pins the contract independent of the
	// generic wrapper.
	ResetForTest()
	topic := NewTopic[testEvent]("poison", TopicConfig{})
	tr := NewInMemoryTransport()
	t.Cleanup(func() { _ = tr.Close() })
	BindTopics(tr)

	stop := runConsumer(t, tr, "poison", "sub",
		ConsumeConfig{MaxRetries: 5, AckDeadline: time.Second},
		func(msg Message) Disposition {
			if _, err := topic.decode(msg.Payload); err != nil {
				return DispositionDLQ
			}
			return DispositionAck
		})
	defer stop()

	// Inject directly into the queue with malformed JSON, bypassing
	// Topic.Publish (which would never produce malformed bytes).
	if err := tr.Publish(context.Background(), "poison", []byte("not json"), nil); err != nil {
		t.Fatalf("raw Publish: %v", err)
	}

	waitFor(t, time.Second, func() bool { return len(tr.DLQ("poison", "sub")) == 1 })

	// And no retries — first delivery is the only one.
	if dlq := tr.DLQ("poison", "sub"); dlq[0].DeliveryAttempt != 1 {
		t.Errorf("poison should DLQ on first delivery, got attempt=%d", dlq[0].DeliveryAttempt)
	}
}

func TestPublish_WithoutTransport(t *testing.T) {
	ResetForTest()
	topic := NewTopic[testEvent]("no-transport", TopicConfig{})
	err := topic.Publish(context.Background(), testEvent{ID: 1})
	if err == nil {
		t.Fatal("expected Publish to fail when no transport is bound")
	}
	// The error should name the topic so the operator knows what's misconfigured.
	if !contains(err.Error(), "no-transport") {
		t.Errorf("error should mention topic name; got: %v", err)
	}
}

func TestNewTopic_DuplicatePanics(t *testing.T) {
	ResetForTest()
	NewTopic[testEvent]("dup", TopicConfig{})
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate NewTopic")
		}
	}()
	NewTopic[testEvent]("dup", TopicConfig{})
}

func TestTopicsSnapshot(t *testing.T) {
	ResetForTest()
	NewTopic[testEvent]("z", TopicConfig{Description: "zee"})
	NewTopic[testEvent]("a", TopicConfig{Durable: true})

	got := Topics()
	if len(got) != 2 {
		t.Fatalf("want 2 topics, got %d", len(got))
	}
	// Sorted by name → "a" first.
	if got[0].Name != "a" || !got[0].Durable {
		t.Errorf("topic[0]: want a/durable, got %+v", got[0])
	}
	if got[1].Name != "z" || got[1].Description != "zee" {
		t.Errorf("topic[1]: want z/zee, got %+v", got[1])
	}
}

func TestClose_Idempotent(t *testing.T) {
	tr := NewInMemoryTransport()
	if err := tr.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	// Publish to a closed transport must error, not panic.
	if err := tr.Publish(context.Background(), "any", []byte("x"), nil); err == nil {
		t.Error("Publish on closed transport should fail")
	}
}

func TestConsume_StopsOnContextCancel(t *testing.T) {
	tr := NewInMemoryTransport()
	t.Cleanup(func() { _ = tr.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	consumed := errors.New("not yet")
	go func() {
		defer wg.Done()
		err := tr.Consume(ctx, "x", "s", ConsumeConfig{MaxRetries: 1, AckDeadline: time.Second},
			func(msg Message) Disposition { return DispositionAck })
		consumed = err
	}()
	// Give the consumer a moment to register its queue.
	waitFor(t, time.Second, func() bool {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		return len(tr.subsByTopic["x"]) == 1
	})
	cancel()
	wg.Wait()
	if consumed != nil {
		t.Errorf("Consume should return nil on cancel, got %v", consumed)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestLateBind covers the case where a topic is registered AFTER
// UseInMemory has run. Without the registry caching the active
// transport, late-registered topics would Publish into the void.
func TestLateBind(t *testing.T) {
	ResetForTest()
	tr := NewInMemoryTransport()
	t.Cleanup(func() { _ = tr.Close() })

	// Bind the transport BEFORE any topic exists — simulates the
	// boot order where pubsub.UseInMemory's Invoke fires before a
	// lazy module registers its topic.
	BindTopics(tr)

	topic := NewTopic[testEvent]("late", TopicConfig{})
	stop := runConsumer(t, tr, "late", "sub",
		ConsumeConfig{MaxRetries: 1, AckDeadline: time.Second},
		func(msg Message) Disposition { return DispositionAck })
	defer stop()

	if err := topic.Publish(context.Background(), testEvent{ID: 1}); err != nil {
		t.Fatalf("late-registered topic should still Publish; got: %v", err)
	}
}

// TestPublish_InjectsTraceparent ensures the W3C traceparent
// derived from ctx's span lands in Message.Attrs so downstream
// trace stitching can read it.
func TestPublish_InjectsTraceparent(t *testing.T) {
	ResetForTest()
	topic := NewTopic[testEvent]("trace-prop", TopicConfig{})
	tr := NewInMemoryTransport()
	t.Cleanup(func() { _ = tr.Close() })
	BindTopics(tr)

	var got Message
	var seen int32
	stop := runConsumer(t, tr, "trace-prop", "sub",
		ConsumeConfig{MaxRetries: 1, AckDeadline: time.Second},
		func(msg Message) Disposition {
			got = msg
			atomic.AddInt32(&seen, 1)
			return DispositionAck
		})
	defer stop()

	// Build a ctx carrying a span. The trace package's StartSpan
	// emits to a bus only when one is in ctx; for this test we don't
	// need actual events, only that SpanFromCtx returns a span with
	// well-formed IDs so traceAttrs picks them up.
	ctx, _ := traceCtxWithSpan()

	if err := topic.Publish(ctx, testEvent{ID: 1}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&seen) == 1 })

	tp := got.Attrs["traceparent"]
	if tp == "" {
		t.Fatalf("Publish should have set traceparent in Attrs; got Attrs=%v", got.Attrs)
	}
	// Shape: "00-<32hex>-<16hex>-01"
	if !contains(tp, "00-") || len(tp) != 55 {
		t.Errorf("traceparent malformed: %q", tp)
	}
}

// TestDispatch_StitchesTraceFromAttrs proves the consume-side
// stitching: when the message carries a traceparent (set by Publish
// when called from a request handler), the handler's ctx carries a
// span whose TraceID matches the publisher's TraceID, and whose
// ParentID matches the publisher's SpanID. The dashboard's trace
// waterfall renders the chain as one trace.
func TestDispatch_StitchesTraceFromAttrs(t *testing.T) {
	ResetForTest()
	topic := NewTopic[testEvent]("stitch", TopicConfig{})

	// Publisher side: ctx with bus + a started span.
	bus := trace.NewBus(100)
	pubCtx := trace.WithBus(context.Background(), bus)
	pubCtx, pubSpan := trace.StartSpan(pubCtx, "publisher.request")
	defer pubSpan.End(nil)

	// Synthesize a message the way Publish would: JSON-encoded payload
	// + traceparent in Attrs derived from pubCtx's span.
	body, err := encodeForTest(testEvent{ID: 9, Msg: "stitched"})
	if err != nil {
		t.Fatal(err)
	}
	msg := Message{
		Topic:           "stitch",
		Subscription:    "sub",
		Payload:         body,
		Attrs:           traceAttrs(pubCtx),
		DeliveryAttempt: 1,
	}

	// Capture the ctx the handler sees.
	var handlerCtx context.Context
	disp := dispatch(
		trace.WithBus(context.Background(), bus), // worker ctx mimics what AsWorker installs
		topic,
		"sub",
		SubscriptionConfig{MaxRetries: 1, AckDeadline: time.Second}.withDefaults(),
		msg,
		func(ctx context.Context, e testEvent) error {
			handlerCtx = ctx
			return nil
		},
	)
	if disp != DispositionAck {
		t.Errorf("disposition: want Ack, got %v", disp)
	}

	consumerSpan, ok := trace.SpanFromCtx(handlerCtx)
	if !ok || consumerSpan == nil {
		t.Fatal("handler ctx must carry a span")
	}
	if consumerSpan.TraceID != pubSpan.TraceID {
		t.Errorf("trace stitching failed: publisher TraceID=%s, consumer TraceID=%s",
			pubSpan.TraceID, consumerSpan.TraceID)
	}
	if consumerSpan.ParentID != pubSpan.SpanID {
		t.Errorf("consumer ParentID=%s, want %s (publisher SpanID)",
			consumerSpan.ParentID, pubSpan.SpanID)
	}
}

// TestDispatch_NoTraceparentMintsFreshTrace covers messages with no
// traceparent — e.g. published by an external system or a non-trace
// codepath. The consumer must still get a usable trace, just not
// linked to anything upstream.
func TestDispatch_NoTraceparentMintsFreshTrace(t *testing.T) {
	ResetForTest()
	topic := NewTopic[testEvent]("no-tp", TopicConfig{})
	bus := trace.NewBus(100)

	body, _ := encodeForTest(testEvent{ID: 1})
	msg := Message{Topic: "no-tp", Subscription: "sub", Payload: body, DeliveryAttempt: 1}

	var handlerCtx context.Context
	dispatch(
		trace.WithBus(context.Background(), bus),
		topic,
		"sub",
		SubscriptionConfig{}.withDefaults(),
		msg,
		func(ctx context.Context, e testEvent) error {
			handlerCtx = ctx
			return nil
		},
	)

	sp, ok := trace.SpanFromCtx(handlerCtx)
	if !ok || sp == nil {
		t.Fatal("handler ctx must carry a span")
	}
	// Without an inbound traceparent, StartSpan mints a fresh TraceID.
	// We only require it to be well-formed; the value itself is random.
	if len(sp.TraceID) != 32 {
		t.Errorf("fresh TraceID malformed: %q", sp.TraceID)
	}
	if sp.ParentID != "" {
		t.Errorf("with no remote parent, ParentID should be empty; got %q", sp.ParentID)
	}
}

// TestDispatch_MalformedTraceparentDoesNotCrash covers a hostile or
// buggy producer setting a non-W3C header. The consumer should
// silently fall back to a fresh trace, not panic or skip the
// handler.
func TestDispatch_MalformedTraceparentDoesNotCrash(t *testing.T) {
	ResetForTest()
	topic := NewTopic[testEvent]("bad-tp", TopicConfig{})
	bus := trace.NewBus(100)

	body, _ := encodeForTest(testEvent{ID: 1})
	msg := Message{
		Topic: "bad-tp", Subscription: "sub", Payload: body, DeliveryAttempt: 1,
		Attrs: map[string]string{"traceparent": "garbage-value"},
	}

	called := false
	disp := dispatch(
		trace.WithBus(context.Background(), bus),
		topic,
		"sub",
		SubscriptionConfig{}.withDefaults(),
		msg,
		func(ctx context.Context, e testEvent) error {
			called = true
			return nil
		},
	)
	if !called {
		t.Error("handler should still run when traceparent is malformed")
	}
	if disp != DispositionAck {
		t.Errorf("disposition: want Ack, got %v", disp)
	}
}

// TestPublish_NoSpanInCtx confirms Publish from outside a request
// (cron, worker) still works — Attrs simply lacks traceparent.
func TestPublish_NoSpanInCtx(t *testing.T) {
	ResetForTest()
	topic := NewTopic[testEvent]("no-span", TopicConfig{})
	tr := NewInMemoryTransport()
	t.Cleanup(func() { _ = tr.Close() })
	BindTopics(tr)

	var got Message
	var seen int32
	stop := runConsumer(t, tr, "no-span", "sub",
		ConsumeConfig{MaxRetries: 1, AckDeadline: time.Second},
		func(msg Message) Disposition {
			got = msg
			atomic.AddInt32(&seen, 1)
			return DispositionAck
		})
	defer stop()

	if err := topic.Publish(context.Background(), testEvent{ID: 1}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&seen) == 1 })

	if _, ok := got.Attrs["traceparent"]; ok {
		t.Errorf("traceparent must not be set when ctx has no span; got Attrs=%v", got.Attrs)
	}
}
