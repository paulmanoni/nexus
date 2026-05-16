package template

import (
	"strings"
	"testing"
)

// renderSrc parses + lowers + renders src against the given scope.
// Returns the final stitched HTML so tests can assert on the
// user-visible output. Tests that need to inspect the Rendered
// structure call Parse + Lower + Render directly.
func renderSrc(t *testing.T, src string, scope any, opts ...RenderOption) string {
	t.Helper()
	f, err := Parse("test.nlt", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	frag, err := Lower(f.Template)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	return Render(frag, scope, opts...).HTML()
}

// --- expression evaluator ------------------------------------------

func TestRender_LiteralsAndIdent(t *testing.T) {
	state := struct {
		Name  string
		Count int
	}{Name: "Paul", Count: 3}

	cases := []struct{ src, want string }{
		{`<template><p>{{ Name }}</p></template>`, `<p>Paul</p>`},
		{`<template><p>{{ Count }}</p></template>`, `<p>3</p>`},
		{`<template><p>{{ "literal" }}</p></template>`, `<p>literal</p>`},
		{`<template><p>{{ 42 }}</p></template>`, `<p>42</p>`},
		{`<template><p>{{ true }}</p></template>`, `<p>true</p>`},
		{`<template><p>{{ nil }}</p></template>`, `<p></p>`},
	}
	for _, c := range cases {
		got := renderSrc(t, c.src, state)
		if got != c.want {
			t.Errorf("%q: got %q want %q", c.src, got, c.want)
		}
	}
}

func TestRender_FieldAccess(t *testing.T) {
	type Post struct {
		Title string
		Likes int
	}
	state := struct {
		Post Post
	}{Post: Post{Title: "Hello", Likes: 7}}

	got := renderSrc(t, `<template><p>{{ Post.Title }} ({{ Post.Likes }})</p></template>`, state)
	if got != `<p>Hello (7)</p>` {
		t.Errorf("got %q", got)
	}
}

func TestRender_HTMLEscaping(t *testing.T) {
	state := struct{ Body string }{Body: `<script>alert("xss")</script>`}
	got := renderSrc(t, `<template><p>{{ Body }}</p></template>`, state)
	if strings.Contains(got, "<script>") {
		t.Errorf("script tag should have been escaped; got %q", got)
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Errorf("expected escaped output; got %q", got)
	}
}

func TestRender_AttributeBinding(t *testing.T) {
	state := struct{ URL string }{URL: "/photo.jpg"}
	got := renderSrc(t, `<template><img :src="URL"></template>`, state)
	if !strings.Contains(got, `src="/photo.jpg"`) {
		t.Errorf("got %q", got)
	}
}

func TestRender_AttributeBindingEscapesQuotes(t *testing.T) {
	state := struct{ Title string }{Title: `He said "hi"`}
	got := renderSrc(t, `<template><a :title="Title">x</a></template>`, state)
	if strings.Contains(got, `title="He said "hi""`) {
		t.Errorf("attribute value must be escaped; got %q", got)
	}
	if !strings.Contains(got, `&#34;`) && !strings.Contains(got, `&quot;`) {
		t.Errorf("expected quote escape; got %q", got)
	}
}

// --- helpers and builtins ------------------------------------------

func TestRender_HelperFunction(t *testing.T) {
	state := struct{ Name string }{Name: "paul"}
	got := renderSrc(t, `<template><p>{{ upper(Name) }}</p></template>`, state,
		WithHelpers(map[string]any{
			"upper": strings.ToUpper,
		}))
	if got != `<p>PAUL</p>` {
		t.Errorf("got %q", got)
	}
}

func TestRender_HelperWithMultipleArgs(t *testing.T) {
	state := struct{ Body string }{Body: "Hello, world"}
	truncate := func(s string, n int) string {
		if len(s) <= n {
			return s
		}
		return s[:n] + "…"
	}
	got := renderSrc(t, `<template><p>{{ truncate(Body, 5) }}</p></template>`, state,
		WithHelpers(map[string]any{"truncate": truncate}))
	if got != `<p>Hello…</p>` {
		t.Errorf("got %q", got)
	}
}

func TestRender_BuiltinLen(t *testing.T) {
	state := struct{ Items []int }{Items: []int{1, 2, 3, 4}}
	got := renderSrc(t, `<template><p>{{ len(Items) }} items</p></template>`, state)
	if got != `<p>4 items</p>` {
		t.Errorf("got %q", got)
	}
}

// --- comparisons and branches --------------------------------------

func TestRender_IfTrue(t *testing.T) {
	state := struct{ Show bool }{Show: true}
	got := renderSrc(t, `<template><p nl-if="Show">visible</p></template>`, state)
	if got != `<p>visible</p>` {
		t.Errorf("got %q", got)
	}
}

func TestRender_IfFalse(t *testing.T) {
	state := struct{ Show bool }{Show: false}
	got := renderSrc(t, `<template><p nl-if="Show">hidden</p></template>`, state)
	if got != "" {
		t.Errorf("nl-if=false should render nothing; got %q", got)
	}
}

func TestRender_IfElse(t *testing.T) {
	src := `<template><p nl-if="Logged">in</p><p nl-else>out</p></template>`
	if got := renderSrc(t, src, struct{ Logged bool }{Logged: true}); got != `<p>in</p>` {
		t.Errorf("true: got %q", got)
	}
	if got := renderSrc(t, src, struct{ Logged bool }{Logged: false}); got != `<p>out</p>` {
		t.Errorf("false: got %q", got)
	}
}

func TestRender_IfElseIfElse(t *testing.T) {
	src := `<template><p nl-if="N == 1">one</p><p nl-else-if="N == 2">two</p><p nl-else>many</p></template>`
	if got := renderSrc(t, src, struct{ N int }{N: 1}); got != `<p>one</p>` {
		t.Errorf("N=1: got %q", got)
	}
	if got := renderSrc(t, src, struct{ N int }{N: 2}); got != `<p>two</p>` {
		t.Errorf("N=2: got %q", got)
	}
	if got := renderSrc(t, src, struct{ N int }{N: 5}); got != `<p>many</p>` {
		t.Errorf("N=5: got %q", got)
	}
}

func TestRender_TruthinessSemantics(t *testing.T) {
	src := `<template><p nl-if="V">y</p><p nl-else>n</p></template>`
	cases := []struct {
		v    any
		want string
	}{
		{"hello", `<p>y</p>`},
		{"", `<p>n</p>`},
		{42, `<p>y</p>`},
		{0, `<p>n</p>`},
		{[]int{1}, `<p>y</p>`},
		{[]int{}, `<p>n</p>`},
		{nil, `<p>n</p>`},
	}
	for _, c := range cases {
		state := struct{ V any }{V: c.v}
		if got := renderSrc(t, src, state); got != c.want {
			t.Errorf("V=%#v: got %q want %q", c.v, got, c.want)
		}
	}
}

func TestRender_BinaryArithmetic(t *testing.T) {
	state := struct{ A, B int }{A: 10, B: 3}
	got := renderSrc(t, `<template><p>{{ A + B }} {{ A - B }} {{ A * B }} {{ A / B }} {{ A % B }}</p></template>`, state)
	if got != `<p>13 7 30 3 1</p>` {
		t.Errorf("got %q", got)
	}
}

func TestRender_ShortCircuitAnd(t *testing.T) {
	// If && short-circuits, accessing P.Title when P is nil must not error.
	type Post struct{ Title string }
	state := struct{ P *Post }{P: nil}
	// Use single-quoted attribute so the inner string literal can be double-quoted.
	got := renderSrc(t, `<template><p nl-if='P != nil && P.Title == "x"'>match</p></template>`, state)
	if got != "" {
		t.Errorf("short-circuit failed; got %q", got)
	}
}

// --- loops ----------------------------------------------------------

func TestRender_LoopBasic(t *testing.T) {
	type Item struct {
		ID   int
		Name string
	}
	state := struct{ Items []Item }{Items: []Item{
		{ID: 1, Name: "apple"}, {ID: 2, Name: "pear"}, {ID: 3, Name: "plum"},
	}}
	src := `<template><ul><li nl-for="i in Items" :key="i.ID">{{ i.Name }}</li></ul></template>`
	got := renderSrc(t, src, state)
	want := `<ul><li>apple</li><li>pear</li><li>plum</li></ul>`
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRender_LoopProducesComprehensionWithKeys(t *testing.T) {
	type Item struct{ ID int }
	state := struct{ Items []Item }{Items: []Item{{ID: 10}, {ID: 20}, {ID: 30}}}
	src := `<template><li nl-for="i in Items" :key="i.ID">{{ i.ID }}</li></template>`

	f, _ := Parse("t.nlt", []byte(src))
	frag, _ := Lower(f.Template)
	r := Render(frag, state)

	// Top-level Rendered should have a single Comprehension dynamic.
	if len(r.D) != 1 {
		t.Fatalf("want 1 dynamic, got %d", len(r.D))
	}
	comp, ok := r.D[0].(Comprehension)
	if !ok {
		t.Fatalf("want Comprehension, got %T", r.D[0])
	}
	if len(comp.Rows) != 3 {
		t.Fatalf("want 3 rows, got %d", len(comp.Rows))
	}
	wantKeys := []string{"10", "20", "30"}
	for i, row := range comp.Rows {
		if row.Key != wantKeys[i] {
			t.Errorf("row %d key = %q want %q", i, row.Key, wantKeys[i])
		}
	}
}

func TestRender_LoopEmptyKeepsStableStatics(t *testing.T) {
	// Critical for the diff layer: an empty loop must still produce a
	// Comprehension whose S matches the body's statics, so a later
	// non-empty render's CompDiff doesn't reset statics on the client.
	type Item struct{ ID int }
	src := `<template><li nl-for="i in Items" :key="i.ID">{{ i.ID }}</li></template>`
	f, _ := Parse("t.nlt", []byte(src))
	frag, _ := Lower(f.Template)

	emptyR := Render(frag, struct{ Items []Item }{})
	emptyComp := emptyR.D[0].(Comprehension)
	if len(emptyComp.Rows) != 0 {
		t.Fatalf("expected no rows when items empty; got %d", len(emptyComp.Rows))
	}
	if strings.Join(emptyComp.S, "|") != "<li>|</li>" {
		t.Errorf("empty-loop statics = %v", emptyComp.S)
	}

	fullR := Render(frag, struct{ Items []Item }{Items: []Item{{ID: 1}}})
	fullComp := fullR.D[0].(Comprehension)
	if strings.Join(fullComp.S, "|") != strings.Join(emptyComp.S, "|") {
		t.Errorf("statics differ between empty and full: empty=%v full=%v", emptyComp.S, fullComp.S)
	}
}

func TestRender_LoopFallsBackToIndexKey(t *testing.T) {
	state := struct{ Items []string }{Items: []string{"a", "b"}}
	src := `<template><li nl-for="x in Items">{{ x }}</li></template>`
	f, _ := Parse("t.nlt", []byte(src))
	frag, _ := Lower(f.Template)
	r := Render(frag, state)
	comp := r.D[0].(Comprehension)
	if comp.Rows[0].Key != "0" || comp.Rows[1].Key != "1" {
		t.Errorf("unkeyed loop should index-key; got %q %q", comp.Rows[0].Key, comp.Rows[1].Key)
	}
}

func TestRender_NestedLoop(t *testing.T) {
	type Author struct {
		Name  string
		Tags  []string
	}
	state := struct{ Authors []Author }{Authors: []Author{
		{Name: "A", Tags: []string{"x", "y"}},
		{Name: "B", Tags: []string{"z"}},
	}}
	src := `<template>
<ul nl-for="a in Authors" :key="a.Name">
  <span>{{ a.Name }}</span>
  <em nl-for="t in a.Tags" :key="t">{{ t }}</em>
</ul>
</template>`
	got := renderSrc(t, src, state)
	if !strings.Contains(got, "<span>A</span>") || !strings.Contains(got, "<em>x</em>") || !strings.Contains(got, "<em>z</em>") {
		t.Errorf("nested loop rendering: %q", got)
	}
}

// --- errors as inline markers --------------------------------------

func TestRender_MissingFieldShowsErrorMarker(t *testing.T) {
	state := struct{ Name string }{Name: "Paul"}
	got := renderSrc(t, `<template><p>{{ Misspelled }}</p></template>`, state)
	if !strings.Contains(got, "[!err") {
		t.Errorf("expected inline error marker for missing ident; got %q", got)
	}
}

func TestRender_DivByZeroShowsErrorMarker(t *testing.T) {
	state := struct{ N int }{N: 0}
	got := renderSrc(t, `<template><p>{{ 1 / N }}</p></template>`, state)
	if !strings.Contains(got, "[!err") || !strings.Contains(got, "division by zero") {
		t.Errorf("got %q", got)
	}
}

func TestRender_ErrorHandlerOverride(t *testing.T) {
	state := struct{}{}
	called := false
	got := renderSrc(t, `<template><p>{{ Missing }}</p></template>`, state,
		WithErrorHandler(func(err error, pos Position) string {
			called = true
			return ""
		}))
	if !called {
		t.Error("error handler was not invoked")
	}
	if strings.Contains(got, "[!err") {
		t.Errorf("override should suppress default marker; got %q", got)
	}
	if got != `<p></p>` {
		t.Errorf("expected empty slot; got %q", got)
	}
}

// --- full smoke -----------------------------------------------------

func TestRender_FullSmoke(t *testing.T) {
	type Post struct {
		ID    int
		Title string
		Likes int
	}
	state := struct {
		Posts  []Post
		Filter string
	}{
		Posts: []Post{
			{ID: 1, Title: "First", Likes: 3},
			{ID: 2, Title: "Second", Likes: 0},
		},
		Filter: "f",
	}

	src := `<template>
<div>
  <input :value="Filter">
  <article nl-for="p in Posts" :key="p.ID">
    <h2>{{ p.Title }}</h2>
    <button @click="like">{{ p.Likes }} likes</button>
  </article>
  <p nl-if="len(Posts) == 0">No posts.</p>
</div>
</template>`

	got := renderSrc(t, src, state)
	mustContain := []string{
		// <input> is a void element — lowering emits "/>".
		`<input value="f" />`,
		`<h2>First</h2>`,
		`<h2>Second</h2>`,
		`<button nl-on:click="like">3 likes</button>`,
		`<button nl-on:click="like">0 likes</button>`,
	}
	for _, want := range mustContain {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
	// nl-if branch is false (len > 0), so the "No posts." line must not appear.
	if strings.Contains(got, "No posts") {
		t.Errorf("nl-if branch should have been false; output:\n%s", got)
	}
}