package view

import (
	"encoding/binary"
	"hash/maphash"
	"strconv"
)

// A live page's replies carry its render tree (rendered.go), or the change
// to the tree the browser holds. As JSON:
//
//	{"t": id, "s": [...], "d": [...]}  a frame: its statics' id, the statics
//	                                   themselves the first time a connection
//	                                   meets them, its dynamics
//	{"c": [frame, …]}                  a loop's items
//	"…"                                a dynamic's markup
//	{"r": id, "v": "…"}                long markup, kept by the connection
//	                                   under id; {"r": id} after that
//
// and the change from the previous tree:
//
//	{"u": {"3": change, …}}  the same frame, with the dynamics that changed
//	{"k": [step, …]}         a loop's items: n keeps the next n, -n drops
//	                         them, a frame inserts it, {"u": …} changes the
//	                         next item in place
//	{"p": [step, …]}         a long dynamic's markup, as a token patch
//	                         against what it was (diff.go)
//
// Anything else (a frame, a loop, a string) replaces the dynamic.

// treeDiffer is a connection's server side: the tree the browser holds and
// the statics it has been sent.
type treeDiffer struct {
	prev  *rframe
	ids   map[string]int // fingerprint → the id the browser knows its statics by
	strs  map[string]int // long markup's sig → the id the browser keeps it under
	spots *spotTable     // the spots of prev, for a tracked render to skip

	// What a diff under way gave ids, undone when it can't finish, and the
	// skipped spots it needed the markup of.
	newIDs, newStrs []string
	missing         []uint64
}

// Markup of at least refMinLen bytes is kept by the connection, up to
// refMax strings: a class list or a component the page shows again travels
// once.
const (
	refMinLen = 64
	refMax    = 4096
)

// reset forgets everything the browser held: the next reply is the whole
// tree with every static.
func (t *treeDiffer) reset() { t.prev, t.ids, t.strs, t.spots = nil, nil, nil, nil }

// next returns the reply for a new render: the change from the previous
// tree, or (full) the tree itself.
func (t *treeDiffer) next(root *rframe) (msg any, full bool) {
	msg, full, _ = t.nextSpots(root, nil)
	return msg, full
}

// nextSpots is next for a tracked render, whose spots table holds. When the
// reply would need the markup of spots the render skipped — a new frame
// around them, or the whole tree — it returns their keys and leaves the
// differ as it was: render them, and try again.
func (t *treeDiffer) nextSpots(root *rframe, table *spotTable) (msg any, full bool, missing []uint64) {
	if t.ids == nil {
		t.ids, t.strs = map[string]int{}, map[string]int{}
	}
	t.newIDs, t.newStrs, t.missing = t.newIDs[:0], t.newStrs[:0], nil
	prev := t.prev
	if prev == nil || prev.fp != root.fp {
		msg, full = t.frame(root), true
	} else if u := t.frameChange(prev, root); u == nil {
		msg = map[string]any{"u": map[string]any{}}
	} else {
		msg = u
	}
	if missing = t.missing; missing != nil {
		for _, k := range t.newIDs {
			delete(t.ids, k)
		}
		for _, k := range t.newStrs {
			delete(t.strs, k)
		}
		return nil, false, missing
	}
	t.prev, t.spots = shadow(root, table), table
	if table != nil {
		table.keyed = nil
	}
	return msg, full, nil
}

// frame encodes a whole frame.
func (t *treeDiffer) frame(f *rframe) map[string]any {
	id, known := t.ids[f.fp]
	out := map[string]any{}
	if !known {
		id = len(t.ids)
		t.ids[f.fp] = id
		t.newIDs = append(t.newIDs, f.fp)
		out["s"] = f.s
	}
	out["t"] = id
	d := make([]any, len(f.d))
	for i, v := range f.d {
		d[i] = t.dyn(v)
	}
	out["d"] = d
	return out
}

func (t *treeDiffer) dyn(v any) any {
	switch v := v.(type) {
	case *rframe:
		return t.frame(v)
	case *rcomp:
		items := make([]any, len(v.items))
		for i, it := range v.items {
			items[i] = t.frame(it)
		}
		return map[string]any{"c": items}
	case string:
		return t.str(v)
	case *kept:
		t.missing = append(t.missing, v.key)
		return nil
	}
	return v
}

// str encodes markup: long markup by reference once the browser keeps it.
func (t *treeDiffer) str(s string) any {
	if len(s) < refMinLen {
		return s
	}
	k := sig(s)
	if id, ok := t.strs[k]; ok {
		return map[string]any{"r": id}
	}
	if len(t.strs) >= refMax {
		return s
	}
	id := len(t.strs)
	t.strs[k] = id
	t.newStrs = append(t.newStrs, k)
	return map[string]any{"r": id, "v": s}
}

// frameChange is the change from a to b, frames with the same statics: nil
// when nothing changed.
func (t *treeDiffer) frameChange(a, b *rframe) map[string]any {
	u := map[string]any{}
	for i := range b.d {
		if c, changed := t.dynChange(a.d[i], b.d[i]); changed {
			u[strconv.Itoa(i)] = c
		}
	}
	if len(u) == 0 {
		return nil
	}
	return map[string]any{"u": u}
}

func (t *treeDiffer) dynChange(a, b any) (any, bool) {
	switch b := b.(type) {
	case *kept:
		if sameShadow(a, b.s.value) {
			return nil, false
		}
		t.missing = append(t.missing, b.key)
		return nil, true
	case string:
		old, ok := a.(strSig)
		if !ok {
			return t.str(b), true
		}
		k := sigArr(b)
		if old.h == k {
			return nil, false
		}
		if _, kept := t.strs[string(k[:])]; !kept && old.long != "" && len(b) > longMarkup {
			if p, ok := stringPatch(old.long, b); ok {
				return map[string]any{"p": p}, true
			}
		}
		return t.str(b), true
	case *rframe:
		old, ok := a.(*rframe)
		if !ok || old.fp != b.fp {
			return t.frame(b), true
		}
		u := t.frameChange(old, b)
		return u, u != nil
	case *rcomp:
		old, ok := a.(*rcomp)
		if !ok {
			return t.dyn(b), true
		}
		return t.compChange(old, b)
	}
	return b, true
}

// compChange diffs a loop's items, keeping the ones that stayed.
func (t *treeDiffer) compChange(a, b *rcomp) (any, bool) {
	ak, bk := itemKeys(a.items), itemKeys(b.items)
	edits, ok := myers(ak, bk)
	if !ok {
		return t.dyn(b), true
	}
	var steps []any
	add := func(n int) {
		if k := len(steps); k > 0 {
			if c, ok := steps[k-1].(int); ok && (c > 0) == (n > 0) {
				steps[k-1] = c + n
				return
			}
		}
		steps = append(steps, n)
	}
	oi, ni := 0, 0
	changed := false
	for i := 0; i < len(edits); {
		if edits[i].op == '=' {
			add(1)
			oi, ni, i = oi+1, ni+1, i+1
			continue
		}
		changed = true
		var dels, ins int
		for i < len(edits) && edits[i].op == '-' {
			dels, i = dels+1, i+1
		}
		for i < len(edits) && edits[i].op == '+' {
			ins, i = ins+1, i+1
		}
		// A removed item followed by an added one with the same statics is
		// that item changed in place.
		for dels > 0 && ins > 0 && a.items[oi].fp == b.items[ni].fp {
			if u := t.frameChange(a.items[oi], b.items[ni]); u != nil {
				steps = append(steps, u)
			} else {
				add(1)
			}
			oi, ni, dels, ins = oi+1, ni+1, dels-1, ins-1
		}
		if dels > 0 {
			add(-dels)
			oi += dels
		}
		for ; ins > 0; ins-- {
			steps = append(steps, t.frame(b.items[ni]))
			ni++
		}
	}
	if !changed {
		return nil, false
	}
	return map[string]any{"k": steps}, true
}

// itemKeys identify a loop's items by their statics and dynamics: two items
// with the same key render the same.
func itemKeys(items []*rframe) []string {
	out := make([]string, len(items))
	var a, b maphash.Hash
	a.SetSeed(fpSeeds[0])
	b.SetSeed(fpSeeds[1])
	for i, it := range items {
		a.Reset()
		b.Reset()
		hashFrame(&a, it)
		hashFrame(&b, it)
		out[i] = sum128(&a, &b)
	}
	return out
}

func hashFrame(h *maphash.Hash, f *rframe) {
	h.WriteString(f.fp)
	for _, d := range f.d {
		hashDyn(h, d)
	}
}

// hashDyn hashes a dynamic the same whether it is rendered, a shadow, or a
// skipped spot standing for its shadow.
func hashDyn(h *maphash.Hash, d any) {
	switch d := d.(type) {
	case string:
		h.WriteByte(1)
		k := sigArr(d)
		h.Write(k[:])
	case strSig:
		h.WriteByte(1)
		h.Write(d.h[:])
	case *rframe:
		h.WriteByte(2)
		hashFrame(h, d)
	case *rcomp:
		h.WriteByte(3)
		for _, it := range d.items {
			hashFrame(h, it)
		}
		h.WriteByte(4)
	case *kept:
		hashDyn(h, d.s.value)
	}
}

// dynSig is hashDyn's sum.
func dynSig(d any) string {
	var a, b maphash.Hash
	a.SetSeed(fpSeeds[0])
	b.SetSeed(fpSeeds[1])
	hashDyn(&a, d)
	hashDyn(&b, d)
	return sum128(&a, &b)
}

// sameShadow reports whether a shadow is the one a skipped spot stands for.
func sameShadow(a, b any) bool {
	switch a := a.(type) {
	case strSig:
		bs, ok := b.(strSig)
		return ok && a.h == bs.h
	case *rframe:
		if bf, ok := b.(*rframe); ok && a == bf {
			return true
		}
	case *rcomp:
		if bc, ok := b.(*rcomp); ok && a == bc {
			return true
		}
	}
	return b != nil && dynSig(a) == dynSig(b)
}

// stringPatch is the token patch from old to new markup, when it is clearly
// smaller than new.
func stringPatch(old, new string) ([]any, bool) {
	d := differ{last: tokenize(old)}
	p, ok := d.diff(tokenize(new))
	if !ok || !worthPatching(p, new) {
		return nil, false
	}
	return p, true
}

// The tree a connection keeps for its next diff is a shadow of the last
// render: frames and loops as they were, each dynamic string as its sig —
// enough to tell what changed — and only long markup kept whole, for token
// patches. A page holds a few bytes per dynamic, not a copy of its markup.

const longMarkup = 1024

type strSig struct {
	h    [16]byte // sig of the markup
	long string   // the markup itself, when longer than longMarkup
}

// emptyShadow is every empty dynamic's shadow: one value, not one each.
var emptyShadow any = strSig{h: sigArr("")}

// sig is a 128-bit maphash of s, as a string.
func sig(s string) string {
	k := sigArr(s)
	return string(k[:])
}

// sigArr is sig as an array: no allocation.
func sigArr(s string) [16]byte {
	var out [16]byte
	binary.LittleEndian.PutUint64(out[:8], maphash.String(fpSeeds[0], s))
	binary.LittleEndian.PutUint64(out[8:], maphash.String(fpSeeds[1], s))
	return out
}

// shadow also gives each spot of table the shadow it rendered.
func shadow(f *rframe, table *spotTable) *rframe {
	out := &rframe{fp: f.fp, d: make([]any, len(f.d))}
	for i, d := range f.d {
		switch d := d.(type) {
		case string:
			if d == "" {
				out.d[i] = emptyShadow
				continue
			}
			ss := strSig{h: sigArr(d)}
			if len(d) > longMarkup {
				ss.long = d
			}
			out.d[i] = ss
		case *rframe:
			out.d[i] = shadow(d, table)
		case *rcomp:
			c := &rcomp{items: make([]*rframe, len(d.items))}
			for j, it := range d.items {
				c.items[j] = shadow(it, table)
			}
			out.d[i] = c
		case *kept:
			out.d[i] = d.s.value
		}
	}
	if table != nil {
		for _, k := range table.keyed[f] {
			if s := table.spots[k.key]; s != nil {
				s.value = out.d[k.i]
			}
		}
	}
	return out
}
