package viewrelay

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// Two replicas' relays on one Redis: what one publishes, both receive.
func TestRelayOverRedis(t *testing.T) {
	srv := miniredis.RunT(t)
	a := New(Config{URL: "redis://" + srv.Addr()})
	b := New(Config{URL: "redis://" + srv.Addr()})
	t.Cleanup(func() { a.Close(); b.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	got := make(chan string, 4)
	for _, r := range []*Relay{a, b} {
		go r.Subscribe(ctx, func(p []byte) { got <- string(p) })
	}
	deadline := time.Now().Add(3 * time.Second)
	for srv.PubSubNumSub("nexus:view")["nexus:view"] < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the relays did not subscribe")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := a.Publish(ctx, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case m := <-got:
			if m != "hello" {
				t.Fatalf("received %q", m)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d of 2 relays received it", i)
		}
	}
	if err := New(Config{URL: "not a url"}).Publish(ctx, nil); err == nil {
		t.Fatal("a bad URL published")
	}
}
