package view

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/maphash"
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

	// A tracked render (assign.go) also records the page's spots — the
	// parts of templates Guard opens — into table, and skips a spot of prev
	// whose reads are not in changed.
	track   *tracker
	table   *spotTable
	prev    *spotTable
	changed uint64
	again   map[uint64]bool // spots to render although unchanged
	gen     uint64          // the page render this is, for the components it keeps

	streamNext bool // the next loop is a view.Stream's
}

// recNode is a frame (comp false) or a loop (comp true) being recorded.
type recNode struct {
	comp      bool
	ephemeral bool       // a loop over a view.Stream's Items
	parts     []recPart  // a frame's statics and dynamics, in order
	items     []*recNode // a loop's item frames

	key    uint64 // its place on the page: its parent's key and its own
	kids   int    // the frames and loops opened in it so far
	reads  uint64 // the Assigns read in it, its children's included
	taint  bool   // it can't be skipped: it made a signal, holds a shard, …
	nested []uint64

	guarded  bool         // a spot: Guard opened it
	boundary bool         // a live component: its Assigns are its own
	comps    []*component // the live components in it
	label    string       // the spot's place in the template
	kept     *spot        // the spot was skipped: its previous render stands
	scope    *scope       // the component scope it ran in, and where its log was
	logStart int
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
	top := c.r.top()
	if n := len(top.parts); n > 0 && top.parts[n-1].static {
		// Nothing was written since the last static: an empty dynamic, so
		// the frame's statics don't depend on what its values are.
		top.parts = append(top.parts, recPart{})
	}
	top.parts = append(top.parts, recPart{static: true, text: s})
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
	c.r.push(&recNode{}, 'o')
}

// Guard starts a spot: a nested frame, as Open does, that a tracked render
// may skip. The compiler guards a part of a template that reads nothing of
// the template's own variables — only what it reaches through recv, the
// component's receiver (nil when it uses none), and ctx. When none of the
// Assigns it read last time has changed, Guard reports false: the code is
// not run, and Close keeps what the spot rendered before. label names the
// spot in its file.
func (c Rec) Guard(ctx context.Context, w io.Writer, label string, recv any) bool {
	r := c.r
	if r == nil {
		return true
	}
	r.flush()
	n := &recNode{guarded: r.table != nil, label: label}
	n.key = spotKey(r.top().key, label)
	r.stack = append(r.stack, n)
	if !n.guarded {
		return true
	}
	if (recv != nil && recv != r.track.page) || r.track.off != "" {
		// Another value's method, or an untracked component: what it reads
		// isn't tracked.
		n.taint = true
		return true
	}
	sc := scopeFrom(ctx)
	if r.prev != nil {
		if s := r.prev.spots[n.key]; s != nil && s.value != nil && !s.taint && s.label == label && s.reads&r.changed == 0 && !r.again[n.key] && clean(s.comps) {
			n.kept, n.reads = s, s.reads
			r.carry(s, n.key)
			if sc != nil {
				replay(sc, s.effects)
			}
			return false
		}
	}
	if sc != nil {
		n.scope, n.logStart = sc, len(sc.log)
	}
	return true
}

// Close ends the frame Open or Guard started, as a dynamic of the frame
// around it.
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
	c.r.push(&recNode{comp: true, ephemeral: c.r.streamNext}, 'f')
	c.r.streamNext = false
}

// streamLoop marks the loop being recorded — or, before it starts, the
// next one — as a view.Stream's.
func (r *recorder) streamLoop() {
	if n := len(r.stack); n > 0 && r.stack[n-1].comp {
		r.stack[n-1].ephemeral = true
		return
	}
	r.streamNext = true
}

func (c Rec) Item(io.Writer) {
	if c.r == nil {
		return
	}
	c.r.endItem()
	if loop := c.r.top(); loop.comp {
		c.r.stack = append(c.r.stack, &recNode{key: mix(loop.key, uint64(len(loop.items)))})
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

// push opens n inside the innermost node, keyed by its order there.
func (r *recorder) push(n *recNode, kind byte) {
	p := r.top()
	n.key = mix(p.key, uint64(kind)<<56|uint64(p.kids))
	p.kids++
	r.stack = append(r.stack, n)
}

// pop closes the innermost node: a spot it rendered goes into the table,
// and what it read and holds into its parent.
func (r *recorder) pop() *recNode {
	n := r.top()
	r.stack = r.stack[:len(r.stack)-1]
	if r.table == nil {
		return n
	}
	if n.guarded && n.kept == nil {
		s := &spot{label: n.label, reads: n.reads, taint: n.taint, nested: n.nested, comps: n.comps}
		if n.scope != nil && len(n.scope.log) > n.logStart {
			s.effects = append([]string(nil), n.scope.log[n.logStart:]...)
		}
		r.table.spots[n.key] = s
	}
	p := r.top()
	if n.boundary {
		p.reads |= n.reads & errsBit // the rest are the component's own
	} else {
		p.reads |= n.reads
	}
	p.taint = p.taint || n.taint
	p.comps = append(p.comps, n.comps...)
	if n.guarded {
		p.nested = append(p.nested, n.key)
	} else {
		p.nested = append(p.nested, n.nested...)
	}
	return n
}

// read notes that the part of the page being rendered read an Assign.
func (r *recorder) read(bit uint64) { r.top().reads |= bit }

// carry moves a skipped spot, and the spots inside it, into the new table.
func (r *recorder) carry(s *spot, key uint64) {
	r.table.spots[key] = s
	r.table.kept++
	for _, c := range s.comps {
		c.seen = r.gen
	}
	for _, k := range s.nested {
		if inner := r.prev.spots[k]; inner != nil {
			r.carry(inner, k)
		}
	}
}

// replay applies a skipped spot's effects on the scope it ran in: the
// components it entered and the signals it made.
func replay(sc *scope, effects []string) {
	for _, name := range effects {
		if name == "" {
			sc.states++
			sc.log = append(sc.log, "")
		} else {
			sc.enter(name)
		}
	}
}

// clean reports whether none of comps changed since it last rendered.
func clean(comps []*component) bool {
	for _, c := range comps {
		if c.dirty() {
			return false
		}
	}
	return true
}

// static writes s as a static of the frame being recorded, for markup the
// view package itself writes around a component.
func (c Rec) static(w io.Writer, s string) error {
	if c.r == nil {
		_, err := io.WriteString(w, s)
		return err
	}
	c.r.flush()
	top := c.r.top()
	if n := len(top.parts); n > 0 && top.parts[n-1].static {
		top.parts = append(top.parts, recPart{})
	}
	top.parts = append(top.parts, recPart{static: true, text: s})
	_, err := io.WriteString(c.r.w, s)
	_ = c.r.w.Flush()
	c.r.mark = c.r.out.Len()
	return err
}

// taintFrom marks the part of the page ctx renders as one that can't be
// skipped: it opens a shard, which the render around it keeps track of.
func taintFrom(ctx context.Context) {
	if r, _ := ctx.Value(recorderKey{}).(*recorder); r != nil && len(r.stack) > 0 {
		r.top().taint = true
	}
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
	var keyed map[*rframe][]keyAt
	if r.table != nil {
		keyed = map[*rframe][]keyAt{}
		r.table.keyed = keyed
	}
	return normalize(r.stack[0], keyed)
}

// rframe is a recorded frame in the shape it travels: len(s) == len(d)+1,
// each dynamic a string, an *rframe or an *rcomp — or, in a tracked render,
// a *kept spot.
type rframe struct {
	fp string // the fingerprint of s
	s  []string
	d  []any
}

// keyAt says that dynamic i is the spot key.
type keyAt struct {
	i   int
	key uint64
}

type rcomp struct {
	items []*rframe
	// ephemeral: a view.Stream's latest change — the browser keeps the rows,
	// so the connection keeps only their hash (rdiff.go).
	ephemeral bool
}

// kept is a spot a tracked render skipped: what it rendered before stands.
type kept struct {
	s   *spot
	key uint64
}

// normalize turns a recorded node into a frame. keyed, for a tracked
// render, collects which dynamics of each frame are spots.
func normalize(n *recNode, keyed map[*rframe][]keyAt) *rframe {
	f := &rframe{s: []string{""}}
	key := func(i int, k uint64) {
		if keyed != nil {
			keyed[f] = append(keyed[f], keyAt{i, k})
		}
	}
	for _, p := range n.parts {
		switch {
		case p.static:
			f.s[len(f.s)-1] += p.text
			continue
		case p.node != nil && p.node.comp:
			c := &rcomp{items: make([]*rframe, len(p.node.items)), ephemeral: p.node.ephemeral}
			for i, it := range p.node.items {
				c.items[i] = normalize(it, keyed)
			}
			f.d = append(f.d, c)
		case p.node != nil && p.node.kept != nil:
			key(len(f.d), p.node.key)
			f.d = append(f.d, &kept{p.node.kept, p.node.key})
		case p.node != nil:
			inner := normalize(p.node, keyed)
			v := flatten(inner)
			if p.node.guarded {
				key(len(f.d), p.node.key)
			}
			if keyed != nil && len(inner.d) == 1 && v == inner.d[0] {
				// The frame passed its one dynamic through: so do its spots.
				for _, k := range keyed[inner] {
					key(len(f.d), k.key)
				}
				delete(keyed, inner)
			}
			f.d = append(f.d, v)
		default:
			f.d = append(f.d, p.text)
		}
		f.s = append(f.s, "")
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

// spotTable is what a tracked render recorded of its spots, for the next
// to skip the ones that didn't change: kept by the connection beside the
// tree the browser holds.
type spotTable struct {
	owner *tracker
	epoch uint64 // the owner's version counter when it rendered
	spots map[uint64]*spot
	kept  int // the spots it skipped
	// keyed says which dynamics of the render's frames are spots, until
	// the render is diffed.
	keyed map[*rframe][]keyAt
}

// spot is one part of the page as last rendered.
type spot struct {
	label   string
	reads   uint64
	taint   bool
	nested  []uint64     // the spots directly inside it
	comps   []*component // the live components in it: it is stale when one changed
	effects []string     // the components it entered and signals it made in its scope, to replay
	value   any          // its shadow (rdiff.go), once the render is diffed
}

func spotKey(parent uint64, label string) uint64 {
	return mix(parent, maphash.String(fpSeeds[0], label))
}

// mix combines a parent key with a child's (splitmix64).
func mix(a, b uint64) uint64 {
	z := a*0x9e3779b97f4a7c15 + b + 0x632be59bd9b4e019
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// fingerprint identifies a frame's statics within the process: 128 bits of
// maphash, two seeds — fast, and never sent or compared across processes.
func fingerprint(s []string) string {
	var a, b maphash.Hash
	a.SetSeed(fpSeeds[0])
	b.SetSeed(fpSeeds[1])
	for _, x := range s {
		a.WriteString(x)
		a.WriteByte(0)
		b.WriteString(x)
		b.WriteByte(0)
	}
	return sum128(&a, &b)
}

var fpSeeds = [2]maphash.Seed{maphash.MakeSeed(), maphash.MakeSeed()}

func sum128(a, b *maphash.Hash) string {
	var out [16]byte
	binary.LittleEndian.PutUint64(out[:8], a.Sum64())
	binary.LittleEndian.PutUint64(out[8:], b.Sum64())
	return string(out[:])
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
	case *kept:
		panic("view: a skipped spot has no markup")
	}
}
