package view

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
)

// A broadcast from another replica reaches this replica's pages; one sent
// here is published for the others, and this replica ignores its own.
func TestRelay(t *testing.T) {
	dropParked()
	t.Cleanup(dropParked)
	r := NewMemoryRelay()
	app, stop, err := nexus.InProcess(config.Runtime{},
		nexus.Supply(&likes{}),
		Live[*likesLive]("/likes/:room").Provide(func() *likesLive { return &likesLive{} }),
		UseRelay(r),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })

	// What the other replicas would receive.
	var mu sync.Mutex
	var seen []envelope
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.Subscribe(ctx, func(b []byte) {
		var e envelope
		_ = json.Unmarshal(b, &e)
		mu.Lock()
		seen = append(seen, e)
		mu.Unlock()
	})

	c := dialLive(t, srv, "/likes/lobby/_live")
	reply(t, c)
	waitFor(t, func() bool { return subscribers("likes") == 1 })

	// From another replica.
	b, _ := json.Marshal(envelope{Origin: "elsewhere", Topic: "likes", Data: json.RawMessage(`"remote"`)})
	if err := r.Publish(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if got := reply(t, c); !strings.Contains(got.HTML, `likes/"remote"`) {
		t.Fatalf("a remote broadcast = %+v", got)
	}

	// From here: delivered here once, published once.
	if n := Broadcast(context.Background(), "likes", "local"); n != 1 {
		t.Fatalf("Broadcast reached %d pages", n)
	}
	if got := reply(t, c); !strings.Contains(got.HTML, "likes/local") {
		t.Fatalf("a local broadcast = %+v", got)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range seen {
			if e.Origin == replica && string(e.Data) == `"local"` {
				return true
			}
		}
		return false
	})
	expectNoReply(t, c) // its own broadcast did not come back
}

func TestMessageDecode(t *testing.T) {
	var s string
	if err := (Message{Data: "here"}).Decode(&s); err != nil || s != "here" {
		t.Fatalf("local: %q %v", s, err)
	}
	if err := (Message{Data: json.RawMessage(`"there"`)}).Decode(&s); err != nil || s != "there" {
		t.Fatalf("remote: %q %v", s, err)
	}
	type pet struct{ Name string }
	var p pet
	if err := (Message{Data: map[string]any{"Name": "Rex"}}).Decode(&p); err != nil || p.Name != "Rex" {
		t.Fatalf("converted: %+v %v", p, err)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
