package template

import "testing"

func TestRendered_HTML_PlainStitch(t *testing.T) {
	r := Rendered{
		S: []string{"<p>Hello ", ", you have ", " messages</p>"},
		D: []any{"Paul", "3"},
	}
	got := r.HTML()
	want := "<p>Hello Paul, you have 3 messages</p>"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRendered_HTML_NilDynamic(t *testing.T) {
	// An untaken nl-if branch shows up as nil — should emit nothing.
	r := Rendered{
		S: []string{"<div>", "</div>"},
		D: []any{nil},
	}
	if got := r.HTML(); got != "<div></div>" {
		t.Errorf("nil dynamic should be empty; got %q", got)
	}
}

func TestRendered_HTML_NestedRendered(t *testing.T) {
	inner := Rendered{
		S: []string{"<em>", "</em>"},
		D: []any{"warn"},
	}
	r := Rendered{
		S: []string{"<p>", " here</p>"},
		D: []any{inner},
	}
	if got := r.HTML(); got != "<p><em>warn</em> here</p>" {
		t.Errorf("got %q", got)
	}
}

func TestRendered_HTML_ComprehensionEmpty(t *testing.T) {
	c := Comprehension{
		S:    []string{"<li>", "</li>"},
		Rows: nil,
	}
	r := Rendered{
		S: []string{"<ul>", "</ul>"},
		D: []any{c},
	}
	if got := r.HTML(); got != "<ul></ul>" {
		t.Errorf("empty comp should emit no rows; got %q", got)
	}
}

func TestRendered_HTML_ComprehensionMultiRow(t *testing.T) {
	c := Comprehension{
		S: []string{"<li>", "</li>"},
		Rows: []Row{
			{Key: "1", D: []any{"apple"}},
			{Key: "2", D: []any{"pear"}},
			{Key: "3", D: []any{"plum"}},
		},
	}
	r := Rendered{
		S: []string{"<ul>", "</ul>"},
		D: []any{c},
	}
	want := "<ul><li>apple</li><li>pear</li><li>plum</li></ul>"
	if got := r.HTML(); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRendered_HTML_UnsupportedDynamicMarker(t *testing.T) {
	// A non-{string,Rendered,Comprehension,nil} dynamic must not panic
	// — it emits a visible marker so the bug surfaces in dev.
	r := Rendered{
		S: []string{"a", "b"},
		D: []any{12345},
	}
	got := r.HTML()
	if got != "a[!unsupported dynamic: int]b" {
		t.Errorf("expected marker; got %q", got)
	}
}

func TestRendered_Equal_Identity(t *testing.T) {
	r := Rendered{S: []string{"x", "y"}, D: []any{"a"}}
	if !r.Equal(r) {
		t.Fatal("identical Rendered must be Equal")
	}
}

func TestRendered_Equal_DifferentStatics(t *testing.T) {
	a := Rendered{S: []string{"x", "y"}, D: []any{"v"}}
	b := Rendered{S: []string{"x", "z"}, D: []any{"v"}}
	if a.Equal(b) {
		t.Fatal("different statics must not be Equal")
	}
}

func TestRendered_Equal_DifferentDynamics(t *testing.T) {
	a := Rendered{S: []string{"x", "y"}, D: []any{"v1"}}
	b := Rendered{S: []string{"x", "y"}, D: []any{"v2"}}
	if a.Equal(b) {
		t.Fatal("different dynamics must not be Equal")
	}
}

func TestRendered_Equal_NestedRecurses(t *testing.T) {
	mk := func(inner string) Rendered {
		return Rendered{
			S: []string{"<a>", "</a>"},
			D: []any{Rendered{S: []string{"<b>", "</b>"}, D: []any{inner}}},
		}
	}
	if !mk("x").Equal(mk("x")) {
		t.Fatal("nested Rendered with same content should be Equal")
	}
	if mk("x").Equal(mk("y")) {
		t.Fatal("nested Rendered with different leaf should not be Equal")
	}
}

func TestComprehension_Equal_KeyOrderMatters(t *testing.T) {
	a := Comprehension{
		S: []string{"<li>", "</li>"},
		Rows: []Row{
			{Key: "1", D: []any{"a"}},
			{Key: "2", D: []any{"b"}},
		},
	}
	b := Comprehension{
		S: []string{"<li>", "</li>"},
		Rows: []Row{
			{Key: "2", D: []any{"b"}},
			{Key: "1", D: []any{"a"}},
		},
	}
	if a.Equal(b) {
		t.Fatal("rows in different order should not be Equal")
	}
}

func TestComprehension_Equal_DynamicsCompared(t *testing.T) {
	mk := func(v string) Comprehension {
		return Comprehension{
			S:    []string{"<li>", "</li>"},
			Rows: []Row{{Key: "1", D: []any{v}}},
		}
	}
	if !mk("x").Equal(mk("x")) {
		t.Fatal("same row should be Equal")
	}
	if mk("x").Equal(mk("y")) {
		t.Fatal("differing row dynamics should not be Equal")
	}
}
