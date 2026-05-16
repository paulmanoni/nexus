// Package template implements the live template engine for nexus: an
// HTML rendering pipeline that splits compile-time-static markup from
// runtime-dynamic values so changes can ship as sparse diffs over a
// WebSocket connection.
//
// The two foundational types here are Rendered (the output of a single
// component render call) and CompDiff/Diff (the wire-format patch
// between two Rendered). The compilation pipeline that produces
// Rendered values from .nlt files and the runtime that holds
// connection state both build on top of these.
//
// Import alias note: this package is named "template" but is unrelated
// to text/template / html/template. Users typically alias on import,
// e.g. `import tmpl "github.com/paulmanoni/nexus/live/template"`.
package template

import (
	"fmt"
	"strings"
)

// Rendered is the output of a Component's Render call. The HTML it
// represents is the interleaving of statics and formatted dynamics:
//
//	S[0] + fmt(D[0]) + S[1] + fmt(D[1]) + ... + S[n]
//
// so len(S) MUST equal len(D)+1. Statics are compile-time constants
// shared across every render of the same component template; they
// never appear in diffs after the initial join frame. Dynamics carry
// per-render values; each entry is one of:
//
//   - string: a leaf interpolation (codegen pre-escapes)
//   - Rendered: a nested template fragment (e.g. an nl-if branch body)
//   - Comprehension: an nl-for loop result
//   - nil: an untaken nl-if branch
//
// Producing a Rendered is the responsibility of generated component
// code; this package is concerned only with stitching, equality, and
// diffing them.
type Rendered struct {
	S []string `json:"s"`
	D []any    `json:"d"`
}

// Comprehension is the output of an nl-for loop. Statics are shared
// across every row (the loop body is one template fragment); each Row
// carries a stable Key and its own dynamics. Rows are ordered — the
// slice index is the visual order on the page.
type Comprehension struct {
	S    []string `json:"s"`
	Rows []Row    `json:"r"`
}

// Row is one iteration of a Comprehension. Key is the stable
// identifier used by the diff algorithm to detect insert / remove /
// move; it comes from the loop's :key= binding (or, in the codegen,
// from id="post-{{ .ID }}"-style attributes).
type Row struct {
	Key string `json:"k"`
	D   []any  `json:"d"`
}

// HTML stitches the Rendered tree into a single HTML string. Used for
// the initial server-rendered frame returned from an HTTP GET (so the
// page is visible before the WS connects) and as a testing aid.
func (r Rendered) HTML() string {
	var b strings.Builder
	r.writeHTML(&b)
	return b.String()
}

// HTML stitches a Comprehension into a string by concatenating each
// row's interleaved S/D output. Empty Rows produces an empty string.
func (c Comprehension) HTML() string {
	var b strings.Builder
	c.writeHTML(&b)
	return b.String()
}

func (r Rendered) writeHTML(b *strings.Builder) {
	for i, s := range r.S {
		b.WriteString(s)
		if i < len(r.D) {
			writeDynamic(b, r.D[i])
		}
	}
}

func (c Comprehension) writeHTML(b *strings.Builder) {
	for _, row := range c.Rows {
		for i, s := range c.S {
			b.WriteString(s)
			if i < len(row.D) {
				writeDynamic(b, row.D[i])
			}
		}
	}
}

func writeDynamic(b *strings.Builder, v any) {
	switch x := v.(type) {
	case nil:
		// Untaken branch — emit nothing.
	case string:
		b.WriteString(x)
	case Rendered:
		x.writeHTML(b)
	case Comprehension:
		x.writeHTML(b)
	default:
		// Unexpected — only the four cases above are valid dynamics
		// in codegen output. Emit a visible marker so the bug is
		// noticed in dev rather than silently producing wrong HTML.
		fmt.Fprintf(b, "[!unsupported dynamic: %T]", v)
	}
}

// Equal reports whether two Rendereds produce identical HTML.
// Statics are compared as values (codegen typically shares the
// underlying array, in which case this is a fast pointer-equivalent
// check via length-then-byte comparison).
func (r Rendered) Equal(other Rendered) bool {
	if !staticsEqual(r.S, other.S) {
		return false
	}
	if len(r.D) != len(other.D) {
		return false
	}
	for i := range r.D {
		if !dynamicEqual(r.D[i], other.D[i]) {
			return false
		}
	}
	return true
}

// Equal reports whether two Comprehensions produce identical HTML.
// Rows must match in count, order, key, and per-row dynamics.
func (c Comprehension) Equal(other Comprehension) bool {
	if !staticsEqual(c.S, other.S) {
		return false
	}
	if len(c.Rows) != len(other.Rows) {
		return false
	}
	for i := range c.Rows {
		if c.Rows[i].Key != other.Rows[i].Key {
			return false
		}
		if !rowDynamicsEqual(c.Rows[i].D, other.Rows[i].D) {
			return false
		}
	}
	return true
}

func dynamicEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	switch ax := a.(type) {
	case string:
		bx, ok := b.(string)
		return ok && ax == bx
	case Rendered:
		bx, ok := b.(Rendered)
		return ok && ax.Equal(bx)
	case Comprehension:
		bx, ok := b.(Comprehension)
		return ok && ax.Equal(bx)
	}
	return false
}

func rowDynamicsEqual(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !dynamicEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func staticsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
