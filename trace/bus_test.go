package trace

import (
	"testing"
	"time"
)

func TestBus_PublishSubscribe(t *testing.T) {
	b := NewBus(10)
	_, ch, cancel := b.Subscribe(0, 32)
	defer cancel()

	b.Publish(Event{Kind: KindRequestStart, Service: "s"})

	select {
	case e := <-ch:
		if e.Kind != KindRequestStart || e.Service != "s" {
			t.Fatalf("unexpected event: %+v", e)
		}
		if e.ID == 0 {
			t.Fatal("id not assigned")
		}
		if e.Timestamp.IsZero() {
			t.Fatal("timestamp not assigned")
		}
	case <-time.After(time.Second):
		t.Fatal("no event received")
	}
}

func TestBus_BacklogReplay(t *testing.T) {
	b := NewBus(5)
	for range 3 {
		b.Publish(Event{Kind: KindLog})
	}
	backlog, _, cancel := b.Subscribe(0, 8)
	defer cancel()
	if len(backlog) != 3 {
		t.Fatalf("expected 3 in backlog, got %d", len(backlog))
	}
}

func TestBus_RingEviction(t *testing.T) {
	b := NewBus(3)
	for range 5 {
		b.Publish(Event{Kind: KindLog})
	}
	backlog, _, cancel := b.Subscribe(0, 8)
	defer cancel()
	if len(backlog) != 3 {
		t.Fatalf("expected 3 (ring cap), got %d", len(backlog))
	}
	if backlog[0].ID != 3 || backlog[2].ID != 5 {
		t.Fatalf("expected ids 3..5, got %d..%d", backlog[0].ID, backlog[2].ID)
	}
}

func TestBus_SinceFilter(t *testing.T) {
	b := NewBus(10)
	for range 5 {
		b.Publish(Event{})
	}
	backlog, _, cancel := b.Subscribe(3, 8)
	defer cancel()
	if len(backlog) != 2 {
		t.Fatalf("expected 2 (ids 4,5), got %d", len(backlog))
	}
}

func TestBus_PublishDoesNotBlockOnFullSubscriber(t *testing.T) {
	b := NewBus(10)
	_, _, cancel := b.Subscribe(0, 1) // never drained
	defer cancel()

	done := make(chan struct{})
	go func() {
		for range 100 {
			b.Publish(Event{})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher blocked on slow subscriber")
	}
}

// BenchmarkBusPublishParallel models the dashboard-open production
// shape: every request publishing while one live subscriber drains.
func BenchmarkBusPublishParallel(b *testing.B) {
	bus := NewBus(1000)
	_, ch, cancel := bus.Subscribe(0, 1024)
	defer cancel()
	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			i++
			bus.Publish(Event{TraceID: traceIDs[i%len(traceIDs)], Kind: KindRequestEnd})
		}
	})
	b.StopTimer()
	cancel()
	<-done
}

var traceIDs = func() []string {
	out := make([]string, 64)
	for i := range out {
		out[i] = newTraceID()
	}
	return out
}()

// BenchmarkBusPublishNoSub isolates the ring/mutex cost — the shape when
// the dashboard is enabled but no client is currently connected.
func BenchmarkBusPublishNoSub(b *testing.B) {
	bus := NewBus(1000)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			i++
			bus.Publish(Event{TraceID: traceIDs[i%len(traceIDs)], Kind: KindRequestEnd})
		}
	})
}
