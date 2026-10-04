package view

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/gorilla/websocket"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
)

// likes is shared server state several live pages show.
type likes struct {
	mu sync.Mutex
	n  int
}

func (l *likes) add() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n++
	return l.n
}

func (l *likes) get() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

type likesLive struct {
	Likes int
	Seen  []string
}

func (p *likesLive) Mount(ctx context.Context, sock *Socket, store *likes, room string) error {
	p.Likes = store.get()
	if room != "quiet" {
		sock.Subscribe("likes")
	}
	return nil
}

func (p *likesLive) Like(ctx context.Context, store *likes) error {
	store.add()
	Broadcast(ctx, "likes", "liked")
	return nil
}

func (p *likesLive) Info(ctx context.Context, store *likes, msg Message) error {
	p.Likes = store.get()
	p.Seen = append(p.Seen, fmt.Sprint(msg.Topic, "/", msg.Data))
	return nil
}

func (p *likesLive) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := fmt.Fprintf(w, `<p id="likes">%d</p><p id="seen">%s</p>`, p.Likes, strings.Join(p.Seen, ","))
		return err
	})
}

func bootLikes(t *testing.T) *httptest.Server {
	t.Helper()
	dropParked()
	t.Cleanup(dropParked)
	app, stop, err := nexus.InProcess(config.Runtime{},
		nexus.Supply(&likes{}),
		Live[*likesLive]("/likes/:room").Provide(func() *likesLive { return &likesLive{} }),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	return srv
}

func expectNoReply(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		var r liveReply
		if err := conn.ReadJSON(&r); err != nil {
			return
		}
		if r.Tree != nil || r.Error != "" || r.Ref != 0 {
			t.Fatalf("unexpected reply %+v", r)
		}
	}
}

func TestLiveBroadcast(t *testing.T) {
	t.Cleanup(setGrace(100 * time.Millisecond))
	srv := bootLikes(t)
	if _, err := http.Get(srv.URL + "/likes/lobby"); err != nil {
		t.Fatal(err)
	}
	if n := subscribers("likes"); n != 0 {
		t.Fatalf("a server-rendered page subscribed: %d", n)
	}

	a := dialLive(t, srv, "/likes/lobby/_live")
	b := dialLive(t, srv, "/likes/lobby/_live")
	q := dialLive(t, srv, "/likes/quiet/_live")
	for _, c := range []*websocket.Conn{a, b, q} {
		if r := reply(t, c); !strings.Contains(r.HTML, `<p id="likes">0</p>`) {
			t.Fatalf("first render = %+v", r)
		}
	}
	if n := subscribers("likes"); n != 2 {
		t.Fatalf("%d subscribers, want 2", n)
	}

	if err := a.WriteJSON(liveEvent{Event: "Like"}); err != nil {
		t.Fatal(err)
	}
	// a renders for its event and again for the broadcast it also receives;
	// b renders for the broadcast; q is not subscribed.
	var last got
	for i := 0; i < 2; i++ {
		last = reply(t, a)
	}
	if !strings.Contains(last.HTML, `<p id="likes">1</p>`) {
		t.Fatalf("a = %+v", last)
	}
	if r := reply(t, b); !strings.Contains(r.HTML, `<p id="likes">1</p>`) || !strings.Contains(r.HTML, "likes/liked") {
		t.Fatalf("b was not pushed the change: %+v", r)
	}
	expectNoReply(t, q)

	if n := Broadcast(context.Background(), "likes", 42); n != 2 {
		t.Fatalf("Broadcast reached %d pages, want 2", n)
	}
	if r := reply(t, b); !strings.Contains(r.HTML, "likes/42") {
		t.Fatalf("a broadcast from outside a page = %+v", r)
	}

	raw, _ := json.Marshal(Message{Topic: "likes"})
	if err := b.WriteJSON(liveEvent{Event: "Info", Args: []json.RawMessage{raw}}); err != nil {
		t.Fatal(err)
	}
	for {
		r := reply(t, b)
		if r.Error != "" {
			if !strings.Contains(r.Error, "no event") {
				t.Fatalf("Info from the browser = %+v", r)
			}
			break
		}
	}

	// Closed pages wait for their connection to come back, then leave.
	a.Close()
	b.Close()
	deadline := time.Now().Add(3 * time.Second)
	for subscribers("likes") != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("closed pages still subscribed: %d", subscribers("likes"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
