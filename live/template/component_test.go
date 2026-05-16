package template

import (
	"strings"
	"testing"
)

// --- helpers --------------------------------------------------------

// renderWith2 registers a parent and a child template against a fresh
// engine, then renders the parent against the given scope. Returns
// the final HTML so tests can assert directly on the user-visible
// output without re-deriving the wire format.
func renderWith2(t *testing.T, parentName, parentSrc, childName, childSrc string, scope any, childFactory func() Component) string {
	t.Helper()
	e := New()
	if err := e.Register(childName, []byte(childSrc), childFactory); err != nil {
		t.Fatalf("register child: %v", err)
	}
	// Parent uses a no-op factory; we render via the API instead of
	// going through a session so tests don't need WS plumbing.
	if err := e.Register(parentName, []byte(parentSrc), func() Component { return &dummyComponent{} }); err != nil {
		t.Fatalf("register parent: %v", err)
	}

	parentDef, _ := e.lookup(parentName)
	return Render(parentDef.fragment, scope, WithComponents(e)).HTML()
}

// dummyComponent is a placeholder satisfying Component for parent
// templates whose state lives entirely in the test's scope argument.
type dummyComponent struct{ BaseComponent }

// --- basic prop rendering -------------------------------------------

// cardUser is the named type shared between the userCard component
// and the parent scope's data, so reflect's type-equality assignment
// path succeeds for the :user= bind prop. (Anonymous struct types
// don't compare equal across files even when fields match.)
type cardUser struct {
	Name  string
	Email string
}

type userCard struct {
	BaseComponent
	User    cardUser
	Compact bool
}

const userCardTmpl = `<template><div class="card">{{ User.Name }}<em nl-if="!Compact"> ({{ User.Email }})</em></div></template>`

func TestComponent_RendersWithBindProp(t *testing.T) {
	parent := `<template><UserCard :user="U" /></template>`

	got := renderWith2(t, "Parent", parent, "UserCard", userCardTmpl,
		struct{ U cardUser }{U: cardUser{Name: "paul", Email: "p@example.com"}},
		func() Component { return &userCard{} },
	)
	if !strings.Contains(got, `<div class="card">paul<em> (p@example.com)</em></div>`) {
		t.Errorf("got %q", got)
	}
}

func TestComponent_LiteralPropAssigns(t *testing.T) {
	// Literal "true" string flows in; coerceStringTo converts to bool.
	parent := `<template><UserCard :user="U" compact="true" /></template>`

	got := renderWith2(t, "Parent", parent, "UserCard", userCardTmpl,
		struct{ U cardUser }{U: cardUser{Name: "paul", Email: "p@example.com"}},
		func() Component { return &userCard{} },
	)
	if strings.Contains(got, "p@example.com") {
		t.Errorf("compact=true should hide email; got %q", got)
	}
	if !strings.Contains(got, "paul") {
		t.Errorf("name should still render; got %q", got)
	}
}

// --- component inside nl-for ----------------------------------------

func TestComponent_InsideLoop_KeyedRows(t *testing.T) {
	// Use cardUser as the row type so the :user="u" bind assigns
	// cleanly (same named type both sides of the prop).
	type loopRow struct {
		ID   int
		User cardUser
	}
	rows := []loopRow{
		{ID: 1, User: cardUser{Name: "a", Email: "a@x"}},
		{ID: 2, User: cardUser{Name: "b", Email: "b@x"}},
		{ID: 3, User: cardUser{Name: "c", Email: "c@x"}},
	}
	parent := `<template><ul><UserCard nl-for="r in Rows" :key="r.ID" :user="r.User" /></ul></template>`

	got := renderWith2(t, "Parent", parent, "UserCard", userCardTmpl,
		struct{ Rows []loopRow }{Rows: rows},
		func() Component { return &userCard{} },
	)
	for _, name := range []string{"a", "b", "c"} {
		if !strings.Contains(got, `<div class="card">`+name+`<em`) {
			t.Errorf("missing row for %q in:\n%s", name, got)
		}
	}
}

// --- diff behavior across re-renders --------------------------------

func TestComponent_DiffIsSparseWhenPropsUnchanged(t *testing.T) {
	type U struct{ Name, Email string }
	e := New()
	_ = e.Register("UserCard", []byte(userCardTmpl), func() Component { return &userCard{} })
	_ = e.Register("Parent", []byte(`<template><UserCard :user="U" /></template>`),
		func() Component { return &dummyComponent{} })
	def, _ := e.lookup("Parent")

	scope := struct{ U U }{U: U{Name: "x", Email: "x@x"}}
	r1 := Render(def.fragment, scope, WithComponents(e))
	r2 := Render(def.fragment, scope, WithComponents(e))

	if !r1.Equal(r2) {
		t.Fatal("identical scope → identical Rendered required for diff stability")
	}
	if d := DiffRendered(r1, r2); d != nil {
		t.Errorf("unchanged props should produce nil diff; got %#v", d)
	}
}

func TestComponent_DiffShipsOnlyChangedSlot(t *testing.T) {
	type U struct{ Name, Email string }
	e := New()
	_ = e.Register("UserCard", []byte(userCardTmpl), func() Component { return &userCard{} })
	_ = e.Register("Parent", []byte(`<template><UserCard :user="U" /></template>`),
		func() Component { return &dummyComponent{} })
	def, _ := e.lookup("Parent")

	r1 := Render(def.fragment, struct{ U U }{U: U{Name: "old", Email: "o@x"}}, WithComponents(e))
	r2 := Render(def.fragment, struct{ U U }{U: U{Name: "new", Email: "o@x"}}, WithComponents(e))

	d := DiffRendered(r1, r2)
	if d == nil {
		t.Fatal("diff should be non-nil")
	}
	// Component slot at top level is at index 0; nested sparse diff
	// touches the Name slot inside the child Rendered.
	inner, ok := d["0"].(Diff)
	if !ok {
		t.Fatalf("slot 0 should be a nested Diff; got %T", d["0"])
	}
	// Inner: statics around {{ User.Name }} mean Name is slot 0 in
	// the child Rendered. Just assert it carries "new" somewhere.
	found := false
	for _, v := range inner {
		if s, ok := v.(string); ok && s == "new" {
			found = true
		}
	}
	if !found {
		t.Errorf("inner diff didn't carry the new name: %#v", inner)
	}
}

// --- error paths ----------------------------------------------------

func TestComponent_UnknownNameEmitsMarker(t *testing.T) {
	e := New()
	_ = e.Register("Parent", []byte(`<template><Mystery /></template>`),
		func() Component { return &dummyComponent{} })
	def, _ := e.lookup("Parent")
	html := Render(def.fragment, &dummyComponent{}, WithComponents(e)).HTML()
	// Error message text is HTML-escaped on the way into the output
	// (formatText calls html.EscapeString on the marker), so the name
	// appears as Mystery without surrounding quotes — assertions can't
	// rely on raw " characters here.
	if !strings.Contains(html, "[!err") || !strings.Contains(html, "Mystery") {
		t.Errorf("expected error marker referencing Mystery; got %q", html)
	}
}

func TestComponent_MissingFieldEmitsMarker(t *testing.T) {
	// HTML tokenizer lowercases attribute names, so :unknownField
	// arrives as :unknownfield — the prop-not-found marker reports
	// the lowercased name as it was actually seen.
	parent := `<template><UserCard :unknownField="X" /></template>`
	html := renderWith2(t, "Parent", parent, "UserCard", userCardTmpl,
		struct{ X string }{X: "v"},
		func() Component { return &userCard{} },
	)
	if !strings.Contains(html, "[!err") || !strings.Contains(html, "unknownfield") {
		t.Errorf("expected prop-not-found marker; got %q", html)
	}
}

func TestComponent_NoResolverEmitsMarker(t *testing.T) {
	// Render WITHOUT WithComponents — the interpreter must emit a
	// clear marker rather than panic or render blank.
	e := New()
	_ = e.Register("UserCard", []byte(userCardTmpl), func() Component { return &userCard{} })
	_ = e.Register("Parent", []byte(`<template><UserCard /></template>`),
		func() Component { return &dummyComponent{} })
	def, _ := e.lookup("Parent")

	html := Render(def.fragment, &dummyComponent{}).HTML() // no WithComponents
	if !strings.Contains(html, "[!err") || !strings.Contains(html, "no component resolver") {
		t.Errorf("expected no-resolver marker; got %q", html)
	}
}

// --- multi-level nesting --------------------------------------------

type label struct {
	BaseComponent
	Text string
}

func TestComponent_MultiLevelNesting(t *testing.T) {
	// outer → mid → inner. Each layer passes a prop down.
	e := New()
	_ = e.Register("Label", []byte(`<template><span>{{ Text }}</span></template>`),
		func() Component { return &label{} })
	_ = e.Register("Wrapper", []byte(`<template><div><Label :text="T" /></div></template>`),
		func() Component { return &struct {
			BaseComponent
			T string
		}{} })
	_ = e.Register("Outer", []byte(`<template><Wrapper :t="Greeting" /></template>`),
		func() Component { return &struct {
			BaseComponent
			T string
		}{} })

	def, _ := e.lookup("Outer")
	html := Render(def.fragment, struct{ Greeting string }{Greeting: "hi"}, WithComponents(e)).HTML()
	if html != `<div><span>hi</span></div>` {
		t.Errorf("got %q want <div><span>hi</span></div>", html)
	}
}
