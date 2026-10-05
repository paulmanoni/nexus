package viewgen

import (
	"errors"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

const page = `package app

import (
	"strings"

	"github.com/paulmanoni/nexus/v2/view"
)

//nexus:page GET /
//nexus:auth Required
templ Home() {
	@Counter()
	@Search()
}

templ Counter() {
	{{ count := view.State(ctx, 0) }}
	<button onclick={ count.Set(count.Get() + 1) } disabled?={ count.Get() > 9 }>+1</button>
	<p>Clicked { count.Get() } times</p>
	if count.Get() >= 5 {
		<p>High five</p>
	} else if count.Get() == 0 {
		<p>Start</p>
	} else {
		<p>Keep going</p>
	}
	if len(items) > 0 {
		<p>server if</p>
	}
}

templ Search() {
	{{ s := view.Use[*SearchState](ctx) }}
	<input value={ s.Query.Get() } oninput={ view.Do(func(e view.Event) { s.Query.Set(e.Target.Value) }) }/>
	<b>{ strings.TrimSpace(s.Query.Get()) }</b>
	@Results()
}

templ Results() {
	{{ q := view.Use[*SearchState](ctx).Query }}
	{{ rows := view.Use[*Store](ctx).Find(q.Get()) }}
	for _, r := range rows {
		<li>{ r }</li>
	}
}
`

var pkg = &Package{States: map[string][]string{"SearchState": {"Query"}}}

func TestFile(t *testing.T) {
	res, err := File("app/page.templ", page, pkg)
	if err != nil {
		t.Fatal(err)
	}
	src := string(res.Go)
	for _, want := range []string{
		`ctx = view.Enter(ctx, "Counter")`,
		`view.OnAttr("click", "h`,
		`view.Caps{"count": count}, count.Set(count.Get()+1))`,
		`view.Bind("disabled", "b`,
		`view.Text("t`,
		`view.When("w`,
		`!(count.Get() >= 5) && (count.Get() == 0))`,
		`!(count.Get() >= 5) && !(count.Get() == 0))`, // the else branch
		`if len(items) > 0 {`,                         // a server if stays an if
		`view.Bind("value", "b`,                       // a state struct field is a signal
		`view.Caps{"s.Query": s.Query}, s.Query.Get())`,
		`view.OnAttr("input", "h`,
		`view.ShardStart()`,
		`view.ShardEnd(Results, []any{}, []any{q})`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated Go lacks %s", want)
		}
	}
	if strings.Count(src, "view.ShardStart()") != 1 {
		t.Error("only Results reads a signal on the server")
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "page_templ.go", res.Go, 0); err != nil {
		t.Fatalf("generated Go does not parse: %v", err)
	}
	js := map[string]bool{}
	for _, v := range res.Twins {
		js[v] = true
	}
	for _, want := range []string{
		`(c, e) => { c.count.set((c.count.get() + 1)); }`,
		`(c, e) => { c["s.Query"].set(e.target.value); }`,
		`(c) => (__nx.strings.trimSpace(c["s.Query"].get()))`,
	} {
		if !js[want] {
			t.Errorf("no compiled expression %s among %v", want, res.Twins)
		}
	}
}

// The package registers its page, the shard it detected — with the page's
// gates — and the type its templates Use.
func TestRegistrations(t *testing.T) {
	res, err := File("app/page.templ", page, pkg)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := Registrations([]*Result{res}, pkg, nil)
	if err != nil {
		t.Fatal(err)
	}
	src := string(gen)
	for _, want := range []string{
		`view.Page("GET", "/", Home, auth.Required())`,
		`view.Shard(Results, auth.Required())`,
		`view.Expose[*Store]()`,
		`view.Expose[*SearchState]()`,
		`"github.com/paulmanoni/nexus/v2/extension/auth"`,
		`nexus.RegisterDeferredOptions(`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("view_gen.go lacks %s:\n%s", want, src)
		}
	}
	// Registered in Go already: not registered twice.
	gen, _ = Registrations([]*Result{res}, &Package{States: pkg.States, Shards: map[string]bool{"Results": true}, Exposed: map[string]bool{"*Store": true, "*SearchState": true}}, nil)
	if strings.Contains(string(gen), "view.Shard(") || strings.Contains(string(gen), "view.Expose") {
		t.Errorf("a Go registration must win:\n%s", gen)
	}
}

func registrationErr(t *testing.T, src string) *PositionError {
	t.Helper()
	res, err := File("app/page.templ", src, pkg)
	if err == nil {
		_, err = Registrations([]*Result{res}, pkg, nil)
	}
	var pe *PositionError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a PositionError", err)
	}
	return pe
}

// A shard no page reaches has no gates to inherit, and one reached by pages
// with different gates has no single answer: both must name their own.
func TestShardGatesMustBeKnown(t *testing.T) {
	orphan := strings.Replace(page, "\t@Search()\n", "", 1)
	pe := registrationErr(t, orphan)
	if !strings.Contains(pe.Msg, "Results is a shard") || !strings.Contains(pe.Msg, "no //nexus:page") {
		t.Fatalf("%d: %s", pe.Line, pe.Msg)
	}

	twoPages := page + "\n//nexus:page GET /public\ntempl Public() {\n\t@Results()\n}\n"
	pe = registrationErr(t, twoPages)
	if !strings.Contains(pe.Msg, "different gates (Home, Public)") {
		t.Fatal(pe.Msg)
	}

	own := strings.Replace(twoPages, "templ Results() {", "//nexus:auth Requires view_pets\ntempl Results() {", 1)
	res, err := File("app/page.templ", own, pkg)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := Registrations([]*Result{res}, pkg, nil)
	if err != nil || !strings.Contains(string(gen), `view.Shard(Results, auth.Requires("view_pets"))`) {
		t.Fatalf("%v\n%s", err, gen)
	}
}

func TestDirectiveErrors(t *testing.T) {
	for src, want := range map[string]string{
		strings.Replace(page, "//nexus:page GET /\n", "//nexus:page GET\n", 1):                       "//nexus:page takes a method and a path",
		strings.Replace(page, "//nexus:auth Required\n", "//nexus:cache 5m\n", 1):                    "unknown directive //nexus:cache",
		strings.Replace(page, "templ Counter() {", "//nexus:page GET /c\ntempl Counter(n int) {", 1): "takes no parameters",
	} {
		_, err := File("app/page.templ", src, pkg)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
	pe := registrationErr(t, strings.Replace(page, "templ Counter() {", "//nexus:auth Required\ntempl Counter() {", 1))
	if !strings.Contains(pe.Msg, "Counter is neither") {
		t.Fatal(pe.Msg)
	}
}

// The v1 spelling (//@auth, gofmt's // @auth) is an error at its own line
// that points at the codemod.
func TestLegacyDirectiveSpelling(t *testing.T) {
	for _, legacy := range []string{"//@auth Required\n", "// @auth Required\n"} {
		_, err := File("app/page.templ", strings.Replace(page, "//nexus:auth Required\n", legacy, 1), pkg)
		var pe *PositionError
		if !errors.As(err, &pe) {
			t.Fatalf("%q: err = %v, want a PositionError", legacy, err)
		}
		if pe.Line != 10 || !strings.Contains(pe.Msg, "//@auth is the nexus v1 annotation spelling — write //nexus:auth") ||
			!strings.Contains(pe.Msg, "nexus migrate v2") {
			t.Errorf("%q: %s:%d: %s", legacy, pe.File, pe.Line, pe.Msg)
		}
	}
}

// An @ line that names no component directive is prose: a code example
// in a doc comment compiles.
func TestDocCodeExampleIsNotADirective(t *testing.T) {
	src := strings.Replace(page, "//nexus:auth Required\n", "//nexus:auth Required\n//\n//\t@button.Button(button.Props{Attributes: x}) { ... }\n", 1)
	if _, err := File("app/page.templ", src, pkg); err != nil {
		t.Fatal(err)
	}
}

// A browser expression outside the vocabulary is an error at its line and
// column.
func TestBrowserErrorPosition(t *testing.T) {
	src := strings.Replace(page, `<p>Clicked { count.Get() } times</p>`, `<p>Clicked { count.Get() / 2 } times</p>`, 1)
	_, err := File("app/page.templ", src, pkg)
	var pe *PositionError
	if !errors.As(err, &pe) {
		t.Fatal(err)
	}
	line := strings.Split(src, "\n")[pe.Line-1]
	if pe.Line != 19 || !strings.HasPrefix(line[pe.Col-1:], "count.Get() / 2") || !strings.Contains(pe.Msg, "/ operator") {
		t.Fatalf("%d:%d (%q): %s", pe.Line, pe.Col, line, pe.Msg)
	}
}

func TestReactiveNeedsTheImport(t *testing.T) {
	src := "package app\n\ntempl T() {\n\t{{ c := view.State(ctx, 0) }}\n\t<p>{ c.Get() }</p>\n}\n"
	_, err := File("t.templ", src, nil)
	if err == nil || !strings.Contains(err.Error(), "import") {
		t.Fatal(err)
	}
}

var items []int

// A page in one file and the shard it renders in another: the package pass
// sees both, so the shard still inherits the page's gates.
func TestShardAcrossFiles(t *testing.T) {
	home := "package app\n\n//nexus:page GET /\n//nexus:auth Requires view_pets\ntempl Home() {\n\t@Results()\n}\n"
	results := "package app\n\nimport \"github.com/paulmanoni/nexus/v2/view\"\n\ntempl Results() {\n\t{{ q := view.Use[*SearchState](ctx).Query }}\n\t{{ rows := find(q.Get()) }}\n\tfor _, r := range rows {\n\t\t<li>{ r }</li>\n\t}\n}\n"
	a, err := File("app/home.templ", home, pkg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := File("app/results.templ", results, pkg)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := Registrations([]*Result{a, b}, pkg, nil)
	if err != nil || !strings.Contains(string(gen), `view.Shard(Results, auth.Requires("view_pets"))`) {
		t.Fatalf("%v\n%s", err, gen)
	}
}

// A component library takes extra attributes in its Props: a
// templ.Attributes literal compiles like attributes on an element, and its
// signal reads happen in the browser — they do not make a shard.
func TestLibraryAttributes(t *testing.T) {
	src := `package app

import (
	"github.com/paulmanoni/nexus/v2/view"
	"example.com/ui/button"
)

templ Counter() {
	{{ count := view.State(ctx, 0) }}
	@button.Button(button.Props{Variant: button.VariantOutline, Attributes: templ.Attributes{
		"id":       "inc",
		"onclick":  count.Set(count.Get() + 1),
		"disabled": count.Get() >= 10,
	}}) {
		+1
	}
}
`
	res, err := File("app/counter.templ", src, &Package{})
	if err != nil {
		t.Fatal(err)
	}
	got := string(res.Go)
	for _, want := range []string{
		`view.Attrs(templ.Attributes{"id": "inc"}, view.OnAttr("click", "h`,
		`view.Caps{"count": count}, count.Set(count.Get()+1)), view.Bind("disabled", "b`,
		`view.Caps{"count": count}, count.Get() >= 10))`,
		`Variant: button.VariantOutline`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("generated Go lacks %s", want)
		}
	}
	if strings.Contains(got, "view.ShardStart()") {
		t.Error("reads inside compiled attributes are browser reads, not a shard")
	}
	if len(res.Twins) != 2 {
		t.Fatalf("twins = %v", res.Twins)
	}
	for _, js := range res.Twins {
		if js != `(c, e) => { c.count.set((c.count.get() + 1)); }` && js != `(c) => ((c.count.get() >= 10))` {
			t.Errorf("unexpected twin %s", js)
		}
	}
}

// A live page's Render is a templ method component: its browser-side
// reactivity compiles, view.Send passes through as templ's own script
// attribute, and a server read of a signal is refused.
func TestMethodComponent(t *testing.T) {
	src := `package app

import "github.com/paulmanoni/nexus/v2/view"

templ (l *OrdersLive) Render() {
	{{ open := view.State(ctx, false) }}
	<button onclick={ open.Set(!open.Get()) }>filters</button>
	<div hidden?={ !open.Get() }>…</div>
	for _, o := range l.Orders {
		<li>{ o.Name } <button onclick={ view.Send(l.Cancel, o.ID) }>cancel</button></li>
	}
}
`
	res, err := File("app/orders.templ", src, &Package{})
	if err != nil {
		t.Fatal(err)
	}
	got := string(res.Go)
	for _, want := range []string{
		`ctx = view.Enter(ctx, "OrdersLive.Render")`,
		`view.OnAttr("click", "h`,
		`view.Bind("hidden", "b`,
		`templ.ComponentScript = view.Send(l.Cancel, o.ID)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("generated Go lacks %s", want)
		}
	}
	if strings.Contains(got, "view.ShardStart()") {
		t.Error("a method component is never a shard")
	}
	bad := strings.Replace(src, "for _, o := range l.Orders {", "for _, o := range find(open.Get()) {", 1)
	if _, err := File("app/orders.templ", bad, &Package{}); err == nil || !strings.Contains(err.Error(), "keeps server data in its own fields") {
		t.Fatalf("err = %v", err)
	}
}

// templ drops a script value inside templ.Attributes, so a live event passed
// to a component library's Props.Attributes becomes attribute text.
func TestSendInAttributes(t *testing.T) {
	src := `package app

import (
	"github.com/paulmanoni/nexus/v2/view"
	"example.com/ui/button"
)

templ (b *Board) Render() {
	for _, p := range b.Pets {
		@button.Button(button.Props{Attributes: templ.Attributes{"id": "adopt", "onclick": view.Send(b.Adopt, p.Name)}}) {
			adopt
		}
	}
}
`
	res, err := File("app/board.templ", src, &Package{})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Go); !strings.Contains(got, `templ.Attributes{"id": "adopt", "onclick": view.ScriptAttr(view.Send(b.Adopt, p.Name))}`) {
		t.Errorf("generated Go:\n%s", got)
	}
}
