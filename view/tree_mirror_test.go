package view

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// treeMirror is the browser's side of a connection's render trees, in Go —
// what runtime.js does with a reply, for tests.
type treeMirror struct {
	statics map[int][]string
	strs    map[int]string
	root    *mframe
}

type mframe struct {
	t int
	d []any // string, *mframe, *mcomp
}

type mcomp struct{ items []*mframe }

// apply takes a reply's tree (decoded JSON) and returns the page's markup.
func (m *treeMirror) apply(tree any, full, reset bool) (string, error) {
	if reset || m.statics == nil {
		m.statics, m.strs = map[int][]string{}, map[int]string{}
	}
	obj, ok := tree.(map[string]any)
	if !ok {
		return "", fmt.Errorf("tree is %T", tree)
	}
	if full {
		f, err := m.frame(obj)
		if err != nil {
			return "", err
		}
		m.root = f
	} else {
		if m.root == nil {
			return "", fmt.Errorf("a change with no tree")
		}
		if err := m.update(m.root, obj); err != nil {
			return "", err
		}
	}
	var b strings.Builder
	if err := m.write(&b, m.root); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (m *treeMirror) frame(obj map[string]any) (*mframe, error) {
	t := int(obj["t"].(float64))
	if s, ok := obj["s"].([]any); ok {
		ss := make([]string, len(s))
		for i, v := range s {
			ss[i] = v.(string)
		}
		m.statics[t] = ss
	}
	f := &mframe{t: t}
	for _, v := range obj["d"].([]any) {
		d, err := m.dyn(v)
		if err != nil {
			return nil, err
		}
		f.d = append(f.d, d)
	}
	return f, nil
}

func (m *treeMirror) dyn(v any) (any, error) {
	switch v := v.(type) {
	case string:
		return v, nil
	case map[string]any:
		if r, ok := v["r"].(float64); ok {
			if s, ok := v["v"].(string); ok {
				m.strs[int(r)] = s
			}
			s, ok := m.strs[int(r)]
			if !ok {
				return nil, fmt.Errorf("no string %v", r)
			}
			return s, nil
		}
		if items, ok := v["c"].([]any); ok {
			c := &mcomp{}
			for _, it := range items {
				f, err := m.frame(it.(map[string]any))
				if err != nil {
					return nil, err
				}
				c.items = append(c.items, f)
			}
			return c, nil
		}
		return m.frame(v)
	}
	return nil, fmt.Errorf("dynamic %T", v)
}

func (m *treeMirror) update(f *mframe, obj map[string]any) error {
	for k, change := range obj["u"].(map[string]any) {
		i, err := strconv.Atoi(k)
		if err != nil || i >= len(f.d) {
			return fmt.Errorf("no dynamic %s", k)
		}
		nv, err := m.change(f.d[i], change)
		if err != nil {
			return err
		}
		f.d[i] = nv
	}
	return nil
}

func (m *treeMirror) change(old, change any) (any, error) {
	obj, ok := change.(map[string]any)
	if !ok {
		return m.dyn(change)
	}
	switch {
	case obj["u"] != nil:
		f, ok := old.(*mframe)
		if !ok {
			return nil, fmt.Errorf("an update of %T", old)
		}
		return f, m.update(f, obj)
	case obj["k"] != nil:
		c, ok := old.(*mcomp)
		if !ok {
			return nil, fmt.Errorf("item steps on %T", old)
		}
		var out []*mframe
		i := 0
		for _, step := range obj["k"].([]any) {
			switch s := step.(type) {
			case float64:
				if n := int(s); n > 0 {
					out = append(out, c.items[i:i+n]...)
					i += n
				} else {
					i -= n
				}
			case map[string]any:
				if s["u"] != nil {
					if err := m.update(c.items[i], s); err != nil {
						return nil, err
					}
					out = append(out, c.items[i])
					i++
				} else {
					f, err := m.frame(s)
					if err != nil {
						return nil, err
					}
					out = append(out, f)
				}
			}
		}
		c.items = out
		return c, nil
	case obj["p"] != nil:
		s, ok := old.(string)
		if !ok {
			return nil, fmt.Errorf("a patch of %T", old)
		}
		mi := mirror{last: tokenize(s)}
		b, _ := json.Marshal(obj["p"])
		var steps []any
		_ = json.Unmarshal(b, &steps)
		html, ok := mi.applyLoose(jsonSteps(steps))
		if !ok {
			return nil, fmt.Errorf("a patch that does not apply")
		}
		return html, nil
	}
	return m.dyn(change)
}

func (m *treeMirror) write(b *strings.Builder, f *mframe) error {
	s, ok := m.statics[f.t]
	if !ok || len(s) != len(f.d)+1 {
		return fmt.Errorf("statics %d: have %d for %d dynamics", f.t, len(s), len(f.d))
	}
	for i, st := range s {
		b.WriteString(st)
		if i < len(f.d) {
			switch d := f.d[i].(type) {
			case string:
				b.WriteString(d)
			case *mframe:
				if err := m.write(b, d); err != nil {
					return err
				}
			case *mcomp:
				for _, it := range d.items {
					if err := m.write(b, it); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// applyLoose applies a patch with no token count to check.
func (m *mirror) applyLoose(patch []any) (string, bool) {
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
			if len(s) != 2 || s[0] < 0 || s[0]+s[1] > len(m.last) {
				return "", false
			}
			out = append(out, m.last[s[0]:s[0]+s[1]]...)
		}
	}
	return strings.Join(out, ""), true
}
