package view

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
)

// render is the state of one page or shard render.
type render struct {
	restore     map[string]json.RawMessage // browser values of a shard's own signals
	forcePath   string                     // the path a re-rendered shard had on the page
	rootPending bool                       // the next ShardStart is the shard being re-rendered
	frames      []*frame                   // open shards, innermost last
	shared      map[string]any             // the page's SharedState signals, by id
}

// own records that the innermost open shard uses signal id, so a re-render
// of that shard sends its browser value back.
func (r *render) own(id string) {
	if n := len(r.frames); n > 0 {
		f := r.frames[n-1]
		for _, s := range f.states {
			if s == id {
				return
			}
		}
		f.states = append(f.states, id)
	}
}

// scope is one component instance on the page. Its path — the chain of
// component names with sibling indexes — gives its signals stable ids, so a
// shard re-render finds the ids the page already holds.
type scope struct {
	path   string
	kids   map[string]int
	states int
	log    []string // the components entered in it, and "" for each signal made, in order: a skipped spot replays its share (rendered.go)
}

// enter counts a component entered in this scope and returns its index
// among those of its name.
func (sc *scope) enter(name string) int {
	if sc.kids == nil {
		sc.kids = map[string]int{}
	}
	i := sc.kids[name]
	sc.kids[name] = i + 1
	sc.log = append(sc.log, name)
	return i
}

type (
	renderKey struct{}
	scopeKey  struct{}
)

func withRender(ctx context.Context, r *render) context.Context {
	return context.WithValue(ctx, renderKey{}, r)
}

func renderFrom(ctx context.Context) *render {
	r, _ := ctx.Value(renderKey{}).(*render)
	return r
}

func scopeFrom(ctx context.Context) *scope {
	s, _ := ctx.Value(scopeKey{}).(*scope)
	return s
}

// Enter opens a component's scope. Generated code calls it at the top of
// every component in a file that imports this package.
func Enter(ctx context.Context, name string) context.Context {
	r := renderFrom(ctx)
	if r == nil {
		r = &render{}
		ctx = withRender(ctx, r)
	}
	var path string
	if r.forcePath != "" {
		path, r.forcePath = r.forcePath, ""
	} else {
		parent := scopeFrom(ctx)
		if parent == nil {
			parent = &scope{}
		}
		i := parent.enter(name)
		path = parent.path + "/" + name + "." + strconv.Itoa(i)
	}
	return context.WithValue(ctx, scopeKey{}, &scope{path: path})
}

// State creates a signal owned by the component rendering it:
//
//	{{ count := view.State(ctx, 0) }}
//
// Its id comes from the component's place on the page and the order of its
// State calls, as with React hooks — so call State unconditionally, at the
// top of the component. In a shard re-render it starts from the browser's
// current value, which is user input.
func State[T any](ctx context.Context, initial T) *Signal[T] {
	sc := scopeFrom(ctx)
	if sc == nil {
		sc = &scope{}
	}
	h := sha256.Sum256([]byte(sc.path + "#" + strconv.Itoa(sc.states)))
	sc.states++
	sc.log = append(sc.log, "")
	s := &Signal[T]{id: "s" + hex.EncodeToString(h[:5]), v: initial}
	if r := renderFrom(ctx); r != nil {
		if raw, ok := r.restore[s.id]; ok {
			var v T
			if json.Unmarshal(raw, &v) == nil {
				s.v = v
			}
		}
		r.own(s.id)
	}
	return s
}
