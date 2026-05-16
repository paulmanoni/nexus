package template

import (
	"strings"
	"testing"
)

// mustParse is the happy-path helper: parses src and fails the test
// if parsing errored. Use parseErr below for cases that should fail.
func mustParse(t *testing.T, src string) *File {
	t.Helper()
	f, err := Parse("test.nlt", []byte(src))
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	return f
}

func parseErr(t *testing.T, src string) *ParseError {
	t.Helper()
	_, err := Parse("test.nlt", []byte(src))
	if err == nil {
		t.Fatalf("expected parse error, got nil")
	}
	pe, ok := err.(*ParseError)
	if !ok {
		t.Fatalf("expected *ParseError, got %T: %v", err, err)
	}
	return pe
}

// --- SFC split ------------------------------------------------------

func TestSFC_TemplateOnly(t *testing.T) {
	f := mustParse(t, `<template><p>hi</p></template>`)
	if f.Template == nil {
		t.Fatal("template nil")
	}
	if f.Script != nil || f.Style != nil {
		t.Fatal("script/style should be nil")
	}
	if len(f.Template.Children) != 1 {
		t.Fatalf("want 1 root, got %d", len(f.Template.Children))
	}
}

func TestSFC_AllThreeBlocks(t *testing.T) {
	src := `<template><p>hi</p></template>
<script lang="go">type State struct{}</script>
<style scoped>p { color: red; }</style>`
	f := mustParse(t, src)
	if f.Template == nil || f.Script == nil || f.Style == nil {
		t.Fatalf("expected all three blocks: %+v %+v %+v", f.Template, f.Script, f.Style)
	}
	if f.Script.Lang != "go" {
		t.Errorf("Script.Lang = %q want go", f.Script.Lang)
	}
	if !strings.Contains(f.Script.Body, "type State struct") {
		t.Errorf("Script.Body = %q", f.Script.Body)
	}
	if !f.Style.Scoped {
		t.Error("Style.Scoped should be true")
	}
	if !strings.Contains(f.Style.Body, "color: red") {
		t.Errorf("Style.Body = %q", f.Style.Body)
	}
}

func TestSFC_BlocksInAnyOrder(t *testing.T) {
	src := `<script lang="go">x</script>
<style>y</style>
<template>z</template>`
	f := mustParse(t, src)
	if f.Template == nil || f.Script == nil || f.Style == nil {
		t.Fatal("expected all three blocks")
	}
	if f.Style.Scoped {
		t.Error("Style.Scoped should default false")
	}
	if f.Style.Lang != "css" {
		t.Errorf("Style.Lang default should be 'css'; got %q", f.Style.Lang)
	}
}

func TestSFC_MissingTemplateIsError(t *testing.T) {
	pe := parseErr(t, `<script>x</script>`)
	if !strings.Contains(pe.Msg, "missing <template>") {
		t.Errorf("msg = %q", pe.Msg)
	}
}

func TestSFC_DuplicateBlockIsError(t *testing.T) {
	pe := parseErr(t, `<template>a</template><template>b</template>`)
	if !strings.Contains(pe.Msg, "duplicate <template>") {
		t.Errorf("msg = %q", pe.Msg)
	}
}

func TestSFC_TopLevelGarbageIsError(t *testing.T) {
	pe := parseErr(t, `<template>x</template><div>nope</div>`)
	if !strings.Contains(pe.Msg, "unexpected top-level") {
		t.Errorf("msg = %q", pe.Msg)
	}
}

func TestSFC_StrayTextBetweenBlocksIsError(t *testing.T) {
	pe := parseErr(t, "<template>x</template>\nstray text\n<script>y</script>")
	if !strings.Contains(pe.Msg, "unexpected text") {
		t.Errorf("msg = %q", pe.Msg)
	}
}

func TestSFC_NestedTemplateDoesNotClosePrematurely(t *testing.T) {
	// Inner <template> is a slot wrapper, not the SFC boundary.
	src := `<template><div><template nl-if="x"><span>a</span></template></div></template>`
	f := mustParse(t, src)
	if len(f.Template.Children) != 1 {
		t.Fatalf("expected 1 root, got %d", len(f.Template.Children))
	}
	root := f.Template.Children[0].(*ElementNode)
	if root.Tag != "div" {
		t.Fatalf("root tag = %q want div", root.Tag)
	}
}

func TestSFC_StyleScopedFlag(t *testing.T) {
	f := mustParse(t, `<template>x</template><style scoped lang="scss">y</style>`)
	if !f.Style.Scoped {
		t.Error("Scoped should be true")
	}
	if f.Style.Lang != "scss" {
		t.Errorf("Lang = %q want scss", f.Style.Lang)
	}
}

// --- Interpolation --------------------------------------------------

func TestInterpolation_Single(t *testing.T) {
	f := mustParse(t, `<template><p>Hello {{ name }}</p></template>`)
	p := f.Template.Children[0].(*ElementNode)
	if len(p.Children) != 2 {
		t.Fatalf("want 2 children (text + interp), got %d", len(p.Children))
	}
	if txt, ok := p.Children[0].(*TextNode); !ok || txt.Text != "Hello " {
		t.Errorf("children[0] = %#v", p.Children[0])
	}
	if interp, ok := p.Children[1].(*InterpolationNode); !ok || interp.Expr != "name" {
		t.Errorf("children[1] = %#v", p.Children[1])
	}
}

func TestInterpolation_MultipleInOneText(t *testing.T) {
	f := mustParse(t, `<template><p>{{ a }} and {{ b }}!</p></template>`)
	p := f.Template.Children[0].(*ElementNode)
	if len(p.Children) != 4 {
		t.Fatalf("want 4 children (interp, text, interp, text), got %d:\n%#v", len(p.Children), p.Children)
	}
	mustExpr := func(n Node, want string) {
		t.Helper()
		i, ok := n.(*InterpolationNode)
		if !ok {
			t.Fatalf("want interpolation, got %T", n)
		}
		if i.Expr != want {
			t.Errorf("expr = %q want %q", i.Expr, want)
		}
	}
	mustText := func(n Node, want string) {
		t.Helper()
		x, ok := n.(*TextNode)
		if !ok {
			t.Fatalf("want text, got %T", n)
		}
		if x.Text != want {
			t.Errorf("text = %q want %q", x.Text, want)
		}
	}
	mustExpr(p.Children[0], "a")
	mustText(p.Children[1], " and ")
	mustExpr(p.Children[2], "b")
	mustText(p.Children[3], "!")
}

func TestInterpolation_TrimsWhitespace(t *testing.T) {
	f := mustParse(t, `<template><p>{{   spaced   }}</p></template>`)
	p := f.Template.Children[0].(*ElementNode)
	interp := p.Children[0].(*InterpolationNode)
	if interp.Expr != "spaced" {
		t.Errorf("expr = %q want %q", interp.Expr, "spaced")
	}
}

func TestInterpolation_UnterminatedIsError(t *testing.T) {
	pe := parseErr(t, `<template><p>{{ broken </p></template>`)
	if !strings.Contains(pe.Msg, "unterminated") {
		t.Errorf("msg = %q", pe.Msg)
	}
}

func TestInterpolation_EmptyIsError(t *testing.T) {
	pe := parseErr(t, `<template><p>{{ }}</p></template>`)
	if !strings.Contains(pe.Msg, "empty interpolation") {
		t.Errorf("msg = %q", pe.Msg)
	}
}

// --- Attribute classification ---------------------------------------

func findAttr(elem *ElementNode, raw string) *Attribute {
	for i := range elem.Attrs {
		if elem.Attrs[i].Raw == raw {
			return &elem.Attrs[i]
		}
	}
	return nil
}

func TestAttr_Plain(t *testing.T) {
	f := mustParse(t, `<template><p class="x" id="y">hi</p></template>`)
	p := f.Template.Children[0].(*ElementNode)
	a := findAttr(p, "class")
	if a == nil || a.Kind != AttrPlain || a.Name != "class" || a.Value != "x" {
		t.Errorf("class attr = %+v", a)
	}
	if b := findAttr(p, "id"); b == nil || b.Value != "y" {
		t.Errorf("id attr = %+v", b)
	}
}

func TestAttr_BindShorthand(t *testing.T) {
	f := mustParse(t, `<template><img :src="url"></template>`)
	img := f.Template.Children[0].(*ElementNode)
	a := findAttr(img, ":src")
	if a == nil || a.Kind != AttrBind || a.Name != "src" || a.Value != "url" {
		t.Errorf("attr = %+v", a)
	}
}

func TestAttr_BindLongForm(t *testing.T) {
	f := mustParse(t, `<template><img nl-bind:src="url"></template>`)
	img := f.Template.Children[0].(*ElementNode)
	a := findAttr(img, "nl-bind:src")
	if a == nil || a.Kind != AttrBind || a.Name != "src" || a.Value != "url" {
		t.Errorf("attr = %+v", a)
	}
}

func TestAttr_OnShorthand(t *testing.T) {
	f := mustParse(t, `<template><button @click="like">x</button></template>`)
	btn := f.Template.Children[0].(*ElementNode)
	a := findAttr(btn, "@click")
	if a == nil || a.Kind != AttrOn || a.Name != "click" || a.Value != "like" {
		t.Errorf("attr = %+v", a)
	}
}

func TestAttr_OnWithModifiers(t *testing.T) {
	f := mustParse(t, `<template><button @click.prevent.stop="x">y</button></template>`)
	btn := f.Template.Children[0].(*ElementNode)
	a := findAttr(btn, "@click.prevent.stop")
	if a == nil {
		t.Fatal("attr missing")
	}
	if a.Kind != AttrOn || a.Name != "click" {
		t.Errorf("kind/name = %v/%q", a.Kind, a.Name)
	}
	want := []string{"prevent", "stop"}
	if len(a.Modifiers) != 2 || a.Modifiers[0] != want[0] || a.Modifiers[1] != want[1] {
		t.Errorf("modifiers = %v want %v", a.Modifiers, want)
	}
}

func TestAttr_DirectiveIf(t *testing.T) {
	f := mustParse(t, `<template><p nl-if="show">hi</p></template>`)
	p := f.Template.Children[0].(*ElementNode)
	a := findAttr(p, "nl-if")
	if a == nil || a.Kind != AttrDirective || a.Name != "if" || a.Value != "show" {
		t.Errorf("attr = %+v", a)
	}
}

func TestAttr_DirectiveFor(t *testing.T) {
	f := mustParse(t, `<template><li nl-for="x in xs" :key="x.ID">{{ x.Name }}</li></template>`)
	li := f.Template.Children[0].(*ElementNode)
	a := findAttr(li, "nl-for")
	if a == nil || a.Value != "x in xs" {
		t.Errorf("nl-for attr = %+v", a)
	}
	k := findAttr(li, ":key")
	if k == nil || k.Kind != AttrBind || k.Name != "key" {
		t.Errorf(":key attr = %+v", k)
	}
}

func TestAttr_DirectiveModelWithModifiers(t *testing.T) {
	f := mustParse(t, `<template><input nl-model.lazy.number="age"></template>`)
	in := f.Template.Children[0].(*ElementNode)
	a := findAttr(in, "nl-model.lazy.number")
	if a == nil || a.Kind != AttrDirective || a.Name != "model" {
		t.Errorf("attr = %+v", a)
	}
	if len(a.Modifiers) != 2 || a.Modifiers[0] != "lazy" || a.Modifiers[1] != "number" {
		t.Errorf("modifiers = %v", a.Modifiers)
	}
}

func TestAttr_SlotShorthand(t *testing.T) {
	f := mustParse(t, `<template><Card><template #header>hi</template></Card></template>`)
	card := f.Template.Children[0].(*ElementNode)
	tmpl := card.Children[0].(*ElementNode)
	a := findAttr(tmpl, "#header")
	if a == nil || a.Kind != AttrDirective || a.Name != "slot" || a.Arg != "header" {
		t.Errorf("slot attr = %+v", a)
	}
}

func TestAttr_SlotLongForm(t *testing.T) {
	f := mustParse(t, `<template><Card><template nl-slot:footer>x</template></Card></template>`)
	card := f.Template.Children[0].(*ElementNode)
	tmpl := card.Children[0].(*ElementNode)
	a := findAttr(tmpl, "nl-slot:footer")
	if a == nil || a.Name != "slot" || a.Arg != "footer" {
		t.Errorf("slot attr = %+v", a)
	}
}

func TestAttr_UnknownDirectiveSuggestsClose(t *testing.T) {
	pe := parseErr(t, `<template><p nl-iff="x">hi</p></template>`)
	if !strings.Contains(pe.Msg, "unknown directive") {
		t.Errorf("msg = %q", pe.Msg)
	}
	if !strings.Contains(pe.Msg, "did you mean 'nl-if'") {
		t.Errorf("expected did-you-mean suggestion; msg = %q", pe.Msg)
	}
}

func TestAttr_UnknownDirectiveNoSuggestionForGarbage(t *testing.T) {
	pe := parseErr(t, `<template><p nl-xyzzy="x">hi</p></template>`)
	if !strings.Contains(pe.Msg, "unknown directive") {
		t.Errorf("msg = %q", pe.Msg)
	}
	if strings.Contains(pe.Msg, "did you mean") {
		t.Errorf("should NOT suggest for unrelated input; msg = %q", pe.Msg)
	}
}

// --- Elements / structure -------------------------------------------

func TestElement_Nested(t *testing.T) {
	f := mustParse(t, `<template><div><p><span>x</span></p></div></template>`)
	div := f.Template.Children[0].(*ElementNode)
	p := div.Children[0].(*ElementNode)
	span := p.Children[0].(*ElementNode)
	if span.Tag != "span" {
		t.Errorf("span.Tag = %q", span.Tag)
	}
}

func TestElement_SelfClosing(t *testing.T) {
	f := mustParse(t, `<template><div><Foo /></div></template>`)
	div := f.Template.Children[0].(*ElementNode)
	foo := div.Children[0].(*ElementNode)
	if !foo.SelfClosing {
		t.Error("Foo should be SelfClosing")
	}
	if !foo.IsComponent {
		t.Error("Foo should be IsComponent (PascalCase)")
	}
}

func TestElement_VoidElementDoesNotPushStack(t *testing.T) {
	// <br> without explicit close should not break depth counting.
	f := mustParse(t, `<template><div><br>after</div></template>`)
	div := f.Template.Children[0].(*ElementNode)
	if len(div.Children) != 2 {
		t.Fatalf("want 2 children (br + text); got %d: %#v", len(div.Children), div.Children)
	}
	if br := div.Children[0].(*ElementNode); strings.ToLower(br.Tag) != "br" {
		t.Errorf("first child = %+v", br)
	}
	if txt := div.Children[1].(*TextNode); txt.Text != "after" {
		t.Errorf("second child text = %q", txt.Text)
	}
}

func TestElement_ComponentTagPreservesCase(t *testing.T) {
	f := mustParse(t, `<template><UserCard :user="u" /></template>`)
	uc := f.Template.Children[0].(*ElementNode)
	if uc.Tag != "UserCard" {
		t.Errorf("Tag = %q want UserCard (case must survive)", uc.Tag)
	}
	if !uc.IsComponent {
		t.Error("IsComponent should be true")
	}
}

func TestElement_TemplateTagIsNotComponent(t *testing.T) {
	f := mustParse(t, `<template><template nl-if="x">hi</template></template>`)
	outer := f.Template.Children[0].(*ElementNode)
	if outer.IsComponent {
		t.Error("inner <template> should NOT be a component (it's the slot/group wrapper)")
	}
}

func TestElement_UnclosedIsError(t *testing.T) {
	pe := parseErr(t, `<template><div><p>oops</template>`)
	// html.Tokenizer may close <p> implicitly; we still expect SOME error
	// when the user clearly forgot a close tag. The exact message varies,
	// but the position should point inside the template.
	if pe.Pos.Filename != "test.nlt" {
		t.Errorf("pos filename = %q", pe.Pos.Filename)
	}
}

func TestElement_MismatchedCloseIsError(t *testing.T) {
	pe := parseErr(t, `<template><div></span></div></template>`)
	if !strings.Contains(pe.Msg, "no matching open") && !strings.Contains(pe.Msg, "mismatched") {
		t.Errorf("msg = %q", pe.Msg)
	}
}

// --- Position tracking ----------------------------------------------

func TestPosition_LineColAttributed(t *testing.T) {
	src := "<template>\n  <p>{{ broken </p>\n</template>"
	pe := parseErr(t, src)
	if pe.Pos.Line != 2 {
		t.Errorf("line = %d want 2", pe.Pos.Line)
	}
	if pe.Pos.Filename != "test.nlt" {
		t.Errorf("file = %q", pe.Pos.Filename)
	}
}

func TestPosition_NodesCarryPosition(t *testing.T) {
	src := "<template>\n  <p>hi</p>\n</template>"
	f := mustParse(t, src)
	p := f.Template.Children[1].(*ElementNode) // children[0] is "\n  " whitespace TextNode
	if p.Position.Line != 2 {
		t.Errorf("p line = %d want 2", p.Position.Line)
	}
}

// --- Integration example --------------------------------------------

func TestParse_FullExampleSmoke(t *testing.T) {
	// The motivating example from the design doc, slightly trimmed.
	src := `<template>
  <div class="posts">
    <input :value="filter" @input="filter" placeholder="Search…" />

    <article nl-for="post in posts" :key="post.ID" :id="` + "`post-${post.ID}`" + `">
      <h2>{{ titlecase(post.Title) }}</h2>
      <button @click.prevent="like" :data-id="post.ID">
        {{ post.Likes }} likes
      </button>
    </article>

    <p nl-if="posts.length === 0">No posts match "{{ filter }}".</p>

    <UserCard nl-for="u in topUsers" :key="u.ID" :user="u" />
  </div>
</template>

<script lang="go">
type State struct {
    Posts    []Post
    TopUsers []User
    Filter   string
}
</script>

<style scoped>
.posts article { padding: 1rem; }
</style>`

	f := mustParse(t, src)
	if f.Template == nil || f.Script == nil || f.Style == nil {
		t.Fatal("expected all three blocks")
	}
	if !strings.Contains(f.Script.Body, "type State struct") {
		t.Error("script body missing")
	}
	if !f.Style.Scoped {
		t.Error("style scoped flag not set")
	}

	// Walk to the article: template > div > [whitespace, input, ws, article, ws, p, ws, UserCard, ws]
	div := f.Template.Children[1].(*ElementNode) // [0] is leading "\n  " whitespace
	if div.Tag != "div" {
		t.Fatalf("expected div root inside template; got %s", div.Tag)
	}
	var article, userCard *ElementNode
	for _, c := range div.Children {
		if el, ok := c.(*ElementNode); ok {
			switch el.Tag {
			case "article":
				article = el
			case "UserCard":
				userCard = el
			}
		}
	}
	if article == nil {
		t.Fatal("article element not found")
	}
	if userCard == nil {
		t.Fatal("UserCard element not found")
	}
	if !userCard.IsComponent {
		t.Error("UserCard should be IsComponent")
	}
	if a := findAttr(article, "nl-for"); a == nil || a.Value != "post in posts" {
		t.Errorf("article nl-for = %+v", a)
	}
}
