package view

import (
	"encoding/json"
	"strings"
)

// Live updates travel as patches against the page's previous render. Both
// renders are cut into tokens after every '>' — so a token is a tag, or the
// text before one — and diffed; a patch is a list of steps:
//
//	n  (a positive number)  copy the next n tokens of the old render
//	-n (a negative number)  skip the next n tokens of the old render
//	"…" (a string)          insert this text
//
// Counts are in tokens, never bytes, so the browser (UTF-16 strings) applies
// what the server (UTF-8) computed. Each patch carries the new render's token
// count; a browser that ends up elsewhere asks for the full render.

// tokenize cuts html after every '>'.
func tokenize(html string) []string {
	var out []string
	for len(html) > 0 {
		i := strings.IndexByte(html, '>')
		if i < 0 {
			out = append(out, html)
			break
		}
		out = append(out, html[:i+1])
		html = html[i+1:]
	}
	return out
}

// maxEdits bounds the diff's work: past it, a full render is cheaper.
const maxEdits = 2000

// diff returns the patch turning old into new, or ok false when the renders
// differ too much for a patch to be worth it.
func diff(old, new []string) (patch []any, ok bool) {
	edits, ok := myers(old, new)
	if !ok {
		return nil, false
	}
	var steps []any
	push := func(op byte, text string) {
		if n := len(steps); n > 0 {
			switch last := steps[n-1].(type) {
			case int:
				if op == '=' && last > 0 {
					steps[n-1] = last + 1
					return
				}
				if op == '-' && last < 0 {
					steps[n-1] = last - 1
					return
				}
			case string:
				if op == '+' {
					steps[n-1] = last + text
					return
				}
			}
		}
		switch op {
		case '=':
			steps = append(steps, 1)
		case '-':
			steps = append(steps, -1)
		default:
			steps = append(steps, text)
		}
	}
	for _, e := range edits {
		push(e.op, e.text)
	}
	return steps, true
}

// worthPatching reports whether sending patch beats sending the full html.
func worthPatching(patch []any, html string) bool {
	b, err := json.Marshal(patch)
	return err == nil && len(b) < len(html)*3/4
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

// apply is the browser's side of a patch, in Go — for tests.
func apply(old []string, patch []any) (string, bool) {
	var b strings.Builder
	i := 0
	for _, step := range patch {
		switch s := step.(type) {
		case int:
			if s > 0 {
				if i+s > len(old) {
					return "", false
				}
				for _, t := range old[i : i+s] {
					b.WriteString(t)
				}
				i += s
			} else {
				i -= s
			}
		case string:
			b.WriteString(s)
		}
	}
	return b.String(), i <= len(old)
}
