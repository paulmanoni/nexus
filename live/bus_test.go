package live

import (
	"sync/atomic"
	"testing"
	"time"
)

// fakeBus is a test double for Bus: Publish appends to a slice
// callers can inspect; Subscribe returns an injectable channel
// so the test can simulate peer messages.
type fakeBus struct {
	in        chan string
	published []string
	closed    atomic.Bool
}

func newFakeBus() *fakeBus {
	return &fakeBus{in: make(chan string, 4)}
}

func (b *fakeBus) Publish(topic string) error {
	b.published = append(b.published, topic)
	return nil
}

func (b *fakeBus) Subscribe() (<-chan string, func(), error) {
	return b.in, func() {}, nil
}

func (b *fakeBus) Close() error {
	b.closed.Store(true)
	return nil
}

// Notify must publish "" to the attached bus AND wake local
// subscribers. The "and" matters because monolithic deployments
// (no bus) and distributed deployments must behave the same
// from a local caller's perspective.
func TestAttachBus_NotifyPublishesAndWakesLocal(t *testing.T) {
	n := New()
	b := newFakeBus()
	cancel, err := n.AttachBus(b)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer cancel()

	ch, unsub := n.Subscribe()
	defer unsub()

	n.Notify()

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("local subscriber didn't wake")
	}
	if got, want := b.published, []string{""}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("published = %v want %v", got, want)
	}
}

// NotifyTopic must publish the topic AND wake local topic
// subscribers (but NOT broadcast subscribers — that part is
// the existing Notifier contract).
func TestAttachBus_NotifyTopicPublishesAndScopes(t *testing.T) {
	n := New()
	b := newFakeBus()
	cancel, _ := n.AttachBus(b)
	defer cancel()

	all, unsub1 := n.Subscribe()
	defer unsub1()
	posts, unsub2 := n.SubscribeTopic("post:42")
	defer unsub2()

	n.NotifyTopic("post:42")

	select {
	case <-posts:
	case <-time.After(time.Second):
		t.Fatal("topic subscriber didn't wake")
	}
	select {
	case <-all:
		t.Error("broadcast subscriber woke on topic-scoped notify")
	case <-time.After(50 * time.Millisecond):
	}
	if got := b.published; len(got) != 1 || got[0] != "post:42" {
		t.Errorf("published = %v want [post:42]", got)
	}
}

// Incoming bus messages must fan out to local subscribers (this
// is the peer-to-local direction — node A publishes, node B
// receives and wakes its sessions).
func TestAttachBus_IncomingFansOutToLocal(t *testing.T) {
	n := New()
	b := newFakeBus()
	cancel, _ := n.AttachBus(b)
	defer cancel()

	all, unsub1 := n.Subscribe()
	defer unsub1()
	posts, unsub2 := n.SubscribeTopic("post:42")
	defer unsub2()

	// Simulate a peer publish: push a topic into the bus's
	// incoming channel and verify the right subscribers wake.
	b.in <- "post:42"
	select {
	case <-posts:
	case <-time.After(time.Second):
		t.Fatal("post:42 subscriber didn't wake on incoming bus message")
	}
	// Broadcast subscriber shouldn't wake for a topic-scoped
	// incoming, mirroring the local NotifyTopic contract.
	select {
	case <-all:
		t.Error("broadcast woke on topic-scoped incoming")
	case <-time.After(50 * time.Millisecond):
	}

	// Empty-topic incoming == global notify; broadcast should wake.
	b.in <- ""
	select {
	case <-all:
	case <-time.After(time.Second):
		t.Fatal("broadcast didn't wake on empty-topic incoming")
	}
}

// Double-attach is an error — a Notifier hosts one bus at a time.
func TestAttachBus_RejectsDouble(t *testing.T) {
	n := New()
	b1 := newFakeBus()
	c1, err := n.AttachBus(b1)
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	defer c1()

	b2 := newFakeBus()
	if _, err := n.AttachBus(b2); err == nil {
		t.Fatal("expected double-attach error")
	}
}
