//go:build integration

// Package rabbit_test runs against a live RabbitMQ broker. Requires
// NEXUS_RABBIT_URL to be set, e.g.:
//
//	NEXUS_RABBIT_URL=amqp://guest:guest@localhost:5672/ \
//	    go test -tags=integration ./pubsub/rabbit/...
//
// Without the build tag these tests are invisible to `go test ./...`,
// so contributors who don't run a broker locally aren't blocked.
package rabbit

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/pubsub"
)

// uniqueName produces topic names that don't collide across reruns
// against a long-lived broker (declared exchanges + queues persist).
// Without uniqueness, a test that left behind a queue would skew the
// next run's assertions.
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func newTransport(t *testing.T) *Transport {
	t.Helper()
	url := os.Getenv("NEXUS_RABBIT_URL")
	if url == "" {
		t.Skip("NEXUS_RABBIT_URL not set; skipping live RabbitMQ integration test")
	}
	tr, err := New(Config{URL: url})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

// runConsumer mirrors the in-memory test's helper — starts a Consume
// loop and waits long enough for the queue+binding declarations to
// land before returning.
func runConsumer(t *testing.T, tr *Transport, topic, sub string, cfg pubsub.ConsumeConfig, deliver pubsub.Deliver) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = tr.Consume(ctx, topic, sub, cfg, deliver)
	}()
	// Give Consume a moment to declare + bind. 200ms is enough on a
	// localhost broker; CI runners may need more, but if so the
	// test will fail loudly via the waitFor assertions below
	// rather than silently dropping messages.
	time.Sleep(200 * time.Millisecond)
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("consumer %s/%s did not stop within 5s", topic, sub)
		}
	}
}

func waitFor(t *testing.T, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waitFor: condition not met within %s", d)
}

func TestRabbit_Roundtrip(t *testing.T) {
	tr := newTransport(t)
	topic := uniqueName("rt")

	var got []byte
	var seen int32
	stop := runConsumer(t, tr, topic, "sub",
		pubsub.ConsumeConfig{MaxRetries: 3, AckDeadline: 5 * time.Second, BackoffMin: 50 * time.Millisecond, BackoffMax: 500 * time.Millisecond},
		func(msg pubsub.Message) pubsub.Disposition {
			got = msg.Payload
			atomic.AddInt32(&seen, 1)
			return pubsub.DispositionAck
		})
	defer stop()

	if err := tr.Publish(context.Background(), topic, []byte(`{"hello":"world"}`), nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return atomic.LoadInt32(&seen) == 1 })

	if string(got) != `{"hello":"world"}` {
		t.Errorf("payload mismatch: got %s", got)
	}
}

func TestRabbit_AttrsRoundTrip(t *testing.T) {
	tr := newTransport(t)
	topic := uniqueName("attrs")

	var got pubsub.Message
	var seen int32
	stop := runConsumer(t, tr, topic, "sub",
		pubsub.ConsumeConfig{MaxRetries: 3, AckDeadline: 5 * time.Second},
		func(msg pubsub.Message) pubsub.Disposition {
			got = msg
			atomic.AddInt32(&seen, 1)
			return pubsub.DispositionAck
		})
	defer stop()

	attrs := map[string]string{
		"traceparent":    "00-0123456789abcdef0123456789abcdef-fedcba9876543210-01",
		"x-tenant-id":    "acme",
	}
	if err := tr.Publish(context.Background(), topic, []byte(`{}`), attrs); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return atomic.LoadInt32(&seen) == 1 })

	if got.Attrs["traceparent"] != attrs["traceparent"] {
		t.Errorf("traceparent lost in round-trip: got %q", got.Attrs["traceparent"])
	}
	if got.Attrs["x-tenant-id"] != "acme" {
		t.Errorf("custom header lost: got %q", got.Attrs["x-tenant-id"])
	}
}

func TestRabbit_FanOut(t *testing.T) {
	tr := newTransport(t)
	topic := uniqueName("fanout")

	var seenA, seenB int32
	stopA := runConsumer(t, tr, topic, "sub-a",
		pubsub.ConsumeConfig{MaxRetries: 1, AckDeadline: 5 * time.Second},
		func(msg pubsub.Message) pubsub.Disposition { atomic.AddInt32(&seenA, 1); return pubsub.DispositionAck })
	defer stopA()
	stopB := runConsumer(t, tr, topic, "sub-b",
		pubsub.ConsumeConfig{MaxRetries: 1, AckDeadline: 5 * time.Second},
		func(msg pubsub.Message) pubsub.Disposition { atomic.AddInt32(&seenB, 1); return pubsub.DispositionAck })
	defer stopB()

	if err := tr.Publish(context.Background(), topic, []byte(`{}`), nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return atomic.LoadInt32(&seenA) == 1 && atomic.LoadInt32(&seenB) == 1
	})
}

func TestRabbit_RetryThenSucceed(t *testing.T) {
	tr := newTransport(t)
	topic := uniqueName("retry-ok")

	var attempts int32
	stop := runConsumer(t, tr, topic, "sub",
		pubsub.ConsumeConfig{MaxRetries: 5, AckDeadline: 5 * time.Second, BackoffMin: 20 * time.Millisecond, BackoffMax: 100 * time.Millisecond},
		func(msg pubsub.Message) pubsub.Disposition {
			n := atomic.AddInt32(&attempts, 1)
			if n < 3 {
				return pubsub.DispositionRetry
			}
			return pubsub.DispositionAck
		})
	defer stop()

	if err := tr.Publish(context.Background(), topic, []byte(`{}`), nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool { return atomic.LoadInt32(&attempts) >= 3 })
}

// TestRabbit_RetryExhausted publishes a message that the handler
// always rejects. The consumer should retry MaxRetries times, then
// stop redelivering — verified by counting attempts and confirming
// the count plateaus.
func TestRabbit_RetryExhausted(t *testing.T) {
	tr := newTransport(t)
	topic := uniqueName("retry-bad")

	var attempts int32
	var mu sync.Mutex
	last := time.Now()
	stop := runConsumer(t, tr, topic, "sub",
		pubsub.ConsumeConfig{MaxRetries: 3, AckDeadline: 5 * time.Second, BackoffMin: 20 * time.Millisecond, BackoffMax: 100 * time.Millisecond},
		func(msg pubsub.Message) pubsub.Disposition {
			mu.Lock()
			last = time.Now()
			mu.Unlock()
			atomic.AddInt32(&attempts, 1)
			return pubsub.DispositionRetry
		})
	defer stop()

	if err := tr.Publish(context.Background(), topic, []byte(`{}`), nil); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Poll until the attempt count has plateaued for a full second —
	// that's our signal the dispatcher gave up and routed to DLQ.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		quiet := time.Since(last) > time.Second
		mu.Unlock()
		if quiet && atomic.LoadInt32(&attempts) >= 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	got := atomic.LoadInt32(&attempts)
	if got < 3 {
		t.Errorf("expected >= 3 attempts before DLQ, got %d", got)
	}
	if got > 5 {
		t.Errorf("retry budget overshot: got %d attempts (MaxRetries=3)", got)
	}
}