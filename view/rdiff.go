package view

import (
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
	prev *rframe
	ids  map[string]int // fingerprint → the id the browser knows its statics by
	strs map[string]int // long markup's sig → the id the browser keeps it under
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
func (t *treeDiffer) reset() { t.prev, t.ids, t.strs = nil, nil, nil }

// next returns the reply for a new render: the change from the previous
// tree, or (full) the tree itself.
func (t *treeDiffer) next(root *rframe) (msg any, full bool) {
	if t.ids == nil {
		t.ids, t.strs = map[string]int{}, map[string]int{}
	}
	prev := t.prev
	defer func() { t.prev = shadow(root) }()
	if prev == nil || prev.fp != root.fp {
		return t.frame(root), true
	}
	u := t.frameChange(prev, root)
	if u == nil {
		return map[string]any{"u": map[string]any{}}, false
	}
	return u, false
}

// frame encodes a whole frame.
func (t *treeDiffer) frame(f *rframe) map[string]any {
	id, known := t.ids[f.fp]
	out := map[string]any{}
	if !known {
		id = len(t.ids)
		t.ids[f.fp] = id
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
	case string:
		old, ok := a.(strSig)
		if !ok {
			return t.str(b), true
		}
		k := sig(b)
		if old.h == k {
			return nil, false
		}
		if _, kept := t.strs[k]; !kept && old.long != "" && len(b) > longMarkup {
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
		switch d := d.(type) {
		case string:
			h.WriteByte(1)
			h.WriteString(sig(d))
		case strSig:
			h.WriteByte(1)
			h.WriteString(d.h)
		case *rframe:
			h.WriteByte(2)
			hashFrame(h, d)
		case *rcomp:
			h.WriteByte(3)
			for _, it := range d.items {
				hashFrame(h, it)
			}
			h.WriteByte(4)
		}
	}
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
	h    string // sig of the markup
	long string // the markup itself, when longer than longMarkup
}

// sig is a 128-bit maphash of s, as a string.
func sig(s string) string {
	var a, b maphash.Hash
	a.SetSeed(fpSeeds[0])
	b.SetSeed(fpSeeds[1])
	a.WriteString(s)
	b.WriteString(s)
	return sum128(&a, &b)
}

func shadow(f *rframe) *rframe {
	out := &rframe{fp: f.fp, d: make([]any, len(f.d))}
	for i, d := range f.d {
		switch d := d.(type) {
		case string:
			ss := strSig{h: sig(d)}
			if len(d) > longMarkup {
				ss.long = d
			}
			out.d[i] = ss
		case *rframe:
			out.d[i] = shadow(d)
		case *rcomp:
			c := &rcomp{items: make([]*rframe, len(d.items))}
			for j, it := range d.items {
				c.items[j] = shadow(it)
			}
			out.d[i] = c
		}
	}
	return out
}
