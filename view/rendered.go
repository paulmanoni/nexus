package view

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"strings"

	templruntime "github.com/a-h/templ/runtime"
)

// A live page renders into a tree, as Phoenix LiveView's engine does: the
// markup a template always writes (its statics) apart from what it fills in
// (its dynamics). The view compiler (viewgen) has the code templ generates
// tell a recorder where each static starts and ends, where a loop and its
// items are, and where a branch or a nested component begins:
//
//	frame  the statics of a stretch of template, and the dynamics between
//	       them: strings, nested frames, comprehensions
//	comp   a loop: one frame per item
//
// Statics are identified by their text (a frame's fingerprint), sent to a
// connection once and referred to after that, and only changed dynamics
// travel (rdiff.go). Whatever a template didn't compile with the recorder —
// a component written by hand, a library's — lands in the dynamic between
// two statics, as markup.

// recorderKey is where a live render keeps its recorder on the context.
type recorderKey struct{}

// recorder builds the tree of one render as the generated code writes it.
// Components write through w — templ's buffered writer, shared by every
// component of the render — into out.
type recorder struct {
	w     *templruntime.Buffer
	out   *bytes.Buffer
	mark  int        // where out's unrecorded tail starts
	stack []*recNode // open frames and loops, innermost last
}

// recNode is a frame (comp false) or a loop (comp true) being recorded.
type recNode struct {
	comp  bool
	parts []recPart  // a frame's statics and dynamics, in order
	items []*recNode // a loop's item frames
}

type recPart struct {
	static bool
	text   string   // a static, or a dynamic's markup
	node   *recNode // a nested frame or loop
}

// withRecorder starts recording a render into out: render into the
// returned writer, then call tree.
func withRecorder(ctx context.Context, out *bytes.Buffer) (context.Context, *recorder) {
	w, _ := templruntime.GetBuffer(out)
	r := &recorder{w: w, out: out, stack: []*recNode{{}}}
	return context.WithValue(ctx, recorderKey{}, r), r
}

// Rec is what generated code records a component's render through. Its zero
// value — a render that isn't a live page's, or one into another buffer —
// only writes.
type Rec struct{ r *recorder }

// Record returns the recorder of the live render ctx belongs to, when the
// component writes into its writer. Generated code calls it once per
// component; it is not for hand-written code.
func Record(ctx context.Context, w io.Writer) Rec {
	r, _ := ctx.Value(recorderKey{}).(*recorder)
	if r == nil || w != io.Writer(r.w) {
		return Rec{}
	}
	return Rec{r}
}

// S writes a static: s is the n-th static of its file, as templ numbers them.
func (c Rec) S(w io.Writer, n int, s string) error {
	if c.r == nil {
		return templruntime.WriteString(w, n, s)
	}
	c.r.flush()
	c.r.top().parts = append(c.r.top().parts, recPart{static: true, text: s})
	err := templruntime.WriteString(w, n, s)
	_ = c.r.w.Flush()
	c.r.mark = c.r.out.Len()
	return err
}

// Open starts a nested frame: a branch, or a component a template renders.
func (c Rec) Open(io.Writer) {
	if c.r == nil {
		return
	}
	c.r.flush()
	c.r.stack = append(c.r.stack, &recNode{})
}

// Close ends the frame Open started, as a dynamic of the frame around it.
func (c Rec) Close(io.Writer) {
	if c.r == nil || len(c.r.stack) < 2 || c.r.top().comp {
		return
	}
	c.r.flush()
	f := c.r.pop()
	c.r.top().parts = append(c.r.top().parts, recPart{node: f})
}

// ForStart starts a loop; Item starts each of its items; ForEnd ends it.
func (c Rec) ForStart(io.Writer) {
	if c.r == nil {
		return
	}
	c.r.flush()
	c.r.stack = append(c.r.stack, &recNode{comp: true})
}

func (c Rec) Item(io.Writer) {
	if c.r == nil {
		return
	}
	c.r.endItem()
	if c.r.top().comp {
		c.r.stack = append(c.r.stack, &recNode{})
	}
}

func (c Rec) ForEnd(io.Writer) {
	if c.r == nil {
		return
	}
	c.r.endItem()
	if !c.r.top().comp || len(c.r.stack) < 2 {
		return
	}
	loop := c.r.pop()
	c.r.top().parts = append(c.r.top().parts, recPart{node: loop})
}

func (r *recorder) top() *recNode { return r.stack[len(r.stack)-1] }

func (r *recorder) pop() *recNode {
	n := r.top()
	r.stack = r.stack[:len(r.stack)-1]
	return n
}

// endItem closes the open item frame of the innermost loop, if one is open.
func (r *recorder) endItem() {
	if len(r.stack) < 2 || r.top().comp || !r.stack[len(r.stack)-2].comp {
		return
	}
	r.flush()
	item := r.pop()
	r.top().items = append(r.top().items, item)
}

// flush records what was written since the last mark as a dynamic.
func (r *recorder) flush() {
	_ = r.w.Flush()
	if r.out.Len() > r.mark {
		r.top().parts = append(r.top().parts, recPart{text: string(r.out.Bytes()[r.mark:])})
		r.mark = r.out.Len()
	}
}

// tree finishes the render: the root frame, every open frame closed, and
// the writer back in templ's pool.
func (r *recorder) tree() *rframe {
	defer templruntime.ReleaseBuffer(r.w)
	for len(r.stack) > 1 {
		if r.top().comp {
			Rec{r}.ForEnd(r.w)
		} else {
			r.flush()
			f := r.pop()
			r.top().parts = append(r.top().parts, recPart{node: f})
		}
	}
	r.flush()
	return normalize(r.stack[0])
}

// rframe is a recorded frame in the shape it travels: len(s) == len(d)+1,
// each dynamic a string, an *rframe or an *rcomp.
type rframe struct {
	fp string // the fingerprint of s
	s  []string
	d  []any
}

type rcomp struct{ items []*rframe }

func normalize(n *recNode) *rframe {
	f := &rframe{s: []string{""}}
	for _, p := range n.parts {
		switch {
		case p.static:
			f.s[len(f.s)-1] += p.text
		case p.node != nil && p.node.comp:
			c := &rcomp{items: make([]*rframe, len(p.node.items))}
			for i, it := range p.node.items {
				c.items[i] = normalize(it)
			}
			f.d = append(f.d, c)
			f.s = append(f.s, "")
		case p.node != nil:
			f.d = append(f.d, flatten(normalize(p.node)))
			f.s = append(f.s, "")
		default:
			f.d = append(f.d, p.text)
			f.s = append(f.s, "")
		}
	}
	f.fp = fingerprint(f.s)
	return f
}

// flatten replaces a nested frame that only passes something through — a
// component or branch with no markup of its own around one dynamic, or
// nothing at all — with what it holds, so changes don't travel through it.
func flatten(f *rframe) any {
	switch {
	case len(f.d) == 0 && len(f.s) == 1:
		return f.s[0]
	case len(f.d) == 1 && f.s[0] == "" && f.s[1] == "":
		return f.d[0]
	}
	return f
}

func fingerprint(s []string) string {
	h := sha256.Sum256([]byte(strings.Join(s, "\x00")))
	return base64.RawURLEncoding.EncodeToString(h[:12])
}

// html renders a tree back into markup — what the browser does with it.
func (f *rframe) html() string {
	var b strings.Builder
	f.write(&b)
	return b.String()
}

func (f *rframe) write(b *strings.Builder) {
	for i, s := range f.s {
		b.WriteString(s)
		if i < len(f.d) {
			writeDyn(b, f.d[i])
		}
	}
}

func writeDyn(b *strings.Builder, d any) {
	switch d := d.(type) {
	case string:
		b.WriteString(d)
	case *rframe:
		d.write(b)
	case *rcomp:
		for _, it := range d.items {
			it.write(b)
		}
	}
}
