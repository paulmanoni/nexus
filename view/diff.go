package view

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Live updates travel as patches against the page's previous render. Both
// renders are cut into tokens — before every '<', after every '>', '"' and
// "&#34;" — so a text node, an attribute value and a view.Send argument are
// tokens of their own,
// the parts a template fills in (LiveView's dynamics). The token lists are
// diffed; a patch is a list of steps:
//
//	n      (a positive number)  copy the next n tokens of the old render
//	-n     (a negative number)  skip the next n tokens of the old render
//	"…"    (a string)           insert this text
//	[p, n] (a pair)             insert n tokens copied from position p of the
//	                            old render — a new list item reuses the tags
//	                            of the items already there, so only its
//	                            values travel (LiveView's shared statics)
//	[id]   (a single number)    insert the long token the connection's
//	                            dictionary holds under id — markup sent once
//	                            is never sent again, even after it left the
//	                            page (LiveView's template fingerprints)
//
// Counts are in tokens, never bytes, so the browser (UTF-16 strings) applies
// what the server (UTF-8) computed. Each patch carries the new render's token
// count; a browser that ends up elsewhere asks for the full render. Both
// sides build the dictionary the same way: every token of at least
// dictMinLen bytes, in order of first appearance, up to dictMax entries,
// reset whenever a full render is sent.

const (
	maxEdits   = 2000 // past this many edits a full render is cheaper
	dictMinLen = 16
	dictMax    = 4096
)

// tokenize cuts html before every '<' and after every '>', '"' and "&#34;"
// (an escaped quote: view.Send's arguments are JSON inside an attribute, so
// each argument becomes a token of its own).
func tokenize(html string) []string {
	var out []string
	start := 0
	for i := 0; i < len(html); i++ {
		switch html[i] {
		case '<':
			if i > start {
				out = append(out, html[start:i])
				start = i
			}
		case '>', '"':
			out = append(out, html[start:i+1])
			start = i + 1
		case ';':
			if i >= 4 && html[i-4:i+1] == "&#34;" {
				out = append(out, html[start:i+1])
				start = i + 1
			}
		}
	}
	if start < len(html) {
		out = append(out, html[start:])
	}
	return out
}

// dictionary is the connection's memory of long tokens, kept identically
// by the server and the browser.
type dictionary struct {
	ids   map[string]int
	words []string
}

func (d *dictionary) reset() { d.ids, d.words = map[string]int{}, nil }

func (d *dictionary) observe(tokens []string) {
	if d.ids == nil {
		d.ids = map[string]int{}
	}
	for _, t := range tokens {
		if len(t) < dictMinLen || len(d.words) >= dictMax {
			continue
		}
		if _, ok := d.ids[t]; !ok {
			d.ids[t] = len(d.words)
			d.words = append(d.words, t)
		}
	}
}

// differ is one connection's server side: the render the browser holds and
// the dictionary it shares.
type differ struct {
	last []string
	dict dictionary
}

// next returns the reply body for a new render: a patch when the browser
// holds a previous render and the patch is clearly smaller, else the full
// render (which resets the dictionary on both sides).
func (d *differ) next(html string) (patch []any, n int, full bool) {
	tokens := tokenize(html)
	if d.last != nil {
		if p, ok := d.diff(tokens); ok && worthPatching(p, html) {
			d.last = tokens
			d.dict.observe(tokens)
			return p, len(tokens), false
		}
	}
	d.last = tokens
	d.dict.reset()
	d.dict.observe(tokens)
	return nil, 0, true
}

// forget drops the browser's render: the next reply is a full render.
func (d *differ) forget() { d.last = nil }

// diff returns the patch from the held render to tokens, or ok false when
// they differ too much for a patch to be worth it.
func (d *differ) diff(tokens []string) ([]any, bool) {
	edits, ok := myers(d.last, tokens)
	if !ok {
		return nil, false
	}
	var steps []any
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			steps = append(steps, d.references(lit.String())...)
			lit.Reset()
		}
	}
	for _, e := range edits {
		switch e.op {
		case '+':
			lit.WriteString(e.text)
			continue
		case '=':
			flush()
			if n := len(steps); n > 0 {
				if c, ok := steps[n-1].(int); ok && c > 0 {
					steps[n-1] = c + 1
					continue
				}
			}
			steps = append(steps, 1)
		case '-':
			flush()
			if n := len(steps); n > 0 {
				if c, ok := steps[n-1].(int); ok && c < 0 {
					steps[n-1] = c - 1
					continue
				}
			}
			steps = append(steps, -1)
		}
	}
	flush()
	return steps, true
}

// references encodes inserted text as literals, runs of the held render
// ([p, n]) and dictionary entries ([id]) — whichever is shortest.
func (d *differ) references(text string) []any {
	index := map[string][]int{}
	for i, t := range d.last {
		if len(index[t]) < 32 { // enough candidates for repeated markup
			index[t] = append(index[t], i)
		}
	}
	toks := tokenize(text)
	var out []any
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			out = append(out, lit.String())
			lit.Reset()
		}
	}
	for i := 0; i < len(toks); {
		bestP, bestN, bestBytes := 0, 0, 0
		for _, p := range index[toks[i]] {
			n, size := 0, 0
			for p+n < len(d.last) && i+n < len(toks) && d.last[p+n] == toks[i+n] {
				size += len(toks[i+n])
				n++
			}
			if size > bestBytes {
				bestP, bestN, bestBytes = p, n, size
			}
		}
		// A reference costs about as much as "[1234,12]": take it only when
		// it replaces more text than that.
		if bestBytes > 12 {
			flush()
			out = append(out, []int{bestP, bestN})
			i += bestN
			continue
		}
		if id, ok := d.dict.ids[toks[i]]; ok {
			flush()
			out = append(out, []int{id})
			i++
			continue
		}
		lit.WriteString(toks[i])
		i++
	}
	flush()
	return out
}

// worthPatching reports whether sending patch beats sending the full html.
func worthPatching(patch []any, html string) bool {
	b, err := marshal(patch)
	return err == nil && len(b) < len(html)*3/4
}

// marshal encodes JSON without HTML-escaping: markup travels as markup, not
// as <…> (a third of a patch's bytes otherwise).
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

type edit struct {
	op   byte // '=', '-', '+'
	text string
}

// myers is the O(ND) shortest edit script (Myers 1986) over tokens.
func myers(a, b []string) ([]edit, bool) {
	n, m := len(a), len(b)
	max := n + m
	if max == 0 {
		return nil, true
	}
	limit := max
	if limit > maxEdits {
		limit = maxEdits
	}
	offset := max
	v := make([]int, 2*max+2)
	var trace [][]int
	for d := 0; d <= limit; d++ {
		snapshot := make([]int, len(v))
		copy(snapshot, v)
		trace = append(trace, snapshot)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1] // down: an insertion
			} else {
				x = v[offset+k-1] + 1 // right: a deletion
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				return backtrack(a, b, trace, offset, d), true
			}
		}
	}
	return nil, false
}

func backtrack(a, b []string, trace [][]int, offset, d int) []edit {
	x, y := len(a), len(b)
	var rev []edit
	for ; d > 0; d-- {
		v := trace[d]
		k := x - y
		var prevK int
		if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[offset+prevK]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			rev = append(rev, edit{op: '='})
		}
		if x == prevX {
			y--
			rev = append(rev, edit{op: '+', text: b[y]})
		} else {
			x--
			rev = append(rev, edit{op: '-'})
		}
	}
	for x > 0 && y > 0 {
		x--
		y--
		rev = append(rev, edit{op: '='})
	}
	out := make([]edit, len(rev))
	for i := range rev {
		out[i] = rev[len(rev)-1-i]
	}
	return out
}

// mirror is the browser's side of a connection, in Go — for tests.
type mirror struct {
	last []string
	dict dictionary
}

func (m *mirror) full(html string) {
	m.last = tokenize(html)
	m.dict.reset()
	m.dict.observe(m.last)
}

// apply applies a patch (steps decoded from JSON or built in Go).
func (m *mirror) apply(patch []any, n int) (string, bool) {
	var out []string
	i := 0
	for _, step := range patch {
		switch s := step.(type) {
		case int:
			if s > 0 {
				if i+s > len(m.last) {
					return "", false
				}
				out = append(out, m.last[i:i+s]...)
				i += s
			} else {
				i -= s
			}
		case string:
			out = append(out, tokenize(s)...)
		case []int:
			switch len(s) {
			case 1:
				if s[0] >= len(m.dict.words) {
					return "", false
				}
				out = append(out, m.dict.words[s[0]])
			case 2:
				if s[0] < 0 || s[0]+s[1] > len(m.last) {
					return "", false
				}
				out = append(out, m.last[s[0]:s[0]+s[1]]...)
			}
		}
	}
	if len(out) != n {
		return "", false
	}
	m.last = out
	m.dict.observe(out)
	return strings.Join(out, ""), true
}
