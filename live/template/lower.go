package template

import (
	"fmt"
	"strings"
)

// Lower walks an AST and produces an IR Fragment ready to be
// interpreted at render time. Errors come back when a feature is not
// yet supported (nl-model / nl-show / nl-html / nl-text / nl-pre /
// nl-slot) or when the AST is structurally invalid (an nl-else with
// no preceding nl-if, an element with both nl-if and nl-for, etc.).
//
// Static dedup across fragments is intentionally not performed here —
// each Fragment owns its own Statics. A later optimization pass (or
// a future codegen step) can canonicalize.
func Lower(template *Template) (*Fragment, error) {
	l := &lowerer{}
	return l.fragment(template.Children)
}

type lowerer struct{}

func (l *lowerer) fragment(nodes []Node) (*Fragment, error) {
	var b fragBuilder
	if err := l.lowerSiblings(nodes, &b); err != nil {
		return nil, err
	}
	return b.finish(), nil
}

// lowerSiblings walks a sibling list, handling nl-if / nl-else-if /
// nl-else chains by greedily gobbling consecutive branch elements
// into one BranchSlot. Everything else (text, interp, plain element,
// nl-for, component) is lowered one node at a time.
func (l *lowerer) lowerSiblings(nodes []Node, b *fragBuilder) error {
	i := 0
	for i < len(nodes) {
		n := nodes[i]

		// Text and interpolation are straightforward.
		switch v := n.(type) {
		case *TextNode:
			b.text(v.Text)
			i++
			continue
		case *InterpolationNode:
			b.slot(ExprSlot{Expr: v.Expr, Pos: v.Position})
			i++
			continue
		}

		elem, ok := n.(*ElementNode)
		if !ok {
			return &ParseError{Pos: n.Pos(), Msg: fmt.Sprintf("lower: unexpected node type %T", n)}
		}

		s, err := extractStructural(elem)
		if err != nil {
			return err
		}

		// Orphan else-if / else: parser allows them, lowering rejects.
		if s.isElseIf && s.ifExpr == "" {
			return &ParseError{Pos: elem.Position, Msg: "nl-else-if with no preceding nl-if"}
		}
		if s.isElse {
			return &ParseError{Pos: elem.Position, Msg: "nl-else with no preceding nl-if"}
		}

		// nl-for: lowers to a LoopSlot. Cannot combine with nl-if on the
		// same element (Vue 3 rule — split into a wrapping <template>).
		if s.forExpr != "" {
			if s.ifExpr != "" {
				return &ParseError{Pos: elem.Position, Msg: "nl-for and nl-if cannot coexist on the same element; wrap in <template nl-if>"}
			}
			loop, err := l.lowerLoop(elem, s)
			if err != nil {
				return err
			}
			b.slot(loop)
			i++
			continue
		}

		// nl-if: lowers to a BranchSlot, greedily collecting else-if /
		// else from immediately-following siblings.
		if s.ifExpr != "" {
			branchSlot, consumed, err := l.lowerBranches(nodes, i)
			if err != nil {
				return err
			}
			b.slot(branchSlot)
			i += consumed
			continue
		}

		// Plain element — including <template> invisible wrappers and
		// component references — flows through the same code path. The
		// emit step distinguishes.
		if err := l.lowerElement(elem, b); err != nil {
			return err
		}
		i++
	}
	return nil
}

// lowerLoop builds a LoopSlot from an element annotated with nl-for.
// The element's body (without nl-for and :key, since they belong to
// the loop itself) becomes the Body fragment.
func (l *lowerer) lowerLoop(elem *ElementNode, s structuralDirs) (LoopSlot, error) {
	v, iter, err := parseForExpr(s.forExpr, elem.Position)
	if err != nil {
		return LoopSlot{}, err
	}
	var bodyB fragBuilder
	if err := l.lowerElementBody(elem, &bodyB); err != nil {
		return LoopSlot{}, err
	}
	return LoopSlot{
		Var:     v,
		Iter:    iter,
		KeyExpr: s.keyExpr,
		Body:    bodyB.finish(),
		Pos:     elem.Position,
	}, nil
}

// lowerBranches starts at nodes[startIdx] (an nl-if element) and
// gobbles consecutive nl-else-if / nl-else sibling elements into a
// BranchSlot. Returns the slot and the number of source nodes
// consumed (always >= 1).
func (l *lowerer) lowerBranches(nodes []Node, startIdx int) (BranchSlot, int, error) {
	first := nodes[startIdx].(*ElementNode)
	firstDirs, _ := extractStructural(first)

	var branches []Branch

	// First branch: the nl-if.
	body, err := l.fragmentFromElement(first)
	if err != nil {
		return BranchSlot{}, 0, err
	}
	branches = append(branches, Branch{Cond: firstDirs.ifExpr, Fragment: body})
	consumed := 1
	sawElse := false

	for startIdx+consumed < len(nodes) {
		// Skip whitespace-only text nodes between branches so users
		// can format with newlines without breaking the chain.
		if tn, ok := nodes[startIdx+consumed].(*TextNode); ok {
			if strings.TrimSpace(tn.Text) == "" {
				consumed++
				continue
			}
			break
		}

		nextElem, ok := nodes[startIdx+consumed].(*ElementNode)
		if !ok {
			break
		}
		nextDirs, err := extractStructural(nextElem)
		if err != nil {
			return BranchSlot{}, 0, err
		}

		switch {
		case nextDirs.isElseIf:
			if sawElse {
				return BranchSlot{}, 0, &ParseError{Pos: nextElem.Position, Msg: "nl-else-if after nl-else"}
			}
			body, err := l.fragmentFromElement(nextElem)
			if err != nil {
				return BranchSlot{}, 0, err
			}
			branches = append(branches, Branch{Cond: nextDirs.elseIfExpr, Fragment: body})
			consumed++
		case nextDirs.isElse:
			if sawElse {
				return BranchSlot{}, 0, &ParseError{Pos: nextElem.Position, Msg: "duplicate nl-else"}
			}
			body, err := l.fragmentFromElement(nextElem)
			if err != nil {
				return BranchSlot{}, 0, err
			}
			branches = append(branches, Branch{Cond: "", Fragment: body})
			sawElse = true
			consumed++
		default:
			// Not part of the branch chain.
			return BranchSlot{Branches: branches, Pos: first.Position}, consumed, nil
		}
	}

	return BranchSlot{Branches: branches, Pos: first.Position}, consumed, nil
}

// fragmentFromElement lowers a single element (including its open
// tag, attributes, and children) into a standalone Fragment. Used
// for branch bodies, where the branch wraps the whole element.
func (l *lowerer) fragmentFromElement(elem *ElementNode) (*Fragment, error) {
	var b fragBuilder
	if err := l.lowerElement(elem, &b); err != nil {
		return nil, err
	}
	return b.finish(), nil
}

// lowerElement emits one element into the builder. Handles the
// <template> invisible-wrapper case, component references, and
// plain HTML elements (opening tag with attrs, children, closing tag).
func (l *lowerer) lowerElement(elem *ElementNode, b *fragBuilder) error {
	if err := checkUnsupportedDirectives(elem); err != nil {
		return err
	}

	// <template> as an element is an invisible group wrapper — render
	// its children with no surrounding tags. (The outermost <template>
	// of the SFC was already unwrapped in parseTemplate.)
	if strings.EqualFold(elem.Tag, "template") {
		return l.lowerSiblings(elem.Children, b)
	}

	if elem.IsComponent {
		return l.lowerComponent(elem, b)
	}

	// <nl-island /> is built-in sugar over a div with the
	// nl-island / nl-island-src / nl-island-props attribute
	// trio. Pure server-side rewrite — the client only ever
	// sees the desugared div, so all the existing morph /
	// lifecycle / channel logic applies unchanged.
	if strings.EqualFold(elem.Tag, "nl-island") {
		return l.lowerIslandTag(elem, b)
	}

	return l.lowerHTMLElement(elem, b)
}

// lowerIslandTag desugars <nl-island name="X" src="Y"
// :props="Z"> into <div nl-island="X" nl-island-src="Y"
// nl-island-props="<JSON of Z>">. Structural directives
// (nl-if / nl-for) on the tag still work because
// lowerSiblings handles them before reaching us — by the
// time we run, only non-structural attrs remain.
//
// Recognized attrs:
//
//	name="…"   →  the island's logical name (required)
//	src="…"    →  ES module URL the client dynamic-imports (required)
//	:props="…" →  expression whose value JSON-encodes into props
//	:src="…"   →  dynamic src (rare; e.g. routing the URL through
//	              a build-hash; uses the regular bind path)
//
// Anything else triggers an error rather than silently
// passing through — keeps the surface honest while it's
// young; we can relax later if there's a real need.
func (l *lowerer) lowerIslandTag(elem *ElementNode, b *fragBuilder) error {
	var name, src string
	var nameSet, srcSet bool
	var propsAttr *Attribute
	var dynamicSrc *Attribute
	for i := range elem.Attrs {
		a := &elem.Attrs[i]
		switch {
		case a.Kind == AttrPlain && a.Name == "name":
			name = a.Value
			nameSet = true
		case a.Kind == AttrPlain && a.Name == "src":
			src = a.Value
			srcSet = true
		case a.Kind == AttrBind && a.Name == "props":
			propsAttr = a
		case a.Kind == AttrBind && a.Name == "src":
			dynamicSrc = a
		default:
			return &ParseError{
				Pos: a.Position,
				Msg: fmt.Sprintf("<nl-island>: unsupported attribute %q (use name, src, :src, :props)", a.Raw),
			}
		}
	}
	if !nameSet || name == "" {
		return &ParseError{Pos: elem.Position, Msg: "<nl-island>: missing name=\"…\""}
	}
	if !srcSet && dynamicSrc == nil {
		return &ParseError{Pos: elem.Position, Msg: "<nl-island>: missing src=\"…\" (or :src=\"…\")"}
	}

	b.text(`<div nl-island="` + name + `"`)
	if dynamicSrc != nil {
		b.text(` nl-island-src="`)
		b.slot(ExprSlot{Expr: dynamicSrc.Value, Pos: dynamicSrc.Position})
		b.text(`"`)
	} else {
		b.text(` nl-island-src="` + src + `"`)
	}
	if propsAttr != nil {
		b.text(` nl-island-props="`)
		b.slot(IslandPropsSlot{Expr: propsAttr.Value, Pos: propsAttr.Position})
		b.text(`"`)
	}
	b.text(`></div>`)
	return nil
}

// lowerElementBody emits an element's body assuming its structural
// directives (nl-for, nl-if, etc.) have already been consumed by the
// caller. It's used for loop bodies and branch bodies, where the
// outer element still renders (with non-structural attrs).
func (l *lowerer) lowerElementBody(elem *ElementNode, b *fragBuilder) error {
	if strings.EqualFold(elem.Tag, "template") {
		return l.lowerSiblings(elem.Children, b)
	}
	if elem.IsComponent {
		return l.lowerComponent(elem, b)
	}
	if strings.EqualFold(elem.Tag, "nl-island") {
		return l.lowerIslandTag(elem, b)
	}
	return l.lowerHTMLElement(elem, b)
}

// lowerHTMLElement emits "<tag attrs>...children...</tag>" into b.
// Attributes are categorized at emit time: plain attrs go entirely
// into statics; :bind attrs split the value into a slot; @on attrs
// flow into statics as a "nl-on:event=..." marker that the client JS
// hooks up on the wire.
func (l *lowerer) lowerHTMLElement(elem *ElementNode, b *fragBuilder) error {
	b.text("<" + elem.Tag)
	attrs, err := desugarModel(elem.Attrs)
	if err != nil {
		return err
	}
	for _, a := range attrs {
		if isStructuralAttr(a) {
			continue
		}
		if err := emitAttr(a, b); err != nil {
			return err
		}
	}
	if elem.SelfClosing || voidElements[strings.ToLower(elem.Tag)] {
		// Both forms render the same HTML; choose " />" for clarity.
		b.text(" />")
		return nil
	}
	b.text(">")
	if err := l.lowerSiblings(elem.Children, b); err != nil {
		return err
	}
	b.text("</" + elem.Tag + ">")
	return nil
}

func emitAttr(a Attribute, b *fragBuilder) error {
	switch a.Kind {
	case AttrPlain:
		b.text(" " + a.Name + `="` + a.Value + `"`)
		return nil
	case AttrBind:
		// :nl-island-props is special: the value must reach the
		// client as JSON (not a stringified scalar) so the
		// island can rehydrate typed props. Same JSON-encoded
		// + HTML-escaped pipeline as data-nl-args; different
		// slot type because we emit one value, not an array.
		if a.Name == "nl-island-props" {
			b.text(` nl-island-props="`)
			b.slot(IslandPropsSlot{Expr: a.Value, Pos: a.Position})
			b.text(`"`)
			return nil
		}
		b.text(" " + a.Name + `="`)
		b.slot(ExprSlot{Expr: a.Value, Pos: a.Position})
		b.text(`"`)
		return nil
	case AttrOn:
		// Event bindings flow to the wire as marker attrs the client
		// scans after every diff to (re)attach listeners. Modifiers
		// ride along in the attribute name dot-suffix so the client
		// has the full directive surface visible.
		name := "nl-on:" + a.Name
		if len(a.Modifiers) > 0 {
			name += "." + strings.Join(a.Modifiers, ".")
		}
		b.text(" " + name + `="` + a.Value + `"`)
		// Call-form handlers ship their arg expressions as a
		// JSON-encoded data-nl-args attribute the client reads
		// and ships back as payload.args.
		if len(a.CallArgs) > 0 {
			b.text(` data-nl-args="`)
			b.slot(ArgsSlot{Exprs: a.CallArgs, Pos: a.Position})
			b.text(`"`)
		}
		return nil
	case AttrDirective:
		// Client-side directives are pass-through attributes —
		// the engine doesn't interpret them server-side; the
		// client JS scans for them and applies its own
		// behavior (hook lifecycle, stream-op routing).
		switch a.Name {
		case "hook":
			b.text(" nl-hook=\"" + a.Value + "\"")
			return nil
		case "stream":
			b.text(" nl-stream=\"" + a.Value + "\"")
			return nil
		case "navigate":
			// Boolean marker: presence of the attribute is what
			// the client looks for, not its value. Emit value-
			// less attribute so the DOM sees the marker.
			b.text(" nl-navigate")
			return nil
		case "island":
			// Island name. Pass through as nl-island="<name>".
			// The client matches this against the WeakMap of
			// mounted instances and against PushIsland(name)
			// targets from the server.
			b.text(" nl-island=\"" + a.Value + "\"")
			return nil
		case "island-src":
			// URL of the ES module that exports mount/updated/
			// destroyed. The client dynamic-import()s it on
			// first sight of the element.
			b.text(" nl-island-src=\"" + a.Value + "\"")
			return nil
		}
		// Other directives are handled upstream; reaching this
		// point with one is a bug in the lowering routing.
		return &ParseError{Pos: a.Position, Msg: fmt.Sprintf("lower: directive nl-%s reached emit step (should have been handled)", a.Name)}
	}
	return &ParseError{Pos: a.Position, Msg: fmt.Sprintf("lower: unknown attr kind %v", a.Kind)}
}

// lowerComponent records a ComponentSlot. Prop values are categorized
// the same way attributes are at the parser layer: plain = literal,
// bind = expression. Event handlers on components and slot routing
// are deferred to a later pass.
func (l *lowerer) lowerComponent(elem *ElementNode, b *fragBuilder) error {
	var props []ComponentProp
	for _, a := range elem.Attrs {
		if isStructuralAttr(a) {
			continue
		}
		switch a.Kind {
		case AttrPlain:
			props = append(props, ComponentProp{Name: a.Name, Value: a.Value, IsBind: false, Pos: a.Position})
		case AttrBind:
			props = append(props, ComponentProp{Name: a.Name, Value: a.Value, IsBind: true, Pos: a.Position})
		case AttrOn:
			return &ParseError{Pos: a.Position, Msg: "@event on components is not yet supported (slot routing — deferred)"}
		case AttrDirective:
			return &ParseError{Pos: a.Position, Msg: fmt.Sprintf("directive nl-%s on a component is not yet supported", a.Name)}
		}
	}

	var children *Fragment
	if len(elem.Children) > 0 {
		var cb fragBuilder
		if err := l.lowerSiblings(elem.Children, &cb); err != nil {
			return err
		}
		children = cb.finish()
	}

	b.slot(ComponentSlot{
		Name:     elem.Tag,
		Props:    props,
		Children: children,
		Pos:      elem.Position,
	})
	return nil
}

// structuralDirs is what extractStructural returns: the directives
// that affect *whether* and *how* an element renders, as opposed to
// the directives that affect *what* is rendered onto it.
type structuralDirs struct {
	ifExpr     string
	elseIfExpr string
	isElseIf   bool
	isElse     bool
	forExpr    string
	keyExpr    string
}

func extractStructural(elem *ElementNode) (structuralDirs, error) {
	var s structuralDirs
	for _, a := range elem.Attrs {
		switch {
		case a.Kind == AttrDirective && a.Name == "if":
			s.ifExpr = a.Value
		case a.Kind == AttrDirective && a.Name == "else-if":
			s.isElseIf = true
			s.elseIfExpr = a.Value
		case a.Kind == AttrDirective && a.Name == "else":
			s.isElse = true
		case a.Kind == AttrDirective && a.Name == "for":
			s.forExpr = a.Value
		case a.Kind == AttrBind && a.Name == "key":
			s.keyExpr = a.Value
		}
	}
	return s, nil
}

func isStructuralAttr(a Attribute) bool {
	if a.Kind == AttrBind && a.Name == "key" {
		return true
	}
	if a.Kind != AttrDirective {
		return false
	}
	switch a.Name {
	// Structural in the literal sense: handled by lowerSiblings
	// (BranchSlot / LoopSlot) and not emitted as HTML.
	case "if", "else-if", "else", "for":
		return true
	// nl-once is a render-time hint — silently no-op'd in v1 — and
	// must not leak into the emitted HTML as a stray attribute.
	case "once":
		return true
	}
	return false
}

// checkUnsupportedDirectives errors out on directives the parser
// accepts but the lowering does not yet implement. Better to fail
// loudly at compile time than silently drop a directive at render.
func checkUnsupportedDirectives(elem *ElementNode) error {
	for _, a := range elem.Attrs {
		if a.Kind != AttrDirective {
			continue
		}
		switch a.Name {
		case "show", "html", "text", "pre", "slot":
			return &ParseError{
				Pos: a.Position,
				Msg: fmt.Sprintf("nl-%s is parsed but not yet supported by the lowering (will land in a follow-up)", a.Name),
			}
		case "once":
			// no-op hint; safe to ignore
		}
	}
	return nil
}

// desugarModel rewrites nl-model directives into the underlying
// :value bind + @input (or @change for .lazy) + data-* markers
// the server's __model interceptor reads. Called inline during HTML
// element lowering so the rest of the emit path doesn't need to
// know nl-model exists.
//
// nl-model="Filter"                  → :value="Filter"
//                                       @input="__model"
//                                       data-model-expr="Filter"
// nl-model.lazy="Filter"             → :value, @change, data-model-expr
// nl-model.lazy.trim.number="Age"    → :value, @change,
//                                       data-model-expr="Age",
//                                       data-model-mods="trim.number"
//
// Value-coercion modifiers (trim, number) ride along in
// data-model-mods. Unknown modifiers are filtered out — the parser
// accepts any dot-suffix, but we only honor the documented set.
func desugarModel(attrs []Attribute) ([]Attribute, error) {
	var out []Attribute
	for _, a := range attrs {
		if a.Kind != AttrDirective || a.Name != "model" {
			out = append(out, a)
			continue
		}
		if !isValidModelExpr(a.Value) {
			return nil, &ParseError{Pos: a.Position, Msg: fmt.Sprintf("nl-model expression must be a bare identifier chain (got %q); index/method expressions are not yet supported", a.Value)}
		}
		event := "input"
		var valueMods []string
		for _, m := range a.Modifiers {
			switch m {
			case "lazy":
				event = "change"
			case "trim", "number":
				valueMods = append(valueMods, m)
			default:
				// Silently ignore unknown modifiers. Vue ecosystem
				// has accumulated many — debounce.NNN, capture,
				// passive — that we may grow into; rejecting now
				// would break templates that work elsewhere.
			}
		}
		out = append(out,
			Attribute{Kind: AttrBind, Name: "value", Value: a.Value, Position: a.Position},
			Attribute{Kind: AttrOn, Name: event, Value: "__model", Position: a.Position},
			Attribute{Kind: AttrPlain, Name: "data-model-expr", Value: a.Value, Position: a.Position},
		)
		if len(valueMods) > 0 {
			out = append(out, Attribute{
				Kind:     AttrPlain,
				Name:     "data-model-mods",
				Value:    strings.Join(valueMods, "."),
				Position: a.Position,
			})
		}
	}
	return out, nil
}

// isValidModelExpr accepts only bare-identifier chains: Filter,
// State.Filter, Form.Email.Address. Anything with brackets, parens,
// or operators is rejected at lowering time so users see the
// limitation before runtime.
func isValidModelExpr(expr string) bool {
	if expr == "" {
		return false
	}
	for _, part := range strings.Split(expr, ".") {
		if !isASCIIIdent(part) {
			return false
		}
	}
	return true
}

func isASCIIIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
		if !ok && i > 0 {
			ok = c >= '0' && c <= '9'
		}
		if !ok {
			return false
		}
	}
	return true
}

// parseForExpr decodes "x in xs" / "x, i in xs" / "k, v in m" forms
// to a (var, iter) pair. v1 supports only the single-var "x in xs"
// shape; index/key destructuring lands later with the interpreter's
// for-loop semantics.
func parseForExpr(expr string, pos Position) (string, string, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return "", "", &ParseError{Pos: pos, Msg: "nl-for needs an expression like \"x in xs\""}
	}
	parts := strings.SplitN(expr, " in ", 2)
	if len(parts) != 2 {
		return "", "", &ParseError{Pos: pos, Msg: fmt.Sprintf("nl-for: expected \"x in xs\", got %q", expr)}
	}
	v := strings.TrimSpace(parts[0])
	iter := strings.TrimSpace(parts[1])
	if strings.Contains(v, ",") {
		return "", "", &ParseError{Pos: pos, Msg: "nl-for index/key destructuring (\"x, i in xs\") is not yet supported"}
	}
	if v == "" || iter == "" {
		return "", "", &ParseError{Pos: pos, Msg: fmt.Sprintf("nl-for: malformed expression %q", expr)}
	}
	return v, iter, nil
}

// fragBuilder accumulates statics and slots while maintaining the
// invariant that finish() returns a Fragment with
// len(Statics) == len(Slots) + 1 (including the trivial empty case
// where both are 0 — finish coerces that to {[""], nil}).
type fragBuilder struct {
	statics []string
	slots   []Slot
}

func (b *fragBuilder) text(s string) {
	if s == "" {
		return
	}
	if len(b.statics) == len(b.slots) {
		// Need a new static slot (we either just started or just
		// emitted a slot).
		b.statics = append(b.statics, s)
		return
	}
	// Otherwise the last static is "unpaired" and can absorb more text.
	b.statics[len(b.statics)-1] += s
}

func (b *fragBuilder) slot(sl Slot) {
	if len(b.statics) == len(b.slots) {
		// Need an empty static before this slot to keep the invariant.
		b.statics = append(b.statics, "")
	}
	b.slots = append(b.slots, sl)
}

func (b *fragBuilder) finish() *Fragment {
	if len(b.statics) == 0 && len(b.slots) == 0 {
		return &Fragment{Statics: []string{""}}
	}
	if len(b.statics) == len(b.slots) {
		// Trailing slot with no static after it — emit empty trailer.
		b.statics = append(b.statics, "")
	}
	return &Fragment{Statics: b.statics, Slots: b.slots}
}
