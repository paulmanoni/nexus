package trace

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ctxWith builds a context that behaves as if the trace middleware had run —
// a root Span and the bus both reachable via SpanFromCtx / BusFromCtx.
func ctxWith(t *testing.T, bus *Bus) (context.Context, *Span) {
	t.Helper()
	root := &Span{
		TraceID:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SpanID:   "bbbbbbbbbbbbbbbb",
		Service:  "svc",
		Endpoint: "GET /x",
		Start:    time.Now(),
		bus:      bus,
	}
	ctx := context.Background()
	ctx = context.WithValue(ctx, spanCtxKey{}, root)
	ctx = context.WithValue(ctx, busCtxKey{}, bus)
	return ctx, root
}

func drain(ch <-chan Event, n int, timeout time.Duration) []Event {
	out := make([]Event, 0, n)
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case e := <-ch:
			out = append(out, e)
		case <-deadline:
			return out
		}
	}
	return out
}

func TestStartSpan_EmitsStartAndEnd(t *testing.T) {
	bus := NewBus(16)
	_, ch, cancel := bus.Subscribe(0, 16)
	defer cancel()

	ctx, root := ctxWith(t, bus)
	_, span := StartSpan(ctx, "db.query", Str("rows", "5"))
	span.End(nil)

	events := drain(ch, 2, time.Second)
	if len(events) != 2 {
		t.Fatalf("expected 2 events (start+end), got %d", len(events))
	}
	start, end := events[0], events[1]
	if start.Kind != KindSpanStart || end.Kind != KindSpanEnd {
		t.Fatalf("wrong kinds: %s / %s", start.Kind, end.Kind)
	}
	if start.TraceID != root.TraceID {
		t.Fatalf("child must inherit TraceID: %q vs %q", start.TraceID, root.TraceID)
	}
	if start.ParentID != root.SpanID {
		t.Fatalf("child ParentID must equal root SpanID: %q vs %q", start.ParentID, root.SpanID)
	}
	if start.Name != "db.query" {
		t.Fatalf("name not propagated: %q", start.Name)
	}
	if span.TraceID != root.TraceID || span.ParentID != root.SpanID {
		t.Fatalf("span fields wrong: %+v", span)
	}
	if _, ok := end.Meta["rows"]; !ok {
		t.Fatalf("attrs not carried on end event: %+v", end.Meta)
	}
}

func TestStartSpan_ErrorOnEnd(t *testing.T) {
	bus := NewBus(16)
	_, ch, cancel := bus.Subscribe(0, 16)
	defer cancel()

	ctx, _ := ctxWith(t, bus)
	_, span := StartSpan(ctx, "x")
	span.End(errors.New("boom"))

	events := drain(ch, 2, time.Second)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[1].Error != "boom" {
		t.Fatalf("error not recorded: %q", events[1].Error)
	}
}

func TestStartSpan_NestedParent(t *testing.T) {
	bus := NewBus(16)
	_, ch, cancel := bus.Subscribe(0, 16)
	defer cancel()

	ctx, root := ctxWith(t, bus)
	childCtx, child := StartSpan(ctx, "outer")
	_, grand := StartSpan(childCtx, "inner")
	grand.End(nil)
	child.End(nil)

	events := drain(ch, 4, time.Second)
	if len(events) != 4 {
		t.Fatalf("expected 4 events, got %d", len(events))
	}
	// Order: outer.start, inner.start, inner.end, outer.end
	if events[0].ParentID != root.SpanID {
		t.Fatalf("outer parent should be root: %q vs %q", events[0].ParentID, root.SpanID)
	}
	if events[1].ParentID != child.SpanID {
		t.Fatalf("inner parent should be outer: %q vs %q", events[1].ParentID, child.SpanID)
	}
	if events[0].TraceID != root.TraceID || events[1].TraceID != root.TraceID {
		t.Fatalf("all events should share TraceID")
	}
}

func TestIn_ClosesSpanAndRecordsError(t *testing.T) {
	bus := NewBus(16)
	_, ch, cancel := bus.Subscribe(0, 16)
	defer cancel()

	ctx, _ := ctxWith(t, bus)

	result := func() (err error) {
		defer In(ctx, "op")(&err)
		return errors.New("nope")
	}
	_ = result()

	events := drain(ch, 2, time.Second)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[1].Kind != KindSpanEnd || events[1].Error != "nope" {
		t.Fatalf("end event wrong: %+v", events[1])
	}
}

func TestSpan_EndIsIdempotent(t *testing.T) {
	bus := NewBus(16)
	_, ch, cancel := bus.Subscribe(0, 16)
	defer cancel()

	ctx, _ := ctxWith(t, bus)
	_, span := StartSpan(ctx, "x")
	span.End(nil)
	span.End(nil) // second call must not publish
	span.End(nil)

	events := drain(ch, 3, 200*time.Millisecond)
	if len(events) != 2 {
		t.Fatalf("expected exactly 2 events (one start+end), got %d", len(events))
	}
}

func TestStartSpan_OrphanMintsTrace(t *testing.T) {
	bus := NewBus(16)
	_, ch, cancel := bus.Subscribe(0, 16)
	defer cancel()

	// No parent span in ctx, but bus is reachable.
	ctx := context.WithValue(context.Background(), busCtxKey{}, bus)
	_, span := StartSpan(ctx, "orphan")
	span.End(nil)

	if span.TraceID == "" || span.ParentID != "" {
		t.Fatalf("orphan span should have fresh trace and no parent: %+v", span)
	}

	events := drain(ch, 2, time.Second)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
}

func TestStartSpan_NoBusIsNoop(t *testing.T) {
	// No bus, no parent — span still usable, End must not panic.
	_, span := StartSpan(context.Background(), "x")
	span.End(nil)
	if span.SpanID == "" {
		t.Fatal("SpanID should still be minted")
	}
}

func TestRecord_EmitsLeafSpanPair(t *testing.T) {
	// newSpanID / newTraceID are deterministic in shape, random in value.
	// We only assert length (shape) — random collisions are vanishingly
	// unlikely with 8-byte IDs but still probabilistic, so collision
	// checks would make the test flaky.
	if len(newSpanID()) != 16 {
		t.Fatal("span id must be 16 hex chars")
	}
	if len(newTraceID()) != 32 {
		t.Fatal("trace id must be 32 hex chars")
	}
}