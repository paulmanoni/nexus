// Package chirouter is an opt-in chi-backed nexus router. Select it instead of
// the default stdlib router when you want chi's matching/features. It adds a
// single dependency (go-chi/chi) and no transitive deps.
package chirouter

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/paulmanoni/nexus/httpx"
)

type Router struct {
	mux    *chi.Mux
	global []httpx.HandlerFunc
	routes []httpx.RouteInfo
}

// New builds a chi-backed router.
func New() *Router { return &Router{mux: chi.NewRouter()} }

func (r *Router) Handle(method, path string, chain ...httpx.HandlerFunc) {
	r.routes = append(r.routes, httpx.RouteInfo{Method: method, Path: path})
	full := append([]httpx.HandlerFunc{}, chain...)
	wild := httpx.WildcardName(path)
	r.mux.Method(method, toChi(path), http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		httpx.Serve(full, w, req, path, paramFn(req, wild))
	}))
}

func (r *Router) GET(path string, chain ...httpx.HandlerFunc)    { r.Handle("GET", path, chain...) }
func (r *Router) POST(path string, chain ...httpx.HandlerFunc)   { r.Handle("POST", path, chain...) }
func (r *Router) PUT(path string, chain ...httpx.HandlerFunc)    { r.Handle("PUT", path, chain...) }
func (r *Router) DELETE(path string, chain ...httpx.HandlerFunc) { r.Handle("DELETE", path, chain...) }
func (r *Router) PATCH(path string, chain ...httpx.HandlerFunc)  { r.Handle("PATCH", path, chain...) }
func (r *Router) OPTIONS(path string, chain ...httpx.HandlerFunc) {
	r.Handle("OPTIONS", path, chain...)
}
func (r *Router) HEAD(path string, chain ...httpx.HandlerFunc) { r.Handle("HEAD", path, chain...) }

func (r *Router) Any(path string, chain ...httpx.HandlerFunc) {
	r.routes = append(r.routes, httpx.RouteInfo{Method: "ANY", Path: path})
	full := append([]httpx.HandlerFunc{}, chain...)
	wild := httpx.WildcardName(path)
	r.mux.HandleFunc(toChi(path), func(w http.ResponseWriter, req *http.Request) {
		httpx.Serve(full, w, req, path, paramFn(req, wild))
	})
}

// paramFn returns a path-param lookup that matches gin's convention. chi rewrites
// "*rest" to its "*" catch-all and stores the suffix under the key "*" without a
// leading slash; gin exposes it under the original name WITH a leading slash
// ("/app.js"). Map the route's wildcard name onto chi's "*" key and re-add the
// slash so c.Param("rest") behaves identically on every backend.
func paramFn(req *http.Request, wild string) func(string) string {
	return func(k string) string {
		if wild != "" && k == wild {
			return "/" + chi.URLParam(req, "*")
		}
		return chi.URLParam(req, k)
	}
}

func (r *Router) Use(mw ...httpx.HandlerFunc) { r.global = append(r.global, mw...) }

func (r *Router) Group(prefix string, mw ...httpx.HandlerFunc) httpx.Group {
	return httpx.NewGroup(r, prefix, mw...)
}

func (r *Router) NoRoute(chain ...httpx.HandlerFunc) {
	full := append([]httpx.HandlerFunc{}, chain...)
	r.mux.NotFound(func(w http.ResponseWriter, req *http.Request) {
		httpx.Serve(full, w, req, "", nil)
	})
}

func (r *Router) Static(prefix, dir string) {
	p := strings.TrimSuffix(prefix, "/") + "/"
	r.mux.Handle(p+"*", http.StripPrefix(p, http.FileServer(http.Dir(dir))))
}

func (r *Router) Routes() []httpx.RouteInfo { return r.routes }

func (r *Router) Run(addr string) error { return http.ListenAndServe(addr, r) }

// ServeHTTP runs app-wide middleware (Use) around the mux so it fires for EVERY
// request (gin's engine-level semantics); route chains run inside the match.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if len(r.global) == 0 {
		r.mux.ServeHTTP(w, req)
		return
	}
	chain := append(append([]httpx.HandlerFunc{}, r.global...), func(c *httpx.Ctx) {
		r.mux.ServeHTTP(c.Writer, c.Request)
	})
	httpx.Serve(chain, w, req, req.URL.Path, nil)
}

// toChi rewrites ":id" -> "{id}" and "*rest" -> "*".
func toChi(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		switch {
		case strings.HasPrefix(p, ":"):
			parts[i] = "{" + p[1:] + "}"
		case strings.HasPrefix(p, "*"):
			parts[i] = "*"
		}
	}
	return strings.Join(parts, "/")
}
