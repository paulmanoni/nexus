package template

import (
	"strings"
	"testing"
)

// lowerSrc parses src as a complete SFC and returns the lowered
// Fragment, failing the test if either step errors.
func lowerSrc(t *testing.T, src string) *Fragment {
	t.Helper()
	f, err := Parse("test.nlt", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	frag, err := Lower(f.Template)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	return frag
}

func lowerErr(t *testing.T, src string) *ParseError {
	t.Helper()
	f, err := Parse("test.nlt", []byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = Lower(f.Template)
	if err == nil {
		t.Fatalf("expected lower error, got nil")
	}
	pe, ok := err.(*ParseError)
	if !ok {
		t.Fatalf("expected *ParseError, got %T: %v", err, err)
	}
	return pe
}

// --- invariants -----------------------------------------------------

func assertStaticsSlotsInvariant(t *testing.T, f *Fragment) {
	t.Helper()
	if len(f.Statics) != len(f.Slots)+1 {
		t.Errorf("invariant violated: len(Statics)=%d len(Slots)=%d (expected statics = slots+1)",
			len(f.Statics), len(f.Slots))
	}
}

func TestLower_EmptyTemplate(t *testing.T) {
	frag := lowerSrc(t, `<template></template>`)
	assertStaticsSlotsInvariant(t, frag)
	if len(frag.Statics) != 1 || frag.Statics[0] != "" {
		t.Errorf("empty template should produce {Statics:[\"\"], Slots:[]}; got %+v", frag)
	}
}

func TestLower_PureStatic(t *testing.T) {
	frag := lowerSrc(t, `<template><p class="x">hello world</p></template>`)
	assertStaticsSlotsInvariant(t, frag)
	if len(frag.Slots) != 0 {
		t.Errorf("expected no slots; got %d", len(frag.Slots))
	}
	joined := strings.Join(frag.Statics, "")
	if joined != `<p class="x">hello world</p>` {
		t.Errorf("joined statics = %q", joined)
	}
}

// --- interpolation --------------------------------------------------

func TestLower_TextInterpolation(t *testing.T) {
	frag := lowerSrc(t, `<template><p>Hello {{ name }}!</p></template>`)
	assertStaticsSlotsInvariant(t, frag)
	if len(frag.Slots) != 1 {
		t.Fatalf("expected 1 slot; got %d", len(frag.Slots))
	}
	es, ok := frag.Slots[0].(ExprSlot)
	if !ok {
		t.Fatalf("expected ExprSlot; got %T", frag.Slots[0])
	}
	if es.Expr != "name" {
		t.Errorf("expr = %q want %q", es.Expr, "name")
	}
	if frag.Statics[0] != "<p>Hello " || frag.Statics[1] != "!</p>" {
		t.Errorf("statics = %#v", frag.Statics)
	}
}

func TestLower_MultipleInterpolationsInOneText(t *testing.T) {
	frag := lowerSrc(t, `<template><p>{{ a }}{{ b }}</p></template>`)
	assertStaticsSlotsInvariant(t, frag)
	if len(frag.Slots) != 2 {
		t.Fatalf("expected 2 slots; got %d", len(frag.Slots))
	}
	// Empty static between the two adjacent interpolations.
	if frag.Statics[1] != "" {
		t.Errorf("middle static should be empty; got %q", frag.Statics[1])
	}
}

// --- attribute bindings ---------------------------------------------

func TestLower_BindAttr(t *testing.T) {
	frag := lowerSrc(t, `<template><img :src="url"></template>`)
	assertStaticsSlotsInvariant(t, frag)
	if len(frag.Slots) != 1 {
		t.Fatalf("expected 1 slot; got %d", len(frag.Slots))
	}
	es := frag.Slots[0].(ExprSlot)
	if es.Expr != "url" {
		t.Errorf("expr = %q", es.Expr)
	}
	if !strings.HasSuffix(frag.Statics[0], `src="`) {
		t.Errorf("first static should end with src=\"; got %q", frag.Statics[0])
	}
	if !strings.HasPrefix(frag.Statics[1], `"`) {
		t.Errorf("second static should start with quote; got %q", frag.Statics[1])
	}
}

func TestLower_PlainAttr(t *testing.T) {
	frag := lowerSrc(t, `<template><a href="/home" target="_blank">x</a></template>`)
	assertStaticsSlotsInvariant(t, frag)
	if len(frag.Slots) != 0 {
		t.Errorf("plain attrs should not produce slots; got %d", len(frag.Slots))
	}
	joined := strings.Join(frag.Statics, "")
	if !strings.Contains(joined, `href="/home"`) || !strings.Contains(joined, `target="_blank"`) {
		t.Errorf("missing plain attrs in %q", joined)
	}
}

func TestLower_OnAttrFlowsToStaticAsMarker(t *testing.T) {
	frag := lowerSrc(t, `<template><button @click="like">x</button></template>`)
	assertStaticsSlotsInvariant(t, frag)
	if len(frag.Slots) != 0 {
		t.Errorf("@on should not produce slots; got %d", len(frag.Slots))
	}
	joined := strings.Join(frag.Statics, "")
	if !strings.Contains(joined, `nl-on:click="like"`) {
		t.Errorf("expected nl-on:click marker in static; got %q", joined)
	}
}

func TestLower_OnAttrModifiers(t *testing.T) {
	frag := lowerSrc(t, `<template><button @click.prevent.stop="x">y</button></template>`)
	joined := strings.Join(frag.Statics, "")
	if !strings.Contains(joined, `nl-on:click.prevent.stop="x"`) {
		t.Errorf("expected modifiers in marker; got %q", joined)
	}
}

// --- nl-if chain ----------------------------------------------------

func TestLower_IfOnly(t *testing.T) {
	frag := lowerSrc(t, `<template><p nl-if="show">visible</p></template>`)
	assertStaticsSlotsInvariant(t, frag)
	if len(frag.Slots) != 1 {
		t.Fatalf("expected 1 slot; got %d", len(frag.Slots))
	}
	bs, ok := frag.Slots[0].(BranchSlot)
	if !ok {
		t.Fatalf("expected BranchSlot; got %T", frag.Slots[0])
	}
	if len(bs.Branches) != 1 {
		t.Fatalf("expected 1 branch; got %d", len(bs.Branches))
	}
	if bs.Branches[0].Cond != "show" {
		t.Errorf("cond = %q", bs.Branches[0].Cond)
	}
	body := bs.Branches[0].Fragment
	if joined := strings.Join(body.Statics, ""); joined != "<p>visible</p>" {
		t.Errorf("body statics = %q", joined)
	}
}

func TestLower_IfElse(t *testing.T) {
	src := `<template><p nl-if="a">A</p><p nl-else>B</p></template>`
	frag := lowerSrc(t, src)
	bs := frag.Slots[0].(BranchSlot)
	if len(bs.Branches) != 2 {
		t.Fatalf("want 2 branches; got %d", len(bs.Branches))
	}
	if bs.Branches[0].Cond != "a" {
		t.Errorf("if cond = %q", bs.Branches[0].Cond)
	}
	if bs.Branches[1].Cond != "" {
		t.Errorf("else cond should be empty (marker); got %q", bs.Branches[1].Cond)
	}
}

func TestLower_IfElseIfElse(t *testing.T) {
	src := `<template>
		<p nl-if="a">A</p>
		<p nl-else-if="b">B</p>
		<p nl-else>C</p>
	</template>`
	frag := lowerSrc(t, src)
	// Whitespace between branches becomes a leading TextNode at the
	// top level — find the BranchSlot among the slots.
	var bs BranchSlot
	for _, s := range frag.Slots {
		if b, ok := s.(BranchSlot); ok {
			bs = b
			break
		}
	}
	if len(bs.Branches) != 3 {
		t.Fatalf("want 3 branches; got %d", len(bs.Branches))
	}
	if bs.Branches[0].Cond != "a" || bs.Branches[1].Cond != "b" || bs.Branches[2].Cond != "" {
		t.Errorf("branch conds = [%q, %q, %q]",
			bs.Branches[0].Cond, bs.Branches[1].Cond, bs.Branches[2].Cond)
	}
}

func TestLower_ElseWithoutIfIsError(t *testing.T) {
	pe := lowerErr(t, `<template><p nl-else>orphan</p></template>`)
	if !strings.Contains(pe.Msg, "nl-else") || !strings.Contains(pe.Msg, "no preceding") {
		t.Errorf("msg = %q", pe.Msg)
	}
}

func TestLower_ElseIfWithoutIfIsError(t *testing.T) {
	pe := lowerErr(t, `<template><p nl-else-if="x">orphan</p></template>`)
	if !strings.Contains(pe.Msg, "no preceding") {
		t.Errorf("msg = %q", pe.Msg)
	}
}

// --- nl-for ---------------------------------------------------------

func TestLower_LoopBasic(t *testing.T) {
	src := `<template><li nl-for="p in posts" :key="p.ID">{{ p.Title }}</li></template>`
	frag := lowerSrc(t, src)
	if len(frag.Slots) != 1 {
		t.Fatalf("want 1 slot; got %d", len(frag.Slots))
	}
	ls, ok := frag.Slots[0].(LoopSlot)
	if !ok {
		t.Fatalf("want LoopSlot; got %T", frag.Slots[0])
	}
	if ls.Var != "p" || ls.Iter != "posts" || ls.KeyExpr != "p.ID" {
		t.Errorf("loop = %+v", ls)
	}
	// Body should be the <li>...</li>.
	body := ls.Body
	if joined := strings.Join(body.Statics, ""); !strings.HasPrefix(joined, "<li>") || !strings.HasSuffix(joined, "</li>") {
		t.Errorf("body statics not wrapped in <li></li>: %q", joined)
	}
	if len(body.Slots) != 1 {
		t.Errorf("body should have 1 slot for {{ p.Title }}; got %d", len(body.Slots))
	}
}

func TestLower_LoopOnTemplateWrapper(t *testing.T) {
	// <template nl-for> renders the body without an enclosing element.
	src := `<template><template nl-for="x in xs" :key="x">{{ x }}</template></template>`
	frag := lowerSrc(t, src)
	ls := frag.Slots[0].(LoopSlot)
	body := ls.Body
	if joined := strings.Join(body.Statics, ""); strings.Contains(joined, "<template>") {
		t.Errorf("template wrapper should NOT appear in body statics; got %q", joined)
	}
}

func TestLower_LoopAndIfOnSameElementIsError(t *testing.T) {
	pe := lowerErr(t, `<template><p nl-for="x in xs" nl-if="show" :key="x">{{ x }}</p></template>`)
	if !strings.Contains(pe.Msg, "cannot coexist") {
		t.Errorf("msg = %q", pe.Msg)
	}
}

func TestLower_LoopMalformedExprIsError(t *testing.T) {
	pe := lowerErr(t, `<template><li nl-for="not a valid expression" :key="x">x</li></template>`)
	if !strings.Contains(pe.Msg, "nl-for") {
		t.Errorf("msg = %q", pe.Msg)
	}
}

// --- components -----------------------------------------------------

func TestLower_ComponentRef(t *testing.T) {
	src := `<template><UserCard :user="u" compact="true" /></template>`
	frag := lowerSrc(t, src)
	if len(frag.Slots) != 1 {
		t.Fatalf("want 1 slot; got %d", len(frag.Slots))
	}
	cs, ok := frag.Slots[0].(ComponentSlot)
	if !ok {
		t.Fatalf("want ComponentSlot; got %T", frag.Slots[0])
	}
	if cs.Name != "UserCard" {
		t.Errorf("name = %q", cs.Name)
	}
	if len(cs.Props) != 2 {
		t.Fatalf("want 2 props; got %d", len(cs.Props))
	}
	var userProp, compactProp ComponentProp
	for _, p := range cs.Props {
		switch p.Name {
		case "user":
			userProp = p
		case "compact":
			compactProp = p
		}
	}
	if !userProp.IsBind || userProp.Value != "u" {
		t.Errorf("user prop = %+v", userProp)
	}
	if compactProp.IsBind || compactProp.Value != "true" {
		t.Errorf("compact prop = %+v", compactProp)
	}
}

func TestLower_ComponentWithChildren(t *testing.T) {
	src := `<template><Modal><h1>Title</h1></Modal></template>`
	frag := lowerSrc(t, src)
	cs := frag.Slots[0].(ComponentSlot)
	if cs.Children == nil {
		t.Fatal("Children should be non-nil")
	}
	joined := strings.Join(cs.Children.Statics, "")
	if !strings.Contains(joined, "<h1>Title</h1>") {
		t.Errorf("Children statics = %q", joined)
	}
}

// --- unsupported directives -----------------------------------------

func TestLower_UnsupportedDirectives(t *testing.T) {
	cases := []struct{ src, want string }{
		{`<template><input nl-model="x"></template>`, "nl-model"},
		{`<template><div nl-show="x">y</div></template>`, "nl-show"},
		{`<template><div nl-html="x">y</div></template>`, "nl-html"},
		{`<template><div nl-text="x">y</div></template>`, "nl-text"},
		{`<template><div nl-pre>y</div></template>`, "nl-pre"},
	}
	for _, c := range cases {
		pe := lowerErr(t, c.src)
		if !strings.Contains(pe.Msg, c.want) {
			t.Errorf("src %q: msg = %q (want substring %q)", c.src, pe.Msg, c.want)
		}
	}
}

func TestLower_OnceIsSilentlyAccepted(t *testing.T) {
	// nl-once is a render-time perf hint and is safe to no-op in v1.
	frag := lowerSrc(t, `<template><p nl-once>hi</p></template>`)
	assertStaticsSlotsInvariant(t, frag)
	if len(frag.Slots) != 0 {
		t.Errorf("nl-once should produce no slots; got %d", len(frag.Slots))
	}
}

// --- void + self-closing --------------------------------------------

func TestLower_VoidElement(t *testing.T) {
	frag := lowerSrc(t, `<template><div><br>after</div></template>`)
	joined := strings.Join(frag.Statics, "")
	if !strings.Contains(joined, "<br />") {
		t.Errorf("expected <br /> in output; got %q", joined)
	}
	if !strings.Contains(joined, "after") {
		t.Errorf("expected trailing text; got %q", joined)
	}
}

// --- end-to-end smoke -----------------------------------------------

func TestLower_FullExample(t *testing.T) {
	src := `<template>
  <div class="posts">
    <input :value="filter" @input="filter" placeholder="Search…" />
    <article nl-for="post in posts" :key="post.ID" :id="post.ID">
      <h2>{{ post.Title }}</h2>
      <button @click="like">{{ post.Likes }} likes</button>
    </article>
    <p nl-if="empty">No matches</p>
  </div>
</template>`
	frag := lowerSrc(t, src)
	assertStaticsSlotsInvariant(t, frag)
	// We expect at least:
	//   - one ExprSlot for :value="filter"
	//   - one LoopSlot for the article
	//   - one BranchSlot for the nl-if
	var sawExpr, sawLoop, sawBranch bool
	var walk func(*Fragment)
	walk = func(f *Fragment) {
		for _, s := range f.Slots {
			switch v := s.(type) {
			case ExprSlot:
				sawExpr = true
			case LoopSlot:
				sawLoop = true
				if v.Body != nil {
					walk(v.Body)
				}
			case BranchSlot:
				sawBranch = true
				for _, br := range v.Branches {
					if br.Fragment != nil {
						walk(br.Fragment)
					}
				}
			case ComponentSlot:
				if v.Children != nil {
					walk(v.Children)
				}
			}
		}
	}
	walk(frag)
	if !sawExpr || !sawLoop || !sawBranch {
		t.Errorf("smoke: sawExpr=%v sawLoop=%v sawBranch=%v", sawExpr, sawLoop, sawBranch)
	}
}
