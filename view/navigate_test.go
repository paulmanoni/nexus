package view

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/middleware"
)

// listLive reads its page number from the URL: its props' query field.
type listLive struct {
	LiveView
	Page  string
	Calls int
}

type listProps struct {
	Page string `query:"page"`
}

func (l *listLive) Mount(ctx context.Context, p listProps) error {
	l.Page = p.Page
	l.Calls++
	return nil
}

func (l *listLive) Update(ctx context.Context, p listProps) error {
	l.Page = p.Page
	l.Calls++
	return nil
}

// Next moves to the next page from the server.
func (l *listLive) Next(ctx context.Context) error {
	l.PushPatch("/list?page=next")
	return nil
}

// Away opens another live page from the server.
func (l *listLive) Away(ctx context.Context) error {
	l.PushNavigate("/count/bo")
	return nil
}

func (l *listLive) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := fmt.Fprintf(w, `<p id="page">%s</p><p id="calls">%d</p>`, l.Page, l.Calls)
		return err
	})
}

type closedLive struct{}

func (closedLive) Render() templ.Component { return templ.Raw("closed") }

func bootNav(t *testing.T) *httptest.Server {
	t.Helper()
	dropParked()
	t.Cleanup(dropParked)
	app, stop, err := nexus.InProcess(config.Runtime{},
		nexus.Supply(&greeter{greeting: "hi"}),
		Live[*counterLive]("/count/:name").Provide(func() *counterLive { return counterTemplate }),
		Live[*listLive]("/list").Provide(func() *listLive { return &listLive{} }),
		Live[*closedLive]("/closed", nexus.Use(middleware.Middleware{Name: "closed", HTTP: func(c *httpx.Ctx) {
			c.AbortWithStatus(403)
		}})).Provide(func() *closedLive { return &closedLive{} }),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	return srv
}

func nav(t *testing.T, srv *httptest.Server, conn interface{ WriteJSON(any) error }, ref int, target string) {
	t.Helper()
	if err := conn.WriteJSON(liveEvent{Ref: ref, Event: "__nav", URL: target}); err != nil {
		t.Fatal(err)
	}
}

func TestLivePatch(t *testing.T) {
	srv := bootNav(t)
	c := dialLive(t, srv, "/list/_live?url="+url.QueryEscape("/list?page=2"))
	if r := reply(t, c); !strings.Contains(r.HTML, `<p id="page">2</p>`) || !strings.Contains(r.HTML, `<p id="calls">1</p>`) {
		t.Fatalf("Params runs after Mount with the page's URL: %+v", r)
	}
	nav(t, srv, c, 1, "/list?page=3")
	r := reply(t, c)
	if r.Ref != 1 || r.Patch != "/list?page=3" || r.Full || !strings.Contains(r.HTML, `<p id="page">3</p>`) || !strings.Contains(r.HTML, `<p id="calls">2</p>`) {
		t.Fatalf("a patch = %+v", r)
	}
	if b, _ := json.Marshal(r.Tree); len(b) > 120 {
		t.Fatalf("a patch sent %s", b)
	}
	// From an event.
	if err := c.WriteJSON(liveEvent{Ref: 2, Event: "Next"}); err != nil {
		t.Fatal(err)
	}
	reply(t, c)
	if r := reply(t, c); r.Patch != "/list?page=next" || !strings.Contains(r.HTML, `<p id="page">next</p>`) {
		t.Fatalf("PushPatch = %+v", r)
	}
}

func TestLiveNavigate(t *testing.T) {
	srv := bootNav(t)
	c := dialLive(t, srv, "/list/_live")
	reply(t, c)
	nav(t, srv, c, 1, "/count/bo")
	r := reply(t, c)
	if r.Nav != "/count/bo" || r.Live != "/count/bo/_live" || !strings.Contains(r.HTML, "hi bo") || !strings.Contains(r.HTML, `<p id="m">live</p>`) {
		t.Fatalf("navigating = %+v", r)
	}
	// The new page answers on the same connection.
	if err := c.WriteJSON(liveEvent{Ref: 2, Event: "Add", Args: []json.RawMessage{json.RawMessage("5")}}); err != nil {
		t.Fatal(err)
	}
	if r := reply(t, c); r.Ref != 2 || !strings.Contains(r.HTML, `<p id="n">15</p>`) {
		t.Fatalf("an event after navigating = %+v", r)
	}
	// And back, from the server.
	nav(t, srv, c, 3, "/list?page=9")
	if r := reply(t, c); r.Nav != "/list?page=9" || !strings.Contains(r.HTML, `<p id="page">9</p>`) {
		t.Fatalf("navigating back = %+v", r)
	}
	if err := c.WriteJSON(liveEvent{Ref: 4, Event: "Away"}); err != nil {
		t.Fatal(err)
	}
	reply(t, c)
	if r := reply(t, c); r.Nav != "/count/bo" {
		t.Fatalf("PushNavigate = %+v", r)
	}
}

// A page the connection can't open — not live, or refused by its gates — is
// loaded by the browser.
func TestLiveNavigateFallsBack(t *testing.T) {
	srv := bootNav(t)
	for _, target := range []string{"/nowhere", "/closed", "https://example.com/x"} {
		c := dialLive(t, srv, "/list/_live")
		reply(t, c)
		nav(t, srv, c, 1, target)
		var r liveReply
		if err := c.ReadJSON(&r); err != nil || r.Redirect != target {
			t.Fatalf("%s: %+v %v", target, r, err)
		}
	}
}
