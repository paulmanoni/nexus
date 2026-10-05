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

// counter is a live component: a count of its own and a label from props.
type counter struct {
	N     view.Assign[int]
	Label view.Assign[string]
}

type counterProps struct{ Label string }

func (c *counter) Mount(ctx context.Context, p counterProps) error {
	c.Label.Set(p.Label)
	return nil
}

// Update keeps the count: only the label follows the props.
func (c *counter) Update(ctx context.Context, p counterProps) error {
	c.Label.Set(p.Label)
	return nil
}

func (c *counter) Inc(ctx context.Context) error { c.N.Set(c.N.Get() + 1); return nil }

func (c *counter) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		_, err := io.WriteString(w, `<span class="label">`+templ.EscapeString(c.Label.Get())+`</span><b class="n">`+
			strconv.Itoa(c.N.Get())+`</b><button class="inc" onclick="`+view.Send(c.Inc).Call+`">+</button>`)
		return err
	})
}

// shop is a live page placing two counters.
type shop struct {
	Title view.Assign[string]
	ShowB view.Assign[bool]
}

func (p *shop) Mount(ctx context.Context) error {
	p.Title.Set("Shop")
	p.ShowB.Set(true)
	return nil
}

func (p *shop) Rename(ctx context.Context) error { p.Title.Set("Store"); return nil }
func (p *shop) ToggleB(ctx context.Context) error {
	p.ShowB.Set(!p.ShowB.Get())
	return nil
}
func (p *shop) Relabel(ctx context.Context) error {
	return view.UpdateComponent[*counter](ctx, "b", counterProps{Label: "B!"})
}

func (p *shop) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if err := view.Script().Render(ctx, w); err != nil {
			return err
		}
		io.WriteString(w, `<h1>`+templ.EscapeString(p.Title.Get())+`</h1>`)
		io.WriteString(w, `<div id="a">`)
		if err := view.Component[*counter]("a", counterProps{Label: "A " + p.Title.Get()}).Render(ctx, w); err != nil {
			return err
		}
		io.WriteString(w, `</div><div id="b">`)
		if p.ShowB.Get() {
			if err := view.Component[*counter]("b", counterProps{Label: "B"}).Render(ctx, w); err != nil {
				return err
			}
		}
		io.WriteString(w, `</div>`)
		for _, b := range []struct{ id, call string }{
			{"rename", view.Send(p.Rename).Call}, {"toggle", view.Send(p.ToggleB).Call}, {"relabel", view.Send(p.Relabel).Call},
		} {
			io.WriteString(w, `<button id="`+b.id+`" onclick="`+b.call+`"></button>`)
		}
		return nil
	})
}

func bootShop(t *testing.T) *nexus.App {
	t.Helper()
	app, stop, err := nexus.InProcess(config.Runtime{},
		view.LiveComponent[*counter](),
		view.Live[*shop]("/shop").Provide(func() *shop { return &shop{} }),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	return app
}

// Each component's events reach its own instance; the page's props follow
// into Update, which keeps the component's state; UpdateComponent gives new
// props from a page event; a component the template stops rendering is
// dropped, and comes back mounted afresh.
func TestLiveComponents(t *testing.T) {
	p := viewtest.Mount[*shop](t, bootShop(t))
	p.Expect("#a .label").Text("A Shop")
	p.Click("#a .inc").Click("#a .inc").Click("#b .inc")
	p.Expect("#a .n").Text("2")
	p.Expect("#b .n").Text("1")

	p.Click("#rename")
	p.Expect("h1").Text("Store")
	p.Expect("#a .label").Text("A Store")
	p.Expect("#a .n").Text("2")

	p.Click("#relabel")
	p.Expect("#b .label").Text("B!")
	p.Expect("#b .n").Text("1")
	p.Click("#a .inc")
	p.Expect("#a .n").Text("3")
	p.Expect("#b .label").Text("B!")

	p.Click("#toggle")
	p.Expect("#b .n").Absent()
	p.Click("#toggle")
	p.Expect("#b .n").Text("0")
	p.Expect("#b .label").Text("B")
	p.Expect("#a .n").Text("3")
}
