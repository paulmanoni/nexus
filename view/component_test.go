package view

import (
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/a-h/templ"
	"github.com/dop251/goja"

	"github.com/paulmanoni/nexus/v2"
)

var basketRan ran

// tally is a live component, its Render written as instrumented templ
// code is.
type tally struct {
	Name Assign[string]
	N    Assign[int]
}

type tallyProps struct {
	Name  string
	Start int
}

func (c *tally) Mount(ctx context.Context, p tallyProps) error {
	c.Name.Set(p.Name)
	c.N.Set(p.Start)
	return nil
}

func (c *tally) Inc(ctx context.Context) error { c.N.Set(c.N.Get() + 1); return nil }

func (c *tally) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		r := Record(ctx, w)
		_ = r.S(w, 1, `<p class="tally">`)
		if r.Guard(ctx, w, "tally", c) {
			basketRan.hit("tally " + c.Name.Get())
			io.WriteString(w, c.Name.Get()+"="+strconv.Itoa(c.N.Get()))
		}
		r.Close(w)
		_ = r.S(w, 2, `</p>`)
		return nil
	})
}

// basket is a live page placing two tallies.
type basket struct {
	Title Assign[string]
	Start Assign[int]
}

func (p *basket) Rename(ctx context.Context, s string) error { p.Title.Set(s); return nil }
func (p *basket) Restart(ctx context.Context, n int) error   { p.Start.Set(n); return nil }

func (p *basket) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		r := Record(ctx, w)
		_ = r.S(w, 1, `<h1>`)
		if r.Guard(ctx, w, "title", p) {
			basketRan.hit("title")
			io.WriteString(w, p.Title.Get())
		}
		r.Close(w)
		_ = r.S(w, 2, `</h1>`)
		if r.Guard(ctx, w, "a", p) {
			basketRan.hit("a")
			if err := Component[*tally]("a", tallyProps{Name: "a", Start: p.Start.Get()}).Render(ctx, w); err != nil {
				return err
			}
		}
		r.Close(w)
		if r.Guard(ctx, w, "b", p) {
			basketRan.hit("b")
			if err := Component[*tally]("b", tallyProps{Name: "b"}).Render(ctx, w); err != nil {
				return err
			}
		}
		r.Close(w)
		return nil
	})
}

// newBasket is a basket page instance with the tally registered, outside
// a server: its renders and events run directly.
func newBasket(t *testing.T) (context.Context, *liveDef, *instance) {
	t.Helper()
	app := new(nexus.App)
	if _, err := defFor(reflect.TypeFor[*tally]()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		views.Lock()
		delete(views.m, app)
		views.Unlock()
	})
	page, err := newLiveDef(reflect.TypeFor[*basket]())
	if err != nil {
		t.Fatal(err)
	}
	in := page.instance(reflect.ValueOf(&basket{}), nil)
	in.v.Interface().(*basket).Title.Set("Basket")
	return context.WithValue(context.Background(), appKey{}, app), page, in
}

// fire runs an event on the page — or, with comp, on that component — as
// the socket would.
func fire(t *testing.T, ctx context.Context, def *liveDef, in *instance, ev liveEvent) {
	t.Helper()
	r := def.event(ctx, in, ev, func(int, bool) liveReply { return liveReply{} })
	if r.Error != "" {
		t.Fatalf("%s: %s", ev.Event, r.Error)
	}
}

// A component's event renders that component and the page spots it sits
// in, to reach it; the page's other spots and the other component are
// skipped. A page change that passes a component
// new props renders it; one that doesn't, doesn't.
func TestComponentSkipsTheRest(t *testing.T) {
	noVerify(t)
	ctx, def, in := newBasket(t)
	var server treeDiffer
	var browser treeMirror
	step := func(want []string, html ...string) {
		t.Helper()
		basketRan.take()
		msg, full, err := in.diffRender(ctx, &server)
		if err != nil {
			t.Fatal(err)
		}
		wire, _ := marshal(liveReply{Tree: msg, Full: full, Reset: browser.statics == nil})
		var decoded liveReply
		_ = json.Unmarshal(wire, &decoded)
		got, err := browser.apply(decoded.Tree, decoded.Full, decoded.Reset)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range html {
			if !strings.Contains(got, h) {
				t.Fatalf("the page lacks %q:\n%s", h, got)
			}
		}
		var ran []string
		for k := range basketRan.take() {
			ran = append(ran, k)
		}
		if !sameSet(ran, want) {
			t.Fatalf("ran %v, want %v", ran, want)
		}
	}
	step([]string{"title", "a", "b", "tally a", "tally b"}, "<h1>Basket</h1>", "a=0", "b=0")

	fire(t, ctx, def, in, liveEvent{Event: "Inc", C: LiveKey(reflect.TypeFor[*tally]()) + "#a"})
	step([]string{"a", "tally a"}, "a=1", "b=0", "<h1>Basket</h1>")

	fire(t, ctx, def, in, liveEvent{Event: "Rename", Args: raws(`"Cart"`)})
	step([]string{"title"}, "<h1>Cart</h1>", "a=1")

	// New props for a: Mount runs again (tally has no Update).
	fire(t, ctx, def, in, liveEvent{Event: "Restart", Args: raws(`5`)})
	step([]string{"a", "tally a"}, "a=5", "b=0")

	fire(t, ctx, def, in, liveEvent{Event: "Inc", C: LiveKey(reflect.TypeFor[*tally]()) + "#b"})
	step([]string{"b", "tally b"}, "a=5", "b=1")
}

// Random page and component events: every reply rebuilds in the browser
// (the Go mirror and runtime.js) exactly the page a full render gives.
func TestComponentRandomEvents(t *testing.T) {
	was := reportStale
	reportStale = func(_ reflect.Type, labels []string) { t.Errorf("stale spots: %v", labels) }
	t.Cleanup(func() { reportStale = was })
	wasVerify := verifyTracking
	verifyTracking = true
	t.Cleanup(func() { verifyTracking = wasVerify })

	vm := goja.New()
	if _, err := vm.RunString("globalThis.queueMicrotask = (f) => f();\n" + RuntimeJS()); err != nil {
		t.Fatal(err)
	}
	newTree, _ := goja.AssertFunction(vm.Get("__nx").ToObject(vm).Get("_tree"))
	tv, _ := newTree(goja.Undefined())
	jsApply, _ := goja.AssertFunction(tv.ToObject(vm).Get("apply"))

	ctx, def, in := newBasket(t)
	key := LiveKey(reflect.TypeFor[*tally]())
	var server treeDiffer
	var browser treeMirror
	rng := rand.New(rand.NewSource(5))
	for i := 0; i < 300; i++ {
		if i > 0 {
			switch rng.Intn(4) {
			case 0:
				fire(t, ctx, def, in, liveEvent{Event: "Inc", C: key + "#a"})
			case 1:
				fire(t, ctx, def, in, liveEvent{Event: "Inc", C: key + "#b"})
			case 2:
				fire(t, ctx, def, in, liveEvent{Event: "Rename", Args: raws(strconv.Quote([]string{"x", "y"}[rng.Intn(2)]))})
			case 3:
				fire(t, ctx, def, in, liveEvent{Event: "Restart", Args: raws(strconv.Itoa(rng.Intn(3)))})
			}
		}
		msg, full, err := in.diffRender(ctx, &server)
		if err != nil {
			t.Fatal(err)
		}
		want, _, err := in.render(ctx)
		if err != nil {
			t.Fatal(err)
		}
		wire, _ := marshal(liveReply{Tree: msg, Full: full, Reset: i == 0})
		var decoded liveReply
		_ = json.Unmarshal(wire, &decoded)
		got, err := browser.apply(decoded.Tree, decoded.Full, decoded.Reset)
		if err != nil || got != string(want) {
			t.Fatalf("Go mirror, step %d (%v):\n got %s\nwant %s", i, err, got, want)
		}
		res, err := jsApply(goja.Undefined(), vm.ToValue(string(wire)))
		if err != nil || res.String() != string(want) {
			t.Fatalf("runtime.js, step %d (%v):\n got %v\nwant %s", i, err, res, want)
		}
	}
}

// Send names the component type a method belongs to, so the browser finds
// the instance; a page's method stays unnamed.
func TestSendNamesTheComponent(t *testing.T) {
	embedded.Store(LiveKey(reflect.TypeFor[*tally]()), true)
	c := &tally{}
	if call := ScriptAttr(Send(c.Inc)); !strings.Contains(call, `"`+LiveKey(reflect.TypeFor[*tally]())+`")`) {
		t.Fatalf("a component's event = %s", call)
	}
	p := &basket{}
	if call := ScriptAttr(Send(p.Rename, "x")); strings.Count(call, ",") != 2 {
		t.Fatalf("a page's event = %s", call)
	}
}

// A component definition is checked like a page's.
func TestComponentDefErrors(t *testing.T) {
	type noRender struct{}
	if _, err := newLiveDef(reflect.TypeFor[*noRender]()); err == nil {
		t.Fatal("a component needs Render")
	}
}
