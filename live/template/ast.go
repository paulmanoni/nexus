package template

import "fmt"

// Position records where a node or token originated in source. Filled
// in by the parser for every AST node so error messages and runtime
// diagnostics can point at the exact line/column the author wrote.
type Position struct {
	Filename string
	Line     int // 1-indexed
	Col      int // 1-indexed; counts runes for ASCII, bytes otherwise (good enough)
	Offset   int // 0-indexed byte offset into source
}

func (p Position) String() string {
	if p.Filename == "" {
		return fmt.Sprintf("%d:%d", p.Line, p.Col)
	}
	return fmt.Sprintf("%s:%d:%d", p.Filename, p.Line, p.Col)
}

// ParseError is the single error type returned by Parse / ParseFile.
// Wraps a Position so callers can format with file:line:col context.
type ParseError struct {
	Pos Position
	Msg string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("%s: %s", e.Pos, e.Msg)
}

// File is a fully-parsed .nlt single-file component. Template is
// required; Script and Style are nil when absent.
type File struct {
	Filename string
	Template *Template
	Script   *Script
	Style    *Style
}

// Template is the parsed <template> block. Vue 3-style multi-root
// fragments are allowed; Children holds them in document order.
type Template struct {
	Children []Node
	Pos      Position
}

// Script is the verbatim <script> block. The body is shipped to
// the browser as-is, wrapped per the Scoped flag below. We do not
// transpile or analyze the body — what you write is what runs.
//
// Scoped, when true, makes the renderer wrap the body in an IIFE
// that binds `el` to this component's SSR root element
// (data-nl-component="<Name>"). Selectors that look up children
// then go through el.querySelector instead of document, so two
// instances of the same component on a page don't collide. The
// IIFE re-runs on every live-navigate INTO this component, with
// `el` re-bound to the new root, so listeners attached to the
// previous DOM die with it (no manual cleanup needed).
//
// Lang is the value of the lang= attribute on the open tag.
// Defaults to "js"; carried through unused for now but reserved
// for a future TS-pipeline / Go-handlers integration.
type Script struct {
	Scoped bool
	Lang   string
	Body   string
	Pos    Position
}

// Style is the verbatim <style> block plus its meta attributes.
// The body is not preprocessed; scoped CSS rewriting happens later.
type Style struct {
	Scoped bool
	Lang   string // value of lang= attr; "css" by default
	Body   string
	Pos    Position
}

// Node is implemented by every AST node inside a <template> block.
type Node interface {
	Pos() Position
	isNode()
}

// TextNode is literal text between tags. Whitespace is preserved as
// written so the diff layer can compare consistently — collapsing
// happens (if at all) at render time, not at parse time.
type TextNode struct {
	Text     string
	Position Position
}

func (n *TextNode) Pos() Position { return n.Position }
func (n *TextNode) isNode()       {}

// InterpolationNode is a {{ expr }} site. Expr is the raw text
// between the braces, trimmed of outer whitespace. The expression
// grammar is not parsed here — Go-like syntax is interpreted /
// evaluated by a later layer that has access to component state.
type InterpolationNode struct {
	Expr     string
	Position Position
}

func (n *InterpolationNode) Pos() Position { return n.Position }
func (n *InterpolationNode) isNode()       {}

// ElementNode is an HTML element OR a component reference. The
// IsComponent flag is true when the tag name starts with an
// uppercase letter (Vue convention) and is not the "template"
// special-case wrapper.
type ElementNode struct {
	Tag         string
	Attrs       []Attribute
	Children    []Node
	SelfClosing bool
	IsComponent bool
	Position    Position
}

func (n *ElementNode) Pos() Position { return n.Position }
func (n *ElementNode) isNode()       {}

// AttrKind classifies an attribute by syntactic shape. Codegen and
// the interpreter dispatch on Kind to decide what to do with the
// value.
type AttrKind int

const (
	// AttrPlain is a literal HTML attribute: class="posts".
	AttrPlain AttrKind = iota
	// AttrBind is a dynamic attribute binding: :src="url" or
	// nl-bind:src="url". Value is a Go expression to evaluate.
	AttrBind
	// AttrOn is an event binding: @click="like" or
	// nl-on:click="like". Value is a handler method name.
	AttrOn
	// AttrDirective is a structural directive: nl-if, nl-for,
	// nl-show, nl-model, nl-html, nl-text, nl-once, nl-pre,
	// nl-else, nl-else-if, nl-slot.
	AttrDirective
)

func (k AttrKind) String() string {
	switch k {
	case AttrPlain:
		return "plain"
	case AttrBind:
		return "bind"
	case AttrOn:
		return "on"
	case AttrDirective:
		return "directive"
	}
	return "?"
}

// Attribute is one name=value pair on an ElementNode (including
// directives, which are encoded as Attrs rather than separate node
// types — keeps the AST shallow and easy to walk).
//
// Field meaning by Kind:
//
//   - AttrPlain:     Name = attribute name (e.g. "class"), Value = literal
//   - AttrBind:      Name = target attr (e.g. "src"),       Value = expression
//   - AttrOn:        Name = event name (e.g. "click"),      Value = handler ref
//   - AttrDirective: Name = directive (e.g. "if", "for"),   Value = expression
//
// Arg holds the post-colon argument for directives that take one
// (currently only nl-slot:NAME); empty for everything else.
//
// Modifiers holds dot-suffix modifiers on AttrOn and AttrDirective
// (e.g. @click.prevent.stop → Modifiers=["prevent","stop"];
// nl-model.lazy.number → Modifiers=["lazy","number"]).
type Attribute struct {
	Kind      AttrKind
	Raw       string // exact attribute name as written in source
	Name      string // canonical name; meaning depends on Kind (see godoc above)
	Arg       string // post-colon argument; only set for nl-slot:X today
	Modifiers []string
	Value     string
	Position  Position

	// CallArgs is populated by the parser when an AttrOn value
	// uses the call form @click="like(Post.ID, msg)": each arg
	// is one expression to evaluate against the current scope at
	// render time. Empty (nil) when the value is a bare handler
	// ident — the v0 form @click="like".
	//
	// The lowering wires CallArgs into a data-nl-args attribute
	// carrying the JSON-encoded evaluated values; the server
	// dispatch matches them positionally to the handler's
	// non-Ctx parameters.
	CallArgs []string
}
