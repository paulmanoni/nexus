package view_test

import (
	"context"
	"io"
	"strconv"
	"testing"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/view"
	"github.com/paulmanoni/nexus/v2/view/viewtest"
)

type note struct {
	ID   int
	Text string
}

// feed shows a stream; Title is an Assign, to change something else.
type feed struct {
	Notes view.Stream[note]
	Title view.Assign[string]
	next  int `view:"-"`
}

func (f *feed) Mount(ctx context.Context) error {
	f.Notes.Configure(func(n note) string { return "n" + strconv.Itoa(n.ID) })
	f.Notes.Limit(-4)
	f.Notes.Reset(note{1, "one"}, note{2, "two"})
	f.next = 3
	return nil
}

func (f *feed) Add(ctx context.Context) error {
	f.Notes.Insert(note{f.next, "n" + strconv.Itoa(f.next)})
	f.next++
	return nil
}
func (f *feed) Top(ctx context.Context) error {
	f.Notes.Prepend(note{f.next, "top"})
	f.next++
	return nil
}
func (f *feed) Edit(ctx context.Context) error  { f.Notes.Insert(note{2, "TWO"}); return nil }
func (f *feed) Drop(ctx context.Context) error  { f.Notes.DeleteID("n1"); return nil }
func (f *feed) Clear(ctx context.Context) error { f.Notes.Reset(note{100, "fresh"}); return nil }
func (f *feed) Rename(ctx context.Context) error {
	f.Title.Set(f.Title.Get() + "!")
	return nil
}

func (f *feed) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if err := view.Script().Render(ctx, w); err != nil {
			return err
		}
		io.WriteString(w, `<h1 id="title">`+f.Title.Get()+`</h1><ul id="notes"`)
		if err := templ.RenderAttributes(ctx, w, f.Notes.Attrs()); err != nil {
			return err
		}
		io.WriteString(w, `>`)
		for _, n := range f.Notes.Items() {
			io.WriteString(w, `<li id="`+f.Notes.ID(n)+`">`+n.Text+`</li>`)
		}
		io.WriteString(w, `</ul>`)
		for _, e := range []struct{ id, call string }{
			{"add", view.Send(f.Add).Call}, {"top", view.Send(f.Top).Call}, {"edit", view.Send(f.Edit).Call},
			{"drop", view.Send(f.Drop).Call}, {"clear", view.Send(f.Clear).Call}, {"rename", view.Send(f.Rename).Call},
		} {
			io.WriteString(w, `<button id="`+e.id+`" onclick="`+e.call+`"></button>`)
		}
		return nil
	})
}

func bootFeed(t *testing.T) *nexus.App {
	t.Helper()
	app, stop, err := nexus.InProcess(config.Runtime{},
		view.Live[*feed]("/feed").Provide(func() *feed { return &feed{} }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	return app
}

// The browser keeps a stream's rows and applies each change once: inserts
// at the end or the top, a replaced row in place, deletes, a reset, the
// limit — and a change elsewhere on the page leaves the rows alone.
func TestStream(t *testing.T) {
	p := viewtest.Mount[*feed](t, bootFeed(t))
	p.Expect("#notes").Text("onetwo")
	p.Click("#add")
	p.Expect("#notes").Text("onetwon3")
	p.Click("#rename")
	p.Expect("#title").Text("!")
	p.Expect("#notes").Text("onetwon3")
	p.Click("#top")
	p.Expect("#notes").Text("toponetwon3")
	p.Click("#edit")
	p.Expect("#notes").Text("toponeTWOn3")
	p.Click("#drop")
	p.Expect("#notes").Text("topTWOn3")
	p.Click("#add").Click("#add")
	p.Expect("#notes").Text("TWOn3n5n6") // the last 4
	p.Click("#rename")
	p.Expect("#notes").Text("TWOn3n5n6")
	p.Click("#clear")
	p.Expect("#notes").Text("fresh")
}

// The same in a real browser.
func TestStreamInBrowser(t *testing.T) {
	p := viewtest.Browser(t, bootFeed(t), "/feed")
	p.Expect("#notes").Text("one\ntwo")
	p.Click("#add")
	p.Expect("#notes").Text("one\ntwo\nn3")
	p.Click("#top")
	p.Expect("#notes").Text("top\none\ntwo\nn3")
	p.Click("#rename") // the buttons moved down: click where they are now
	p.Expect("#title").Text("!")
	p.Expect("#notes").Text("top\none\ntwo\nn3")
	p.Click("#drop")
	p.Expect("#notes").Text("top\ntwo\nn3")
}
