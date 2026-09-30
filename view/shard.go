package view

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"sync"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/httpx"
)

// maxShardBody bounds a shard request.
const maxShardBody = 1 << 20

// frame is a shard being rendered.
type frame struct {
	root   bool
	states []string
}

var (
	shardsMu sync.Mutex
	shards   = map[string]*shardDef{}
)

type shardDef struct {
	route string
	fn    reflect.Value
}

// Shard registers a component the server re-renders when a signal it reads
// on the server changes — one whose {{ }} code, for loops or component
// arguments read a signal:
//
//	templ PetResults(q *view.Signal[string]) {
//	    {{ pets := view.Use[*PetStore](ctx).Search(q.Get()) }}
//	    …
//	}
//
//	nexus.Run(cfg, view.Shard(PetResults, auth.Required()), …)
//
// Its endpoint is a nexus op: opts gate it exactly as they gate any route.
// The page's own gates do not run for it, and its arguments and signals
// arrive from the browser — validate them. The generator refuses a
// component that reads a signal on the server without a registration.
func Shard(component any, opts ...nexus.RestOption) nexus.Option {
	v := reflect.ValueOf(component)
	if !v.IsValid() || v.Kind() != reflect.Func || v.Type().NumOut() != 1 || v.Type().Out(0) != reflect.TypeFor[templ.Component]() {
		return nexus.Error(fmt.Errorf("view.Shard: want a templ component function, got %T", component))
	}
	d := &shardDef{route: shardRoute(component), fn: v}
	shardsMu.Lock()
	shards[d.route] = d
	shardsMu.Unlock()
	factory := func() httpx.HandlerFunc { return d.serve }
	opts = append([]nexus.RestOption{nexus.Describe("view shard " + d.route)}, opts...)
	return nexus.AsRestHandler("POST", "/_view/shard/"+d.route, factory, opts...)
}

// shardRoute names a component's endpoint: its package and name, plus a
// hash of the full import path so two packages' components never collide.
func shardRoute(component any) string {
	full := runtime.FuncForPC(reflect.ValueOf(component).Pointer()).Name()
	short := full[strings.LastIndexByte(full, '/')+1:]
	h := sha256.Sum256([]byte(full))
	return short + "-" + hex.EncodeToString(h[:3])
}

type shardRequest struct {
	Path   string                     `json:"path"`
	Args   []json.RawMessage          `json:"args"`
	States map[string]json.RawMessage `json:"states"`
}

func (d *shardDef) serve(c *httpx.Ctx) {
	var req shardRequest
	if err := json.NewDecoder(io.LimitReader(c.Request.Body, maxShardBody)).Decode(&req); err != nil {
		c.String(http.StatusBadRequest, "view: bad shard request: "+err.Error())
		return
	}
	t := d.fn.Type()
	if len(req.Args) != t.NumIn() {
		c.String(http.StatusBadRequest, fmt.Sprintf("view: shard %s takes %d arguments, got %d", d.route, t.NumIn(), len(req.Args)))
		return
	}
	in := make([]reflect.Value, t.NumIn())
	for i := range in {
		p := reflect.New(t.In(i))
		if err := json.Unmarshal(req.Args[i], p.Interface()); err != nil {
			c.String(http.StatusBadRequest, fmt.Sprintf("view: shard %s argument %d: %v", d.route, i+1, err))
			return
		}
		in[i] = p.Elem()
	}
	r := &render{restore: req.States, forcePath: req.Path, rootPending: true}
	ctx := withRender(c.Request.Context(), r)
	comp, _ := d.fn.Call(in)[0].Interface().(templ.Component)
	var buf bytes.Buffer
	err := errors.New("view: shard returned no component")
	if comp != nil {
		err = comp.Render(ctx, &buf)
	}
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, nexus.ErrForbidden) {
			status = http.StatusForbidden
		}
		c.String(status, err.Error())
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", buf.Bytes())
}

// ShardStart opens a shard component's output. Generated code calls it
// first in a component registered with Shard.
func ShardStart() templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		r := renderFrom(ctx)
		if r == nil {
			return errors.New("view: a shard rendered outside a view page — register the page with view.Page or view.HTML")
		}
		f := &frame{root: r.rootPending}
		r.rootPending = false
		r.frames = append(r.frames, f)
		if f.root {
			return nil
		}
		_, err := io.WriteString(w, `<nx-shard>`)
		return err
	})
}

type shardMeta struct {
	Shard  string   `json:"shard"`
	Path   string   `json:"path"`
	Args   []any    `json:"args"`
	Reads  []any    `json:"reads"`
	States []string `json:"states"`
}

// ShardEnd closes a shard component's output with what the browser needs to
// re-render it: the component, its place on the page, its arguments, the
// signals it reads on the server, and the signals it owns. Generated code
// calls it last.
func ShardEnd(component any, args []any, reads []any) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		r := renderFrom(ctx)
		if r == nil || len(r.frames) == 0 {
			return errors.New("view: ShardEnd without ShardStart")
		}
		f := r.frames[len(r.frames)-1]
		r.frames = r.frames[:len(r.frames)-1]
		path := ""
		if sc := scopeFrom(ctx); sc != nil {
			path = sc.path
		}
		meta, err := json.Marshal(shardMeta{Shard: shardRoute(component), Path: path, Args: args, Reads: reads, States: f.states})
		if err != nil {
			return fmt.Errorf("view: shard arguments are not JSON-encodable: %w", err)
		}
		if _, err := fmt.Fprintf(w, `<template data-nx-shard-meta="%s"></template>`, templ.EscapeString(string(meta))); err != nil {
			return err
		}
		if f.root {
			return nil
		}
		_, err = io.WriteString(w, `</nx-shard>`)
		return err
	})
}
