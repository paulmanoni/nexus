package view

import (
	"context"
	"encoding/json"
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

// roomLive tracks itself in a room and lists who is there.
type roomLive struct {
	LiveView
	Online Assign[[]string]
	name   string `view:"-"`
}

type nameProps struct {
	Name string `path:"name"`
}

type seen struct{ Name string }

func (r *roomLive) Mount(ctx context.Context, p nameProps) error {
	r.name = p.Name
	r.Subscribe("room")
	r.Track("room", p.Name, seen{Name: strings.ToUpper(p.Name)})
	r.list()
	return nil
}

func (r *roomLive) list() {
	var out []string
	for _, p := range Presences("room") {
		var s seen
		_ = p.Metas[0].Decode(&s)
		out = append(out, p.Key+"="+s.Name+"x"+string(rune('0'+len(p.Metas))))
	}
	r.Online.Set(out)
}

func (r *roomLive) Info(ctx context.Context, msg Message) error {
	if _, ok := msg.Data.(PresenceDiff); ok {
		r.list()
	}
	return nil
}

func (r *roomLive) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "<p>"+strings.Join(r.Online.Get(), ",")+"</p>")
		return err
	})
}

func bootRoom(t *testing.T, opts ...nexus.Option) *httptest.Server {
	t.Helper()
	dropParked()
	t.Cleanup(dropParked)
	app, stop, err := nexus.InProcess(config.Runtime{},
		append([]nexus.Option{Live[*roomLive]("/room/:name").Provide(func() *roomLive { return &roomLive{} })}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	return srv
}

func untilHTML(t *testing.T, c *websocket.Conn, want string) string {
	t.Helper()
	return untilHTMLFunc(t, c, func(html string) bool { return strings.Contains(html, want) }, want)
}

// untilHTMLFunc reads replies until the page's markup satisfies ok.
func untilHTMLFunc(t *testing.T, c *websocket.Conn, ok func(string) bool, want string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		var r liveReply
		if err := c.ReadJSON(&r); err != nil {
			break
		}
		if r.Tree == nil && r.Error == "" && r.Ref == 0 {
			continue
		}
		if r.Error != "" {
			last = "error: " + r.Error
			continue
		}
		pagesMu.Lock()
		m := pages[c]
		if m == nil {
			m = &treeMirror{}
			pages[c] = m
		}
		html, err := m.apply(r.Tree, r.Full, r.Reset)
		pagesMu.Unlock()
		if err != nil {
			t.Fatalf("a reply that does not apply: %v", err)
		}
		last = html
		if ok(html) {
			return html
		}
	}
	t.Fatalf("never got %q; last: %s", want, last)
	return ""
}

// A page is present while it's open; the others see it join and leave. The
// first, HTTP render tracks nothing.
func TestPresence(t *testing.T) {
	srv := bootRoom(t)
	a := dialLive(t, srv, "/room/ana/_live")
	untilHTML(t, a, "<p>ana=ANAx1</p>")
	if res, err := http.Get(srv.URL + "/room/zed"); err != nil {
		t.Fatal(err)
	} else {
		res.Body.Close()
	}
	b := dialLive(t, srv, "/room/bob/_live")
	untilHTML(t, b, "<p>ana=ANAx1,bob=BOBx1</p>")
	untilHTML(t, a, "<p>ana=ANAx1,bob=BOBx1</p>")
	a2 := dialLive(t, srv, "/room/ana/_live")
	untilHTML(t, a2, "ana=ANAx2")
	untilHTML(t, b, "<p>ana=ANAx2,bob=BOBx1</p>")

	// Leaving: the page ends (its connection closes, then its grace runs out).
	b.Close()
	waitFor(t, func() bool { return parkedCount() == 1 })
	dropParked()
	untilHTML(t, a, "<p>ana=ANAx2</p>")
}

// Across replicas: another replica's joins and leaves reach this one's
// pages, its restated state replaces what it said before, it expires when it
// goes quiet, and this replica's own presence is published.
func TestPresenceRelay(t *testing.T) {
	r := NewMemoryRelay()
	srv := bootRoom(t, UseRelay(r))
	var mu sync.Mutex
	var sent []presenceMsg
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.Subscribe(ctx, func(b []byte) {
		var e envelope
		_ = json.Unmarshal(b, &e)
		if e.Topic != presenceTopic || e.Origin != replica {
			return
		}
		var m presenceMsg
		_ = json.Unmarshal(e.Data, &m)
		mu.Lock()
		sent = append(sent, m)
		mu.Unlock()
	})
	from := func(origin string, m presenceMsg) {
		data, _ := json.Marshal(m)
		b, _ := json.Marshal(envelope{Origin: origin, Topic: presenceTopic, Data: data})
		if err := r.Publish(context.Background(), b); err != nil {
			t.Fatal(err)
		}
	}

	a := dialLive(t, srv, "/room/ana/_live")
	untilHTML(t, a, "<p>ana=ANAx1</p>")
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, m := range sent {
			if m.Kind == "join" && m.Key == "ana" {
				return true
			}
		}
		return false
	})

	from("other", presenceMsg{Kind: "join", Topic: "room", Key: "kim", Ref: "r1", Meta: json.RawMessage(`{"Name":"KIM"}`)})
	untilHTML(t, a, "<p>ana=ANAx1,kim=KIMx1</p>")
	from("other", presenceMsg{Kind: "state", Entries: entries{"room": {"lee": {"r2": json.RawMessage(`{"Name":"LEE"}`)}}}})
	untilHTML(t, a, "<p>ana=ANAx1,lee=LEEx1</p>")

	was := PresenceTTL
	PresenceTTL = 0
	t.Cleanup(func() { PresenceTTL = was })
	expirePresence()
	untilHTML(t, a, "<p>ana=ANAx1</p>")
}

// A stream's change is one batch until it is rendered; the next change
// starts a new one, so the browser applies each once.
func TestStreamBatches(t *testing.T) {
	var s Stream[int]
	s.Configure(func(n int) string { return "i" + string(rune('0'+n)) })
	s.Insert(1)
	s.Insert(2)
	a := s.Attrs()
	if got := s.Items(); len(got) != 2 {
		t.Fatalf("one batch until rendered: %v", got)
	}
	s.Insert(3)
	b := s.Attrs()
	if got := s.Items(); len(got) != 1 || got[0] != 3 {
		t.Fatalf("a new batch after a render: %v", got)
	}
	if a["data-nx-stream-seq"] == b["data-nx-stream-seq"] {
		t.Fatal("each batch has its own seq")
	}
	s.Insert(4)
	s.Delete(4)
	if got := s.Items(); len(got) != 0 || !strings.Contains(s.Attrs()["data-nx-stream-ops"].(string), `"d":["i4"]`) {
		t.Fatalf("a delete takes the item out of the batch and deletes it in the browser: %v %v", got, s.Attrs())
	}
}
