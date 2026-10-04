package view

import (
	"crypto/sha256"
	"encoding/base64"
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
	strs map[string]int // long markup → the id the browser keeps it under
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
	t.prev = root
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
	if id, ok := t.strs[s]; ok {
		return map[string]any{"r": id}
	}
	if len(t.strs) >= refMax {
		return s
	}
	id := len(t.strs)
	t.strs[s] = id
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
		old, ok := a.(string)
		if !ok {
			return t.str(b), true
		}
		if old == b {
			return nil, false
		}
		if _, kept := t.strs[b]; !kept && len(b) > 256 {
			if p, ok := stringPatch(old, b); ok {
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

func itemKeys(items []*rframe) []string {
	out := make([]string, len(items))
	for i, it := range items {
		h := sha256.Sum256([]byte(it.fp + "\x00" + it.html()))
		out[i] = base64.RawURLEncoding.EncodeToString(h[:12])
	}
	return out
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
