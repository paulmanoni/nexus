package view

import (
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/a-h/templ"
	"github.com/dop251/goja"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
)

// ran counts how often each spot of a test page ran.
type ran struct {
	mu sync.Mutex
	n  map[string]int
}

func (r *ran) hit(spot string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == nil {
		r.n = map[string]int{}
	}
	r.n[spot]++
}

// take returns the counts since the last take.
func (r *ran) take() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.n
	r.n = nil
	if n == nil {
		n = map[string]int{}
	}
	return n
}

var ordersRan ran

// untracked is what a careless template reads beside its Assigns.
var untracked atomic.Value

// ordersLive is a tracked page, its Render written as instrumented templ
// code is: each spot under Guard.
type ordersLive struct {
	Status Assign[string]
	Rows   Assign[[]string]
	Count  Assign[int]
}

func (p *ordersLive) Mount(ctx context.Context) error {
	p.Status.Set("open")
	p.Rows.Set([]string{"a", "b"})
	return nil
}

func (p *ordersLive) SetStatus(ctx context.Context, s string) error { p.Status.Set(s); return nil }
func (p *ordersLive) Add(ctx context.Context, row string) error {
	p.Rows.Update(func(rows *[]string) { *rows = append(*rows, row) })
	return nil
}
func (p *ordersLive) Inc(ctx context.Context) error   { p.Count.Set(p.Count.Get() + 1); return nil }
func (p *ordersLive) Same(ctx context.Context) error  { p.Count.Set(p.Count.Get()); return nil }
func (p *ordersLive) Fail(ctx context.Context) error  { return nexus.Invalid().Field("name", "bad") }
func (p *ordersLive) Fixed(ctx context.Context) error { return nil }

func (p *ordersLive) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		r := Record(ctx, w)
		ctx = Enter(ctx, "Orders")
		_ = r.S(w, 1, "<h1>")
		if r.Guard(ctx, w, "status", p) {
			ordersRan.hit("status")
			io.WriteString(w, p.Status.Get())
		}
		r.Close(w)
		_ = r.S(w, 2, "</h1><ul>")
		if r.Guard(ctx, w, "rows", p) {
			ordersRan.hit("rows")
			r.ForStart(w)
			for _, row := range p.Rows.Get() {
				r.Item(w)
				_ = r.S(w, 3, "<li>")
				io.WriteString(w, row)
				_ = r.S(w, 4, "</li>")
			}
			r.ForEnd(w)
		}
		r.Close(w)
		_ = r.S(w, 5, "</ul>")
		// A frame whose markup follows Count, around a spot that doesn't
		// read it: when Count changes, the frame is new to the browser and
		// needs the spot's markup.
		r.Open(w)
		if p.Count.Get()%2 == 0 {
			_ = r.S(w, 6, `<p class="even">`)
		} else {
			_ = r.S(w, 7, `<p class="odd">`)
		}
		if r.Guard(ctx, w, "count", p) {
			ordersRan.hit("count")
			io.WriteString(w, strconv.Itoa(p.Count.Get()))
		}
		r.Close(w)
		_ = r.S(w, 8, " of ")
		if r.Guard(ctx, w, "status-again", p) {
			ordersRan.hit("status-again")
			io.WriteString(w, p.Status.Get())
		}
		r.Close(w)
		_ = r.S(w, 9, "</p>")
		r.Close(w)
		if r.Guard(ctx, w, "errors", nil) {
			ordersRan.hit("errors")
			_ = r.S(w, 10, `<i>`)
			io.WriteString(w, Errors(ctx).Field("name"))
			_ = r.S(w, 11, `</i>`)
		}
		r.Close(w)
		// A spot that renders a component, then one that makes a signal:
		// the signal's id depends on the components entered before it, so
		// it must not change when the first is skipped.
		if r.Guard(ctx, w, "child", p) {
			ordersRan.hit("child")
			child := Enter(ctx, "Child")
			_ = child
			io.WriteString(w, "<s>"+p.Status.Get()+"</s>")
		}
		r.Close(w)
		if r.Guard(ctx, w, "signal", p) {
			ordersRan.hit("signal")
			s := State(Enter(ctx, "Child"), 0)
			io.WriteString(w, `<u id="`+s.ID()+`">`+strconv.Itoa(p.Count.Get())+`</u>`)
		}
		r.Close(w)
		if r.Guard(ctx, w, "careless", nil) {
			ordersRan.hit("careless")
			v, _ := untracked.Load().(string)
			io.WriteString(w, "<em>"+v+"</em>")
		}
		r.Close(w)
		return nil
	})
}

func bootOrders(t *testing.T) *httptest.Server {
	t.Helper()
	dropParked()
	t.Cleanup(dropParked)
	untracked.Store("")
	app, stop, err := nexus.InProcess(config.Runtime{},
		Live[*ordersLive]("/orders").Provide(func() *ordersLive { return &ordersLive{} }),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	return srv
}

func raws(vals ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(vals))
	for i, v := range vals {
		out[i] = json.RawMessage(v)
	}
	return out
}

func noVerify(t *testing.T) {
	was := verifyTracking
	verifyTracking = false
	t.Cleanup(func() { verifyTracking = was })
}

// A tracked page runs only the spots whose Assigns changed, and the browser
// still ends up with the whole page.
func TestAssignSkipsWhatDidNotChange(t *testing.T) {
	noVerify(t)
	srv := bootOrders(t)
	c := dialLive(t, srv, "/orders/_live")
	first := reply(t, c)
	if !strings.Contains(first.HTML, "<h1>open</h1><ul><li>a</li><li>b</li></ul>") {
		t.Fatalf("the first render = %s", first.HTML)
	}
	ordersRan.take()

	step := func(ev liveEvent, wantRan []string, wantHTML ...string) got {
		t.Helper()
		ev.Ref = 1
		if err := c.WriteJSON(ev); err != nil {
			t.Fatal(err)
		}
		r := reply(t, c)
		if r.Error != "" {
			t.Fatalf("%s: %s", ev.Event, r.Error)
		}
		for _, w := range wantHTML {
			if !strings.Contains(r.HTML, w) {
				t.Fatalf("%s: the page lacks %q:\n%s", ev.Event, w, r.HTML)
			}
		}
		n := ordersRan.take()
		var gotRan []string
		for k := range n {
			gotRan = append(gotRan, k)
		}
		if !sameSet(gotRan, wantRan) {
			t.Fatalf("%s ran %v, want %v", ev.Event, gotRan, wantRan)
		}
		return r
	}

	step(liveEvent{Event: "Add", Args: raws(`"c"`)}, []string{"rows"}, "<li>c</li>", "<h1>open</h1>")
	step(liveEvent{Event: "SetStatus", Args: raws(`"paid"`)}, []string{"status", "status-again", "child"},
		"<h1>paid</h1>", " of paid</p>", "<s>paid</s>", "<li>c</li>")
	r := step(liveEvent{Event: "Inc"}, []string{"count", "status-again", "signal"}, `<p class="odd">1 of paid</p>`)
	if r.Full {
		t.Fatal("a change travels as a change")
	}
	step(liveEvent{Event: "Same"}, nil, `<p class="odd">1 of paid</p>`)
	step(liveEvent{Event: "Fail"}, []string{"errors"}, "<i>bad</i>")
	step(liveEvent{Event: "Fixed"}, []string{"errors"}, "<i></i>")
	step(liveEvent{Event: "Fixed"}, nil, "<h1>paid</h1>")
}

// Skipping a spot that renders a component keeps the signals made after it
// on the ids the browser holds: the spot replays the component it entered.
func TestAssignKeepsSignalIDs(t *testing.T) {
	noVerify(t)
	srv := bootOrders(t)
	c := dialLive(t, srv, "/orders/_live")
	first := reply(t, c)
	id := between(first.HTML, `<u id="`, `"`)
	if err := c.WriteJSON(liveEvent{Ref: 1, Event: "Inc"}); err != nil {
		t.Fatal(err)
	}
	r := reply(t, c)
	if got := between(r.HTML, `<u id="`, `"`); got != id || id == "" {
		t.Fatalf("the signal's id went from %q to %q", id, got)
	}
}

// Under nexus dev and in tests, a skipping render is checked against a full
// one: a spot that read something untracked is reported, and the browser
// gets the right page.
func TestAssignVerifyCatchesUntrackedReads(t *testing.T) {
	var reported []string
	var mu sync.Mutex
	was := reportStale
	reportStale = func(_ reflect.Type, labels []string) {
		mu.Lock()
		reported = append(reported, labels...)
		mu.Unlock()
	}
	t.Cleanup(func() { reportStale = was })
	wasVerify := verifyTracking
	verifyTracking = true
	t.Cleanup(func() { verifyTracking = wasVerify })

	srv := bootOrders(t)
	c := dialLive(t, srv, "/orders/_live")
	reply(t, c)
	untracked.Store("changed")
	if err := c.WriteJSON(liveEvent{Ref: 1, Event: "Inc"}); err != nil {
		t.Fatal(err)
	}
	r := reply(t, c)
	if !strings.Contains(r.HTML, "<em>changed</em>") {
		t.Fatalf("the browser must get the full render's page:\n%s", r.HTML)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 1 || reported[0] != "careless" {
		t.Fatalf("reported %v, want [careless]", reported)
	}
}

// A page with a field that isn't an Assign renders everything, as before.
type mixedLive struct {
	Status Assign[string]
	Plain  int
}

func (p *mixedLive) Bump(ctx context.Context) error { p.Plain++; return nil }

func (p *mixedLive) Render() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		r := Record(ctx, w)
		_ = r.S(w, 1, "<p>")
		if r.Guard(ctx, w, "plain", p) {
			io.WriteString(w, strconv.Itoa(p.Plain))
		}
		r.Close(w)
		_ = r.S(w, 2, "</p>")
		return nil
	})
}

func TestAssignNeedsEveryField(t *testing.T) {
	if tr := trackAssigns(reflect.ValueOf(&mixedLive{})); tr.off == "" {
		t.Fatal("a page with a plain field must not be tracked")
	}
	dropParked()
	t.Cleanup(dropParked)
	app, stop, err := nexus.InProcess(config.Runtime{},
		Live[*mixedLive]("/mixed").Provide(func() *mixedLive { return &mixedLive{} }),
	)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(func() { srv.Close(); _ = stop(context.Background()) })
	c := dialLive(t, srv, "/mixed/_live")
	reply(t, c)
	if err := c.WriteJSON(liveEvent{Ref: 1, Event: "Bump"}); err != nil {
		t.Fatal(err)
	}
	if r := reply(t, c); !strings.Contains(r.HTML, "<p>1</p>") {
		t.Fatalf("an untracked page renders everything: %s", r.HTML)
	}
}

// Set of an equal comparable value is not a change; Update always is.
func TestAssignVersions(t *testing.T) {
	var a Assign[string]
	a.Set("x")
	v := a.version()
	a.Set("x")
	if a.version() != v {
		t.Fatal("setting an equal value is not a change")
	}
	var s Assign[[]int]
	s.Set(nil)
	v = s.version()
	s.Set(nil)
	if s.version() == v {
		t.Fatal("a slice can't be compared: every Set is a change")
	}
	s.Update(func(x *[]int) { *x = append(*x, 1) })
	if got := s.Get(); len(got) != 1 {
		t.Fatalf("Update = %v", got)
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if !m[x] {
			return false
		}
	}
	return true
}

func between(s, from, to string) string {
	i := strings.Index(s, from)
	if i < 0 {
		return ""
	}
	s = s[i+len(from):]
	if j := strings.Index(s, to); j >= 0 {
		return s[:j]
	}
	return ""
}

// Random events on a tracked page: every reply, skipping what didn't
// change, rebuilds in the browser (the Go mirror and runtime.js) exactly
// the page a full render gives, and no skipped spot is ever stale.
func TestAssignRandomEvents(t *testing.T) {
	was := reportStale
	reportStale = func(_ reflect.Type, labels []string) { t.Errorf("stale spots: %v", labels) }
	t.Cleanup(func() { reportStale = was })
	untracked.Store("")

	vm := goja.New()
	if _, err := vm.RunString("globalThis.queueMicrotask = (f) => f();\n" + RuntimeJS()); err != nil {
		t.Fatal(err)
	}
	newTree, _ := goja.AssertFunction(vm.Get("__nx").ToObject(vm).Get("_tree"))
	tv, err := newTree(goja.Undefined())
	if err != nil {
		t.Fatal(err)
	}
	jsApply, _ := goja.AssertFunction(tv.ToObject(vm).Get("apply"))

	def, err := newLiveDef(reflect.TypeFor[*ordersLive](), "/orders")
	if err != nil {
		t.Fatal(err)
	}
	in := def.instance(reflect.ValueOf(&ordersLive{}), nil)
	ctx := context.Background()
	if err := in.mount(ctx, func(string) string { return "" }, &Socket{}); err != nil {
		t.Fatal(err)
	}
	var server treeDiffer
	var browser treeMirror
	rng := rand.New(rand.NewSource(11))
	events := []string{"SetStatus", "Add", "Inc", "Same", "Fail", "Fixed"}
	skipped := 0
	for i := 0; i < 400; i++ {
		if i > 0 {
			ev := events[rng.Intn(len(events))]
			var args []json.RawMessage
			switch ev {
			case "SetStatus":
				args = raws(strconv.Quote([]string{"open", "paid", "void"}[rng.Intn(3)]))
			case "Add":
				args = raws(strconv.Quote("r" + strconv.Itoa(i)))
			}
			err := in.call(ctx, def.events[ev], nil, args)
			if errs, ok := validation(err); ok {
				in.setErrs(errs)
			} else if err != nil {
				t.Fatal(err)
			} else {
				in.setErrs(nil)
			}
		}
		msg, full, err := in.diffRender(ctx, &server)
		if err != nil {
			t.Fatal(err)
		}
		if server.spots != nil {
			skipped += server.spots.kept
		}
		want, _, err := in.render(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		wire, _ := marshal(liveReply{Tree: msg, Full: full, Reset: i == 0})
		var decoded liveReply
		if err := json.Unmarshal(wire, &decoded); err != nil {
			t.Fatal(err)
		}
		got, err := browser.apply(decoded.Tree, decoded.Full, decoded.Reset)
		if err != nil || got != string(want) {
			t.Fatalf("Go mirror, step %d (%v):\n got %s\nwant %s\nwire %s", i, err, got, want, wire)
		}
		res, err := jsApply(goja.Undefined(), vm.ToValue(string(wire)))
		if err != nil || res.String() != string(want) {
			t.Fatalf("runtime.js, step %d (%v):\n got %v\nwant %s", i, err, res, want)
		}
	}
	if skipped < 400 {
		t.Fatalf("only %d spots were skipped in 400 renders", skipped)
	}
}

// A field tagged view:"-" — one Render doesn't read, or that doesn't change
// once mounted — leaves the page tracked.
func TestAssignUntaggedFields(t *testing.T) {
	type cfg struct{ Title string }
	type page struct {
		Rows  Assign[[]string]
		cfg   *cfg            `view:"-"`
		marks map[string]bool `view:"-"`
	}
	if tr := trackAssigns(reflect.ValueOf(&page{})); tr.off != "" {
		t.Fatalf("tagged fields keep the page untracked: %s", tr.off)
	}
}
