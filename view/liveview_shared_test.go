package view

import (
	"context"
	"io"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
)

// shared is the state and events two pages share by embedding it.
type shared struct {
	LiveView
	Count Assign[int]
}

func (s *shared) Bump(ctx context.Context) error {
	s.Count.Set(s.Count.Get() + 1)
	s.PutFlash("info", "bumped")
	return nil
}

type tallyA struct{ shared }
type tallyB struct {
	shared
	Name Assign[string]
}

func (a *tallyA) Mount(ctx context.Context) error { return nil }
func (b *tallyB) Mount(ctx context.Context) error { b.Name.Set("b"); return nil }

func (a *tallyA) Render() templ.Component { return sharedTallyRender("a", &a.shared) }
func (b *tallyB) Render() templ.Component { return sharedTallyRender(b.Name.Get(), &b.shared) }

func sharedTallyRender(name string, s *shared) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "<p>"+name+" "+strconv.Itoa(s.Count.Get())+" "+s.Flash("info")+"</p>")
		return err
	})
}

// A struct a page embeds is part of it: its Assigns are tracked, its
// LiveView is the page's, and its events are the page's.
func TestLiveViewSharedStruct(t *testing.T) {
	for _, v := range []any{&tallyA{}, &tallyB{}} {
		rv := reflect.ValueOf(v)
		if off := trackAssigns(rv).off; off != "" {
			t.Errorf("%T is untracked: %s", v, off)
		}
		if liveViewOf(rv) == nil {
			t.Errorf("%T: no LiveView found", v)
		}
	}

	dropParked()
	t.Cleanup(dropParked)
	app, stop, err := nexus.InProcess(config.Runtime{}, Live[*tallyA]("/a"), Live[*tallyB]("/b"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	for _, page := range []string{"a", "b"} {
		c := dialLive(t, srv, "/"+page+"/_live?url=/"+page)
		untilHTML(t, c, "<p>"+page+" 0 </p>")
		if err := c.WriteJSON(liveEvent{Ref: 1, Event: "Bump"}); err != nil {
			t.Fatal(err)
		}
		untilHTML(t, c, "<p>"+page+" 1 bumped</p>")
	}
}
