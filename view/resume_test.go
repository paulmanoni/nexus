package view

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// firstToken reads the connection's first render and its resume token.
func firstToken(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	r := reply(t, conn)
	if r.Resume == "" {
		t.Fatalf("no resume token: %+v", r)
	}
	return r.Resume
}

// A page whose connection drops carries on with its state when the browser
// comes back with its token; without one, or once the grace is over, it
// mounts afresh.
func TestLiveResume(t *testing.T) {
	srv := bootLive(t)
	a := dialLive(t, srv, "/count/ana/_live")
	token := firstToken(t, a)
	if err := a.WriteJSON(liveEvent{Ref: 1, Event: "Add", Args: []json.RawMessage{json.RawMessage("5")}}); err != nil {
		t.Fatal(err)
	}
	if r := reply(t, a); !strings.Contains(r.HTML, `<p id="n">15</p>`) {
		t.Fatalf("after Add = %+v", r)
	}
	a.Close()
	waitParked(t, 1)

	b := dialLive(t, srv, "/count/ana/_live?resume="+token)
	r := reply(t, b)
	if !r.Resumed || !r.Full || !strings.Contains(r.HTML, `<p id="n">15</p>`) {
		t.Fatalf("a resumed page = %+v", r)
	}
	if r.Resume == "" || r.Resume == token {
		t.Fatalf("a resumed page needs a new token, got %q", r.Resume)
	}

	// A token is good once.
	c := dialLive(t, srv, "/count/ana/_live?resume="+token)
	if r := reply(t, c); r.Resumed || !strings.Contains(r.HTML, `<p id="n">10</p>`) {
		t.Fatalf("a used token = %+v", r)
	}

	// Another page's token doesn't fit this one.
	b.Close()
	waitParked(t, 1)
	d := dialLive(t, srv, "/count/bo/_live?resume="+r.Resume)
	if r := reply(t, d); r.Resumed {
		t.Fatalf("a token for another page resumed: %+v", r)
	}
}

func TestLiveResumeGrace(t *testing.T) {
	grace := ResumeGrace
	ResumeGrace = 50 * time.Millisecond
	t.Cleanup(func() { ResumeGrace = grace })
	srv := bootLive(t)
	a := dialLive(t, srv, "/count/ana/_live")
	token := firstToken(t, a)
	a.Close()
	waitParked(t, 1)
	time.Sleep(100 * time.Millisecond)
	b := dialLive(t, srv, "/count/ana/_live?resume="+token)
	if r := reply(t, b); r.Resumed {
		t.Fatalf("a page resumed after its grace: %+v", r)
	}
}

func waitParked(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for parkedCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d pages parked, want %d", parkedCount(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
