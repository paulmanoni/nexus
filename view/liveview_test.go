package view

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/a-h/templ"
	"github.com/gorilla/websocket"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
)

// ticker is a live view embedded in a page: it subscribes on its own, is
// present on its own, and flashes.
type ticker struct {
	LiveView
	Seen  Assign[int]
	Label Assign[string]
}

type tickerProps struct{ Label string }

func (k *ticker) Mount(ctx context.Context, p tickerProps) error {
	k.Label.Set(p.Label)
	k.Subscribe("ticks")
	k.Track("watchers", k.ID(), nil)
	return nil
}

func (k *ticker) Info(ctx context.Context, msg Message) error {
	k.Seen.Set(k.Seen.Get() + 1)
	return nil
}

func (k *ticker) Poke(ctx context.Context) error {
	k.PutFlash("info", "poked "+k.ID())
	return nil
}

func (k *ticker) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "<i>"+k.ID()+":"+k.Label.Get()+":"+strconv.Itoa(k.Seen.Get())+":"+k.Flash("info")+"</i>")
		return err
	})
}

// pinboard is a routed live view: its props come from the path and query.
type pinboard struct {
	LiveView
	Who   Assign[string]
	Page  Assign[int]
	Shown Assign[bool]
	Calls Assign[int]
}

type pinpinboardProps struct {
	Who  string `path:"who"`
	Page int    `query:"page" validate:"int=0|99"`
}

func (b *pinboard) Mount(ctx context.Context, p pinpinboardProps) error {
	b.Who.Set(p.Who)
	b.Page.Set(p.Page)
	b.Shown.Set(true)
	b.Calls.Set(1)
	return nil
}

func (b *pinboard) Update(ctx context.Context, p pinpinboardProps) error {
	b.Page.Set(p.Page)
	b.Calls.Set(b.Calls.Get() + 1)
	return nil
}

func (b *pinboard) Hide(ctx context.Context) error { b.Shown.Set(false); return nil }
func (b *pinboard) Next(ctx context.Context) error {
	b.PushPatch("/pin/" + b.Who.Get() + "?page=" + strconv.Itoa(b.Page.Get()+1))
	return nil
}

func (b *pinboard) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		io.WriteString(w, "<p>"+b.Who.Get()+" p"+strconv.Itoa(b.Page.Get())+" c"+strconv.Itoa(b.Calls.Get())+"</p>")
		if b.Shown.Get() {
			if err := Component[*ticker]("t1", tickerProps{Label: "one"}).Render(ctx, w); err != nil {
				return err
			}
		}
		return Component[*ticker]("t2", tickerProps{Label: "two"}).Render(ctx, w)
	})
}

// needy takes a dependency: embedding it needs a registration.
type needy struct{ LiveView }

func (n *needy) Mount(ctx context.Context, g *greeter) error { return nil }
func (n *needy) Render() templ.Component                     { return templ.Raw("<b>needy</b>") }

type hostsNeedy struct{ LiveView }

func (h *hostsNeedy) Render() templ.Component { return Component[*needy]("n", nil) }

func bootPinboard(t *testing.T, opts ...nexus.Option) *httptest.Server {
	t.Helper()
	dropParked()
	t.Cleanup(dropParked)
	app, stop, err := nexus.InProcess(config.Runtime{},
		append([]nexus.Option{Live[*pinboard]("/pin/:who")}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	return srv
}

// A routed live view's props come from its path and query, validated; a
// patch of its URL runs Update with the new props.
func TestLiveViewProps(t *testing.T) {
	srv := bootPinboard(t)
	res, err := http.Get(srv.URL + "/pin/ana?page=3")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(b), "<p>ana p3 c1</p>") {
		t.Fatalf("the props of the first render:\n%s", b)
	}
	if res, _ := http.Get(srv.URL + "/pin/ana?page=500"); res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("props failing validate: = %d", res.StatusCode)
	}
	c := dialLive(t, srv, "/pin/ana/_live?url="+"/pin/ana%3Fpage%3D3")
	untilHTML(t, c, "<p>ana p3 c1</p>")
	if err := c.WriteJSON(liveEvent{Ref: 1, Event: "Next"}); err != nil {
		t.Fatal(err)
	}
	untilHTML(t, c, "<p>ana p4 c2</p>")
}

// An embedded live view subscribes and is present on its own, its events
// reach it with its ID, its flash lasts until the page's next event, and a
// view the page stops rendering takes its subscription and presence away.
func TestLiveViewEmbedded(t *testing.T) {
	srv := bootPinboard(t)
	c := dialLive(t, srv, "/pin/ana/_live")
	untilAll(t, c, "<i>t1:one:0:</i>", "<i>t2:two:0:</i>")
	waitFor(t, func() bool { return len(Presences("watchers")) == 2 })

	Broadcast(context.Background(), "ticks", nil)
	untilAll(t, c, "<i>t1:one:1:</i>", "<i>t2:two:1:</i>")

	key := LiveKey(reflectType[*ticker]()) + "#t2"
	if err := c.WriteJSON(liveEvent{Ref: 1, Event: "Poke", C: key}); err != nil {
		t.Fatal(err)
	}
	untilHTML(t, c, "<i>t2:two:1:poked t2</i>")
	if err := c.WriteJSON(liveEvent{Ref: 2, Event: "Hide"}); err != nil {
		t.Fatal(err)
	}
	html := untilHTML(t, c, "<i>t2:two:1:</i>")
	if strings.Contains(html, "t1:") {
		t.Fatalf("a hidden view is gone: %s", html)
	}
	waitFor(t, func() bool { return len(Presences("watchers")) == 1 })
	Broadcast(context.Background(), "ticks", nil)
	untilHTML(t, c, "<i>t2:two:2:</i>")
}

// A live view that takes dependencies is embedded once it is registered —
// view.Live[T]("") — and says so when it isn't.
func TestLiveViewRegistration(t *testing.T) {
	for _, registered := range []bool{false, true} {
		opts := []nexus.Option{nexus.Supply(&greeter{}), Live[*hostsNeedy]("/host")}
		if registered {
			opts = append(opts, Live[*needy](""))
		}
		srv := bootPinboard(t, opts...)
		res, err := http.Get(srv.URL + "/host")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		switch {
		case registered && !strings.Contains(string(b), "<b>needy</b>"):
			t.Fatalf("a registered view embeds: %d %s", res.StatusCode, b)
		case !registered && res.StatusCode != http.StatusInternalServerError:
			t.Fatalf("an unregistered view with dependencies says so: %d %s", res.StatusCode, b)
		}
	}
}

func reflectType[T any]() reflect.Type { return reflect.TypeFor[T]() }

// untilAll reads replies until the page has every one of wants.
func untilAll(t *testing.T, c *websocket.Conn, wants ...string) {
	t.Helper()
	untilHTMLFunc(t, c, func(html string) bool {
		for _, w := range wants {
			if !strings.Contains(html, w) {
				return false
			}
		}
		return true
	}, strings.Join(wants, " & "))
}
