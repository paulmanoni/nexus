package template

import "testing"

func TestRewriteScopedCSS_TopLevelSelectors(t *testing.T) {
	body := `.posts { color: red; } .posts h1 { font-size: 2rem; }`
	got := rewriteScopedCSS(body, "abc12345")
	want := `[data-nl-scope="abc12345"] .posts { color: red; } [data-nl-scope="abc12345"] .posts h1 { font-size: 2rem; }`
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestRewriteScopedCSS_CommaList(t *testing.T) {
	got := rewriteScopedCSS(`h1, h2, h3 { margin: 0; }`, "X")
	want := `[data-nl-scope="X"] h1, [data-nl-scope="X"] h2, [data-nl-scope="X"] h3 { margin: 0; }`
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestRewriteScopedCSS_AtRulePassesThrough(t *testing.T) {
	// @-rules at the top level (e.g. @import, @font-face) pass
	// through verbatim — they don't take an element-matching
	// selector. The body of @media containing nested selectors
	// is the documented limitation (not scoped). We're just
	// asserting we don't break @-rules here.
	got := rewriteScopedCSS(`@font-face { font-family: x; }`, "X")
	want := `@font-face { font-family: x; }`
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestRewriteScopedCSS_Empty(t *testing.T) {
	if got := rewriteScopedCSS("", "X"); got != "" {
		t.Errorf("empty body should pass through; got %q", got)
	}
}

func TestComputeScopeID_StableAndShort(t *testing.T) {
	a := computeScopeID("Posts")
	b := computeScopeID("Posts")
	c := computeScopeID("PostsList")
	if a != b {
		t.Errorf("scope ID not stable: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("different components produced same scope ID")
	}
	if len(a) != 8 {
		t.Errorf("scope ID length = %d, want 8", len(a))
	}
}
