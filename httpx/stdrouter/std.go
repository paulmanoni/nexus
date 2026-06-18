// Package stdrouter is the DEFAULT nexus router: Go 1.22's net/http.ServeMux
// behind the httpx.Router seam, with zero third-party dependencies. The default
// nexus binary therefore links no external router at all. gin and chi remain
// available as opt-in backends (httpx/ginrouter, httpx/chirouter).
package stdrouter

import (
	"net/http"
	"strings"

	"github.com/paulmanoni/nexus/httpx"
)

type Router struct {
	mux    *http.ServeMux
	global []httpx.HandlerFunc
	routes []httpx.RouteInfo
}

// New builds the default stdlib-backed router.
func New() *Router { return &Router{mux: http.NewServeMux()} }

func (r *Router) Handle(method, path string, chain ...httpx.HandlerFunc) {
	r.routes = append(r.routes, httpx.RouteInfo{Method: method, Path: path})
	full := append([]httpx.HandlerFunc{}, chain...)
	r.mux.HandleFunc(method+" "+toStd(path), func(w http.ResponseWriter, req *http.Request) {
		httpx.Serve(full, w, req, path, req.PathValue)
	})
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
	// Register method-less so a single pattern matches every method. Looping
	// over StdMethods would panic: Go 1.22's ServeMux treats GET as also
	// matching HEAD, so an explicit "GET /x" + "HEAD /x" pair conflicts.
	r.routes = append(r.routes, httpx.RouteInfo{Method: "ANY", Path: path})
	full := append([]httpx.HandlerFunc{}, chain...)
	r.mux.HandleFunc(toStd(path), func(w http.ResponseWriter, req *http.Request) {
		httpx.Serve(full, w, req, path, req.PathValue)
	})
}

func (r *Router) Use(mw ...httpx.HandlerFunc) { r.global = append(r.global, mw...) }

func (r *Router) Group(prefix string, mw ...httpx.HandlerFunc) httpx.Group {
	return httpx.NewGroup(r, prefix, mw...)
}

func (r *Router) NoRoute(chain ...httpx.HandlerFunc) {
	full := append([]httpx.HandlerFunc{}, chain...)
	// "/" is the least-specific pattern: ServeMux falls back to it for any
	// path no other pattern claims — the stdlib equivalent of gin's NoRoute.
	r.mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		httpx.Serve(full, w, req, "", nil)
	})
}

func (r *Router) Static(prefix, dir string) {
	p := strings.TrimSuffix(prefix, "/") + "/"
	r.mux.Handle(p, http.StripPrefix(p, http.FileServer(http.Dir(dir))))
}

func (r *Router) Routes() []httpx.RouteInfo { return r.routes }

func (r *Router) Run(addr string) error { return http.ListenAndServe(addr, r) }

// ServeHTTP runs app-wide middleware (Use) around the mux so it fires for EVERY
// request — including ones the mux would 404/405 (gin's engine-level middleware
// semantics). Route-specific chains run inside the matched handler afterward.
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

// toStd rewrites canonical ":id" -> "{id}" and "*rest" -> "{rest...}".
func toStd(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		switch {
		case strings.HasPrefix(p, ":"):
			parts[i] = "{" + p[1:] + "}"
		case strings.HasPrefix(p, "*"):
			parts[i] = "{" + p[1:] + "...}"
		}
	}
	return strings.Join(parts, "/")
}
