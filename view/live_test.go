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

type greeter struct{ greeting string }

type counterLive struct {
	LiveView
	Count int
	Owner string
	Mode  string
}

type countProps struct {
	Name string `path:"name"`
}

func newCounterLive() *counterLive { return &counterLive{Count: 10} }

func (l *counterLive) Mount(ctx context.Context, g *greeter, p countProps) error {
	l.Owner = g.greeting + " " + p.Name
	l.Mode = "page"
	if l.Connected() {
		l.Mode = "live"
	}
	return nil
}

// Add is an event: a dependency (*greeter) first, then its argument.
func (l *counterLive) Add(ctx context.Context, g *greeter, n int) error {
	if n <= 0 {
		return nexus.Err(nexus.Conflict, "n must be positive")
	}
	l.Count += n
	return nil
}

func (l *counterLive) Reset(ctx context.Context) error { l.Count = 0; return nil }

func (l *counterLive) helper() string { return "not an event" }

func (l *counterLive) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := fmt.Fprintf(w, `<p id="n">%d</p><p id="o">%s</p><p id="m">%s</p><button onclick="%s">+5</button>`,
			l.Count, l.Owner, l.Mode, Send(l.Add, 5).Call)
		return err
	})
}

var counterTemplate = newCounterLive()

func bootLive(t *testing.T) *httptest.Server {
	t.Helper()
	dropParked()
	t.Cleanup(dropParked)
	app, stop, err := nexus.InProcess(config.Runtime{},
		nexus.Supply(&greeter{greeting: "hi"}),
		Live[*counterLive]("/count/:name").Provide(func() *counterLive { return counterTemplate }),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	return srv
}

func dialLive(t *testing.T, srv *httptest.Server, path string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+path, http.Header{"Origin": {srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

var (
	pagesMu sync.Mutex
	pages   = map[*websocket.Conn]*treeMirror{} // what each test connection's browser holds
)

// got is a reply with the page's markup after it, as the browser builds it.
type got struct {
	liveReply
	HTML string
}

// reply reads the next reply that carries a render or answers an event and,
// like the browser, rebuilds the page's markup into HTML.
func reply(t *testing.T, conn *websocket.Conn) got {
	t.Helper()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var r liveReply
		if err := conn.ReadJSON(&r); err != nil {
			t.Fatal(err)
		}
		if r.Tree == nil && r.Error == "" && r.Ref == 0 {
			continue // the connection's resume token alone
		}
		g := got{liveReply: r}
		if r.Tree != nil {
			pagesMu.Lock()
			m := pages[conn]
			if m == nil {
				m = &treeMirror{}
				pages[conn] = m
			}
			html, err := m.apply(r.Tree, r.Full, r.Reset)
			pagesMu.Unlock()
			if err != nil {
				t.Fatalf("a reply that does not apply: %v: %+v", err, r)
			}
			g.HTML = html
		}
		return g
	}
}

// jsonSteps turns decoded JSON patch steps (float64 numbers) back into ints.
func jsonSteps(p []any) []any {
	out := make([]any, len(p))
	for i, s := range p {
		switch v := s.(type) {
		case float64:
			out[i] = int(v)
		case []any:
			ints := make([]int, len(v))
			for j := range v {
				ints[j] = int(v[j].(float64))
			}
			out[i] = ints
		default:
			out[i] = s
		}
	}
	return out
}

func TestLivePage(t *testing.T) {
	srv := bootLive(t)
	res, err := http.Get(srv.URL + "/count/ana")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	page := string(b)
	for _, want := range []string{
		`<nx-live data-nx-live="/count/ana/_live" data-nx-live-join="`,
		`<p id="n">10</p>`,     // the DI template's default
		`<p id="o">hi ana</p>`, // a DI dependency and a path parameter in Mount
		`<p id="m">page</p>`,   // not connected on the first render
		`onclick="__nx.live.send(this,&#34;Add&#34;,[5])"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %s:\n%s", want, page)
		}
	}
}

// Each connection mounts its own copy; events change it and re-render; the
// DI template never changes.
func TestLiveEvents(t *testing.T) {
	srv := bootLive(t)
	a := dialLive(t, srv, "/count/ana/_live")
	if r := reply(t, a); !strings.Contains(r.HTML, `<p id="m">live</p>`) || !strings.Contains(r.HTML, `<p id="n">10</p>`) {
		t.Fatalf("first render = %+v", r)
	}
	send := func(conn *websocket.Conn, event string, args ...any) got {
		raw := make([]json.RawMessage, len(args))
		for i, a := range args {
			raw[i], _ = json.Marshal(a)
		}
		if err := conn.WriteJSON(liveEvent{Event: event, Args: raw}); err != nil {
			t.Fatal(err)
		}
		return reply(t, conn)
	}
	if r := send(a, "Add", 5); !strings.Contains(r.HTML, `<p id="n">15</p>`) {
		t.Fatalf("after Add(5) = %+v", r)
	}
	if r := send(a, "Add", 5); !strings.Contains(r.HTML, `<p id="n">20</p>`) {
		t.Fatalf("after Add(5) twice = %+v", r)
	}
	if r := send(a, "Add", -1); r.Error != "Add: n must be positive" {
		t.Fatalf("a failing event = %+v", r)
	}
	for _, bad := range []string{"Mount", "Render", "helper", "Nope"} {
		if r := send(a, bad); !strings.Contains(r.Error, "no event") {
			t.Fatalf("%s must not be callable: %+v", bad, r)
		}
	}
	if r := send(a, "Add", "five"); !strings.Contains(r.Error, "argument 1") {
		t.Fatalf("a malformed argument = %+v", r)
	}

	b := dialLive(t, srv, "/count/bo/_live")
	if r := reply(t, b); !strings.Contains(r.HTML, `<p id="n">10</p>`) || !strings.Contains(r.HTML, "hi bo") {
		t.Fatalf("another connection starts fresh: %+v", r)
	}
	if r := send(a, "Reset"); !strings.Contains(r.HTML, `<p id="n">0</p>`) {
		t.Fatalf("after Reset = %+v", r)
	}
	if counterTemplate.Count != 10 || counterTemplate.Owner != "" {
		t.Fatalf("the DI template changed: %+v", counterTemplate)
	}
}

func TestSendNeedsAMethodValue(t *testing.T) {
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "method value") {
			t.Fatalf("recover = %v", r)
		}
	}()
	Send(func() {}, 1)
}
