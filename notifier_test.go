package nexus

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNotify_FanOut(t *testing.T) {
	n := NewNotifier()
	chA, cancelA := n.Subscribe()
	chB, cancelB := n.Subscribe()
	defer cancelA()
	defer cancelB()

	n.Notify()

	timeout := time.After(time.Second)
	for i, ch := range []<-chan struct{}{chA, chB} {
		select {
		case <-ch:
		case <-timeout:
			t.Fatalf("subscriber %d did not receive notify", i)
		}
	}
}

func TestNotify_Coalesces(t *testing.T) {
	n := NewNotifier()
	ch, cancel := n.Subscribe()
	defer cancel()

	// 100 rapid Notify calls must not block, must not panic, must
	// eventually be visible as exactly one nudge in the buffered
	// channel (subscriber reads it once and gets state).
	for i := 0; i < 100; i++ {
		n.Notify()
	}

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("subscriber never received any notify")
	}
	// After draining, no more nudges should be sitting in the
	// channel — the rapid-fire 99 extras coalesced into the same
	// pending slot.
	select {
	case <-ch:
		t.Error("expected coalescing; got a second nudge from the buffer")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestNotify_NonBlocking(t *testing.T) {
	n := NewNotifier()
	// Subscribe but never read. Notify must not block even if the
	// channel buffer is full.
	_, cancel := n.Subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			n.Notify()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked when subscriber's buffer was full")
	}
}

func TestSubscribe_CancelRemovesListener(t *testing.T) {
	n := NewNotifier()
	ch, cancel := n.Subscribe()

	cancel()

	// Channel must be closed so a ranger or further read sees nothing.
	select {
	case _, open := <-ch:
		if open {
			t.Error("channel should be closed after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled channel was not closed")
	}

	// Subsequent Notify must not panic and must not somehow re-deliver.
	n.Notify()
}

func TestSubscribe_CancelIdempotent(t *testing.T) {
	n := NewNotifier()
	_, cancel := n.Subscribe()
	cancel()
	cancel() // second call must not panic / double-close
}

func TestNotify_NilNotifier(t *testing.T) {
	// Mutating subsystems may have a nil notifier (no dashboard
	// wired); Notify must be a no-op rather than nil-deref.
	var n *Notifier
	n.Notify()
}

func TestConcurrent_NotifyAndSubscribe(t *testing.T) {
	n := NewNotifier()
	var wg sync.WaitGroup
	var notifies int64
	stop := time.After(200 * time.Millisecond)

	// One ticker thread issuing notifies.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				n.Notify()
				atomic.AddInt64(&notifies, 1)
			}
		}
	}()

	// Several sub/cancel cycles racing the notifier.
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, cancel := n.Subscribe()
			<-ch
			cancel()
		}()
	}

	wg.Wait()
	if atomic.LoadInt64(&notifies) == 0 {
		t.Fatal("no notifies fired during concurrent test")
	}
}
