package template

import (
	"encoding/json"
	"fmt"
	"html"
	"reflect"
	"strconv"
	"strings"
)

// Render walks an IR Fragment against scope and produces a Rendered
// tree ready for the diff layer and the wire. It is the load-bearing
// runtime entry point — every live-template re-render goes through
// here.
//
// scope is the value identifiers in the template resolve against.
// Typically the component itself (or its State struct). Helper
// functions, registered via WithHelpers, are looked up before fields
// when a CallExpr appears.
//
// Errors during evaluation are NOT returned. Instead they become
// inline "[!err: …]" markers in the rendered output so problems are
// immediately visible at dev time rather than swallowed. Production
// callers that prefer logging-and-suppression can wrap an
// ErrorHandler via WithErrorHandler.
func Render(frag *Fragment, scopeVal any, opts ...RenderOption) Rendered {
	o := defaultRenderOpts()
	for _, opt := range opts {
		opt(&o)
	}
	sc := newScope(scopeVal, o.helpers)
	return renderFragment(frag, sc, &o)
}

// RenderOption configures one call to Render. Helpers and error
// handlers are the two knobs today; more options (component
// registry, sanitizer overrides) will appear with the engine layer.
type RenderOption func(*renderOpts)

// WithHelpers registers a name → function map that the expression
// evaluator can look up when it encounters a CallExpr. Pass plain Go
// funcs: their signatures are reflected at call time.
func WithHelpers(h map[string]any) RenderOption {
	return func(o *renderOpts) {
		if o.helpers == nil {
			o.helpers = make(map[string]any, len(h))
		}
		for k, v := range h {
			o.helpers[k] = v
		}
	}
}

// WithErrorHandler intercepts render-time evaluation errors. If the
// handler returns a non-empty string, that string is used as the
// rendered output for the failing slot (bypassing the default
// "[!err: …]" marker). Useful for production to log + emit "" so
// users never see internal messages.
func WithErrorHandler(fn func(err error, pos Position) string) RenderOption {
	return func(o *renderOpts) {
		o.errFn = fn
	}
}

type renderOpts struct {
	helpers  map[string]any
	errFn    func(err error, pos Position) string
	resolver ComponentResolver
}

// ComponentResolver is what the interpreter calls to instantiate a
// child component referenced by a <Foo /> tag. Engine implements it;
// tests can plug in a fake. Decoupling lets interpret.go stay free
// of engine internals.
type ComponentResolver interface {
	// Resolve returns a fresh component instance and the parsed
	// fragment to render it against. The error is surfaced as an
	// inline marker if the name isn't registered.
	Resolve(name string) (Component, *Fragment, error)
}

// WithComponents wires a ComponentResolver into a Render call. The
// session passes the Engine here so <Foo /> tags in the template
// resolve to registered components. Without a resolver, child
// components render as visible error markers.
func WithComponents(r ComponentResolver) RenderOption {
	return func(o *renderOpts) { o.resolver = r }
}

func defaultRenderOpts() renderOpts {
	return renderOpts{}
}

// renderFragment evaluates each Slot in frag against scope and packs
// the results into a Rendered with the fragment's Statics. The
// invariant len(Statics) == len(Slots)+1 set up by the lowering
// passes through unchanged.
func renderFragment(frag *Fragment, sc *scope, o *renderOpts) Rendered {
	d := make([]any, len(frag.Slots))
	for i, s := range frag.Slots {
		d[i] = renderSlot(s, sc, o)
	}
	return Rendered{S: frag.Statics, D: d}
}

func renderSlot(s Slot, sc *scope, o *renderOpts) any {
	switch sl := s.(type) {
	case ExprSlot:
		v, err := evalExpr(sl.Expr, sc)
		if err != nil {
			return errorMarker(err, sl.Pos, o)
		}
		return formatText(v)

	case ArgsSlot:
		// Each arg evaluates to a Go value; JSON-encode each so
		// type info survives the wire round-trip. The whole
		// array is HTML-escaped at the end so embedding inside
		// data-nl-args="..." doesn't break the attribute.
		parts := make([]string, len(sl.Exprs))
		for i, e := range sl.Exprs {
			v, err := evalExpr(e, sc)
			if err != nil {
				return errorMarker(err, sl.Pos, o)
			}
			b, err := json.Marshal(v)
			if err != nil {
				return errorMarker(err, sl.Pos, o)
			}
			parts[i] = string(b)
		}
		return html.EscapeString("[" + strings.Join(parts, ",") + "]")

	case BranchSlot:
		for _, b := range sl.Branches {
			if b.Cond == "" {
				// nl-else: always taken once we've reached it.
				return renderFragment(b.Fragment, sc, o)
			}
			v, err := evalExpr(b.Cond, sc)
			if err != nil {
				return errorMarker(err, sl.Pos, o)
			}
			if truthy(v) {
				return renderFragment(b.Fragment, sc, o)
			}
		}
		// No branch matched, no else — slot renders nil (no HTML).
		return nil

	case LoopSlot:
		return renderLoop(sl, sc, o)

	case ComponentSlot:
		return renderComponent(sl, sc, o)
	}
	return nil
}

// renderComponent expands a <Foo :p="x" /> reference into a nested
// Rendered tree by resolving the component, instantiating it,
// pushing evaluated prop values onto its exported fields, and
// rendering its template against a scope rooted at the child.
//
// Children in v1 are PURE renders: Mount, Refresh, and event
// handlers don't fire for child components reached through this
// path. Only the top-level component owned by a Session sees
// lifecycle hooks. State on a child is "props in, HTML out"; if
// state-per-child is needed, hoist it to the parent and pass
// downward via props.
//
// Default-slot children (everything between <Foo>…</Foo>) are
// captured by the parser but ignored here. Slot routing is a v2
// concern — emitting a visible warning marker keeps the gap
// noticeable instead of silently dropping authored markup.
func renderComponent(sl ComponentSlot, parentScope *scope, o *renderOpts) any {
	if o.resolver == nil {
		return errorMarker(fmt.Errorf("no component resolver wired; cannot render <%s>", sl.Name), sl.Pos, o)
	}
	component, fragment, err := o.resolver.Resolve(sl.Name)
	if err != nil {
		return errorMarker(err, sl.Pos, o)
	}
	if err := assignProps(component, sl.Props, parentScope); err != nil {
		return errorMarker(fmt.Errorf("<%s>: %w", sl.Name, err), sl.Pos, o)
	}
	if sl.Children != nil {
		// Authors expect children to render somewhere; until slots
		// land, surface the situation rather than letting it vanish.
		// One marker per render is fine — diff stability is preserved
		// because the marker string is identical across renders.
		return Rendered{
			S: []string{"<!-- nl: <" + sl.Name + "> default-slot children are not rendered until nl-slot lands -->"},
			D: []any{renderFragment(fragment, newScope(component, o.helpers), o)},
		}
	}
	return renderFragment(fragment, newScope(component, o.helpers), o)
}

// assignProps evaluates each ComponentProp against parentScope and
// stores it on the matching exported field of the child component.
// Prop names are normalized to PascalCase before field lookup so
// :user-card-key="x" maps to UserCardKey (same convention as the
// event dispatcher uses for handler names).
//
// IsBind=false props ship their literal Value as a string; IsBind=true
// props evaluate the expression and assign whatever Go value comes
// back. Convertible types go through reflect.Convert; mismatches
// return an error that bubbles up as an inline marker.
func assignProps(component Component, props []ComponentProp, parentScope *scope) error {
	cv := reflect.ValueOf(component)
	for cv.Kind() == reflect.Ptr {
		if cv.IsNil() {
			return fmt.Errorf("nil component receiver")
		}
		cv = cv.Elem()
	}
	for _, p := range props {
		var value any = p.Value
		if p.IsBind {
			v, err := evalExpr(p.Value, parentScope)
			if err != nil {
				return fmt.Errorf("prop %q: %w", p.Name, err)
			}
			value = v
		}

		field, ok := lookupPropField(cv, p.Name)
		if !ok {
			return fmt.Errorf("prop %q: no matching exported field on %s", p.Name, cv.Type())
		}
		if !field.CanSet() {
			return fmt.Errorf("prop %q: field is not settable", p.Name)
		}
		if err := setPropValue(field, value); err != nil {
			return fmt.Errorf("prop %q: %w", p.Name, err)
		}
	}
	return nil
}

// lookupPropField finds the exported field matching a prop name.
// First tries the name as-written (handles authors who write
// :UserName in PascalCase); falls back to PascalCase conversion
// (the common case — HTML tokenizer lowercases attribute names so
// :user → "user" → field "User").
func lookupPropField(cv reflect.Value, name string) (reflect.Value, bool) {
	if f := cv.FieldByName(name); f.IsValid() {
		return f, true
	}
	if f := cv.FieldByName(titleCaseEvent(name)); f.IsValid() {
		return f, true
	}
	return reflect.Value{}, false
}

func setPropValue(dst reflect.Value, value any) error {
	if value == nil {
		dst.Set(reflect.Zero(dst.Type()))
		return nil
	}
	src := reflect.ValueOf(value)
	if src.Type() == dst.Type() {
		dst.Set(src)
		return nil
	}
	// Literal props arrive as strings (compact="true", count="3"); reflect
	// won't convert string → bool / int / float on its own, so coerce
	// here. Bind props (IsBind=true) already pass through evalExpr and
	// land with the right Go type, so this path is only hit for literals.
	if src.Kind() == reflect.String {
		if coerced, ok := coerceStringTo(src.String(), dst.Type()); ok {
			dst.Set(coerced)
			return nil
		}
	}
	if src.Type().ConvertibleTo(dst.Type()) {
		dst.Set(src.Convert(dst.Type()))
		return nil
	}
	return fmt.Errorf("cannot assign %T to %s", value, dst.Type())
}

// coerceStringTo handles the small fixed set of string→primitive
// conversions needed for HTML-attribute-style literal props. Returns
// a reflect.Value of dst's exact type on success.
func coerceStringTo(s string, t reflect.Type) (reflect.Value, bool) {
	switch t.Kind() {
	case reflect.Bool:
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes":
			return reflect.ValueOf(true).Convert(t), true
		case "false", "0", "no", "":
			return reflect.ValueOf(false).Convert(t), true
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			return reflect.ValueOf(n).Convert(t), true
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64); err == nil {
			return reflect.ValueOf(n).Convert(t), true
		}
	case reflect.Float32, reflect.Float64:
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return reflect.ValueOf(f).Convert(t), true
		}
	}
	return reflect.Value{}, false
}

// renderLoop evaluates Iter, iterates over the result, and renders
// the Body fragment with the loop variable bound for each item.
// Statics for the Comprehension come from the Body fragment so
// they're stable across empty/non-empty renders (a load-bearing
// detail for the diff algorithm — Comprehension statics shipped
// once should never change).
func renderLoop(sl LoopSlot, parent *scope, o *renderOpts) Comprehension {
	comp := Comprehension{S: sl.Body.Statics}

	iter, err := evalExpr(sl.Iter, parent)
	if err != nil {
		// Iter expression broken: ship an empty comp with an error
		// in the first row so the user sees something went wrong.
		comp.Rows = []Row{{Key: "err", D: []any{errorMarker(err, sl.Pos, o)}}}
		return comp
	}
	if iter == nil {
		return comp
	}

	rv := reflect.ValueOf(iter)
	for rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return comp
		}
		rv = rv.Elem()
	}

	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			item := rv.Index(i).Interface()
			child := parent.push(map[string]any{sl.Var: item})
			rendered := renderFragment(sl.Body, child, o)
			key := loopKey(sl, child, i)
			comp.Rows = append(comp.Rows, Row{Key: key, D: rendered.D})
		}
	case reflect.Map:
		// v1: map iteration order matches Go (randomized). Stable
		// ordering can be added when we know there's a need; the
		// diff layer keys rows by KeyExpr anyway, so reordering
		// across renders is visible to the client as a move op, not
		// a content change.
		iterIdx := 0
		mi := rv.MapRange()
		for mi.Next() {
			child := parent.push(map[string]any{sl.Var: mi.Value().Interface()})
			rendered := renderFragment(sl.Body, child, o)
			key := loopKey(sl, child, iterIdx)
			comp.Rows = append(comp.Rows, Row{Key: key, D: rendered.D})
			iterIdx++
		}
	default:
		comp.Rows = []Row{{Key: "err", D: []any{errorMarker(fmt.Errorf("nl-for: not iterable (%T)", iter), sl.Pos, o)}}}
	}
	return comp
}

// loopKey computes the row key for a Comprehension row. Uses the
// :key="..." expression when present; falls back to the iteration
// index for unkeyed loops (the diff layer can still work without
// keys, just at the cost of always treating row content as the
// identity — moves look like updates).
func loopKey(sl LoopSlot, sc *scope, idx int) string {
	if sl.KeyExpr == "" {
		return strconv.Itoa(idx)
	}
	v, err := evalExpr(sl.KeyExpr, sc)
	if err != nil || v == nil {
		return strconv.Itoa(idx)
	}
	return formatText(v)
}

// formatText turns an evaluated value into the HTML-escaped string
// that will appear in the Rendered tree's dynamics. The escape level
// is conservative (covers both text and attribute contexts) so we
// don't need a separate path for attribute slots; that buys safety
// at the cost of slightly more verbose output in attributes.
func formatText(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return html.EscapeString(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(rv.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(rv.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(rv.Float(), 'g', -1, 64)
	case reflect.Bool:
		return strconv.FormatBool(rv.Bool())
	case reflect.String:
		return html.EscapeString(rv.String())
	}
	// Fallback: Go's default formatting. Useful for diagnostics; users
	// who want custom rendering can call a helper or implement Stringer.
	if s, ok := v.(fmt.Stringer); ok {
		return html.EscapeString(s.String())
	}
	return html.EscapeString(fmt.Sprintf("%v", v))
}

// errorMarker turns an evaluation error into the string the render
// produces in place of the broken slot. The default form is a
// visible "[!err: …]" so dev-mode catches problems immediately;
// production callers can override via WithErrorHandler to log + emit
// an empty string.
func errorMarker(err error, pos Position, o *renderOpts) string {
	if o.errFn != nil {
		return o.errFn(err, pos)
	}
	loc := ""
	if pos.Filename != "" {
		loc = " @" + pos.String()
	}
	return "[!err" + loc + ": " + html.EscapeString(err.Error()) + "]"
}

// --- scope ---------------------------------------------------------

// scope is the lookup chain for expression evaluation. Each push (a
// loop iteration) creates a child with new bindings; identifier
// resolution walks bindings → parent → … → root (which is whatever
// scope value Render was called with, accessed via reflection).
//
// Helpers live separately from the chain because they're never
// shadowed by loop variables — a helper named "len" should always be
// the helper, never the loop var. (Builtins are handled even further
// upstream in expr.go.)
type scope struct {
	parent   *scope
	bindings map[string]any
	root     any
	helpers  map[string]any
}

func newScope(root any, helpers map[string]any) *scope {
	return &scope{root: root, helpers: helpers}
}

func (s *scope) push(bindings map[string]any) *scope {
	return &scope{parent: s, bindings: bindings, root: s.root, helpers: s.helpers}
}

func (s *scope) lookup(name string) (any, bool) {
	for cur := s; cur != nil; cur = cur.parent {
		if v, ok := cur.bindings[name]; ok {
			return v, true
		}
	}
	// Fall back to root via reflection so `Posts` resolves to
	// scope.Posts when root is a struct.
	if s.root != nil {
		if v, err := memberAccess(s.root, name); err == nil {
			return v, true
		}
	}
	return nil, false
}

func (s *scope) helper(name string) (any, bool) {
	if s.helpers == nil {
		return nil, false
	}
	v, ok := s.helpers[name]
	return v, ok
}