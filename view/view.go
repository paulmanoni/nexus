// Package view renders templ components from nexus handlers and makes them
// reactive without a JavaScript build. State lives in components; markup
// that reads a signal stays current in the browser; on* attributes take
// actions; a component that reads a signal on the server re-renders there.
//
//	templ Counter() {
//	    {{ count := view.State(ctx, 0) }}
//	    <button onclick={ count.Set(count.Get() + 1) }>+1</button>
//	    <p>Clicked { count.Get() } times</p>
//	}
//
//	nexus.Run(cfg, view.Page("GET", "/", Counter))
//
// nexus dev and nexus build compile the templates — the reactive parts to
// JavaScript — with no configuration; the files stay plain templ for
// editors. The generator is package viewgen.
package view

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/registry"
)

func init() { nexus.RegisterDeferredOptions(options) }

func options() []nexus.Option {
	return []nexus.Option{
		nexus.AsRest("GET", "/_view/runtime.js", serveJS(func() string { return runtimeJS }), nexus.HideFromDashboard()),
		nexus.AsRest("GET", "/_view/import.js", serveJS(func() string { return importJS }), nexus.HideFromDashboard()),
		nexus.AsRest("GET", "/_view/twins.js", serveJS(twinsJS), nexus.HideFromDashboard()),
		nexus.Invoke(func(app *nexus.App, lc nexus.Lifecycle) {
			lastApp.Store(app)
			registerIslands(app)
			lc.Append(nexus.Hook{OnStop: func(context.Context) error { closeLive(app); return nil }})
		}),
	}
}

// Page serves a component at method + path:
//
//	view.Page("GET", "/", Home, auth.Required())
func Page(method, path string, component func() templ.Component, opts ...nexus.RestOption) nexus.Option {
	handler := func(ctx context.Context) (templ.Component, error) { return component(), nil }
	return nexus.AsRest(method, path, handler, append([]nexus.RestOption{HTML()}, opts...)...)
}

// HTML renders a handler's templ.Component result as the page — for a
// handler that builds its component from request data:
//
//	nexus.AsRest("GET", "/pets/:id", NewPetPage, view.HTML())
func HTML() nexus.RestOption {
	return nexus.RestOptions(nexus.WithRenderer(renderer{}), nexus.Tag(registry.ViewTag, "page"))
}

type renderer struct{}

func (renderer) Render(c *httpx.Ctx, result any) error {
	comp, ok := result.(templ.Component)
	if !ok || comp == nil {
		return fmt.Errorf("view: the handler returned %T, want a templ.Component", result)
	}
	var buf bytes.Buffer
	if err := comp.Render(withRender(withApp(c.Request.Context(), c), &render{}), &buf); err != nil {
		return err
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", buf.Bytes())
	return nil
}

var (
	exposedMu sync.RWMutex
	exposed   = map[reflect.Type]any{}
)

// Expose makes a DI value available to templates through Use:
//
//	nexus.Run(cfg, nexus.Provide(NewPetStore), view.Expose[*PetStore](), …)
func Expose[T any]() nexus.Option {
	return nexus.Invoke(func(v T) {
		exposedMu.Lock()
		exposed[reflect.TypeFor[T]()] = v
		exposedMu.Unlock()
	})
}

// Use returns a value from the app's DI container, for a template's server
// code. A service comes back as is:
//
//	{{ pets := view.Use[*PetStore](ctx).Search(q.Get()) }}
//
// A state struct — one with *view.Signal fields — is per page: Use returns
// a copy for this page whose signals every component shares, starting from
// the DI instance's values (or, in a shard re-render, the browser's):
//
//	type Search struct{ Query *view.Signal[string] }
//	func NewSearch() *Search { return &Search{Query: view.Initial("")} }
//
//	{{ q := view.Use[*Search](ctx).Query }}
//
// The generator exposes every type a template Uses.
func Use[T any](ctx context.Context) T {
	t := reflect.TypeFor[T]()
	exposedMu.RLock()
	v, ok := exposed[t]
	exposedMu.RUnlock()
	if !ok {
		panic(fmt.Sprintf("view.Use[%s]: not exposed — add view.Expose[%s]() to the app", t, t))
	}
	if !isState(t) {
		return v.(T)
	}
	r := renderFrom(ctx)
	if r == nil {
		return v.(T)
	}
	key := "type:" + t.String()
	if cached, ok := r.shared[key].(T); ok {
		for _, id := range stateIDs(t) {
			r.own(id)
		}
		return cached
	}
	page := clonePerPage(v, t, r).(T)
	if r.shared == nil {
		r.shared = map[string]any{}
	}
	r.shared[key] = page
	return page
}

// Initial is a signal field's default in a state struct's constructor.
func Initial[T any](v T) *Signal[T] { return &Signal[T]{v: v} }

// perPage is implemented by *Signal[T]: a copy for one page, under id,
// holding the browser's value when the render restores one.
type perPage interface {
	perPage(id string, r *render) any
}

func (s *Signal[T]) perPage(id string, r *render) any {
	c := &Signal[T]{id: id}
	if s != nil {
		c.v = s.v
	}
	if raw, ok := r.restore[id]; ok {
		var v T
		if json.Unmarshal(raw, &v) == nil {
			c.v = v
		}
	}
	r.own(id)
	return c
}

var perPageType = reflect.TypeFor[perPage]()

// isState reports whether t is a pointer to a struct with signal fields.
func isState(t reflect.Type) bool { return len(stateIDs(t)) > 0 }

// stateIDs are the page ids of t's signal fields: type and field name.
func stateIDs(t reflect.Type) []string {
	if t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
		return nil
	}
	var ids []string
	st := t.Elem()
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if f.IsExported() && f.Type.Implements(perPageType) {
			h := sha256.Sum256([]byte(t.String() + "." + f.Name))
			ids = append(ids, "g"+hex.EncodeToString(h[:5]))
		}
	}
	return ids
}

func clonePerPage(v any, t reflect.Type, r *render) any {
	src := reflect.ValueOf(v).Elem()
	dst := reflect.New(t.Elem())
	dst.Elem().Set(src)
	ids := stateIDs(t)
	n := 0
	for i := 0; i < t.Elem().NumField(); i++ {
		f := t.Elem().Field(i)
		if !f.IsExported() || !f.Type.Implements(perPageType) {
			continue
		}
		field := dst.Elem().Field(i)
		sig, _ := field.Interface().(perPage)
		if field.IsNil() {
			sig = reflect.New(f.Type.Elem()).Interface().(perPage)
		}
		field.Set(reflect.ValueOf(sig.perPage(ids[n], r)))
		n++
	}
	return dst.Interface()
}

// Assets serves handler under prefix — how a component library's files
// (templUI's /templui/js/, say) or a built stylesheet reach the browser:
//
//	mux := http.NewServeMux()
//	utils.SetupScriptRoutes(mux, dev)            // templUI's script routes
//	view.Assets("/templui/js/", mux)
//	view.Assets("/assets/", http.FileServerFS(assetsFS))
//
// The handler sees the request's full path.
func Assets(prefix string, handler http.Handler) nexus.Option {
	route := strings.TrimSuffix(prefix, "/") + "/*path"
	serve := func(c *httpx.Ctx) { handler.ServeHTTP(c.Writer, c.Request) }
	return nexus.AsRest("GET", route, serve, nexus.HideFromDashboard())
}
