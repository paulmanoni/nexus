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
	wild := httpx.WildcardName(path)
	r.mux.HandleFunc(method+" "+toStd(path), func(w http.ResponseWriter, req *http.Request) {
		httpx.Serve(full, w, req, path, paramFn(req, wild))
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
	wild := httpx.WildcardName(path)
	r.mux.HandleFunc(toStd(path), func(w http.ResponseWriter, req *http.Request) {
		httpx.Serve(full, w, req, path, paramFn(req, wild))
	})
}

// paramFn returns a path-param lookup that matches gin's convention. ServeMux's
// "{rest...}" wildcard yields the matched suffix WITHOUT a leading slash
// ("app.js"); gin's "*rest" includes it ("/app.js"). For the wildcard param we
// re-add the slash so handlers that do "assets"+c.Param("rest") work the same on
// every backend. Non-wildcard params pass straight through.
func paramFn(req *http.Request, wild string) func(string) string {
	if wild == "" {
		return req.PathValue
	}
	return func(k string) string {
		if k == wild {
			return "/" + req.PathValue(k)
		}
		return req.PathValue(k)
	}
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
	// Scope to GET (ServeMux serves HEAD off a GET pattern too). A method-less
	// "/media/" matches every method, so it is NOT a subset of an app's catch-all
	// "GET /" — ServeMux flags that as an ambiguous overlap and panics. A static
	// server only needs GET/HEAD anyway, and "GET /media/" is a strict path
	// refinement of "GET /", so the conflict disappears.
	r.mux.Handle("GET "+p, http.StripPrefix(p, http.FileServer(http.Dir(dir))))
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

// toStd rewrites canonical ":id" -> "{id}" and "*rest" -> "{rest...}", and
// pins trailing-slash routes to an exact match.
//
// gin treats a registered route as an EXACT path match (its catch-all is the
// "*rest" wildcard). net/http.ServeMux instead treats any pattern ending in
// "/" as a SUBTREE match — so a literal "GET /" would shadow every unmatched
// GET path (e.g. /assets/app.js), stealing requests an app expects to reach a
// more specific route or the NoRoute fallback. Appending ServeMux's "{$}"
// end-of-path marker to a trailing-slash route ("/" -> "/{$}", "/admin/" ->
// "/admin/{$}") restores gin's exact-match semantics. Wildcard routes end in
// "}" after rewriting, so they keep their intended subtree behavior; NoRoute
// and Static register their patterns directly and never pass through here.
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
	std := strings.Join(parts, "/")
	if strings.HasSuffix(std, "/") {
		std += "{$}"
	}
	return std
}
