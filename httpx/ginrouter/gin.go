// Package ginrouter is an opt-in gin-backed nexus router. It exists for
// backward compatibility and for apps that want gin's ecosystem; selecting it
// re-introduces gin and its transitive dependency tree into the build. The
// default router is stdrouter (zero deps).
//
// Note gin's own middleware chain (c.Next) is never used — Handle drops to
// httpx.Serve immediately, so gin contributes only path matching. That is why
// every nexus middleware runs identically here and on stdrouter/chirouter.
package ginrouter

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/httpx"
)

type Router struct {
	e      *gin.Engine
	global []httpx.HandlerFunc
	routes []httpx.RouteInfo
}

// New builds a gin-backed router. It keeps gin's release-mode default; callers
// that want gin's dev access log can set GIN_MODE=debug.
func New() *Router {
	gin.SetMode(gin.ReleaseMode)
	return &Router{e: gin.New()}
}

func (r *Router) Handle(method, path string, chain ...httpx.HandlerFunc) {
	r.routes = append(r.routes, httpx.RouteInfo{Method: method, Path: path})
	full := append([]httpx.HandlerFunc{}, chain...)
	r.e.Handle(method, path, func(gc *gin.Context) {
		httpx.Serve(full, gc.Writer, gc.Request, gc.FullPath(), gc.Param)
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
	r.routes = append(r.routes, httpx.RouteInfo{Method: "ANY", Path: path})
	full := append([]httpx.HandlerFunc{}, chain...)
	r.e.Any(path, func(gc *gin.Context) {
		httpx.Serve(full, gc.Writer, gc.Request, gc.FullPath(), gc.Param)
	})
}

func (r *Router) Use(mw ...httpx.HandlerFunc) { r.global = append(r.global, mw...) }

func (r *Router) Group(prefix string, mw ...httpx.HandlerFunc) httpx.Group {
	return httpx.NewGroup(r, prefix, mw...)
}

func (r *Router) NoRoute(chain ...httpx.HandlerFunc) {
	full := append([]httpx.HandlerFunc{}, chain...)
	r.e.NoRoute(func(gc *gin.Context) {
		httpx.Serve(full, gc.Writer, gc.Request, "", gc.Param)
	})
}

func (r *Router) Static(prefix, dir string) { r.e.Static(prefix, dir) }

func (r *Router) Routes() []httpx.RouteInfo {
	out := make([]httpx.RouteInfo, 0, len(r.routes))
	out = append(out, r.routes...)
	return out
}

func (r *Router) Run(addr string) error { return http.ListenAndServe(addr, r) }

// ServeHTTP runs app-wide middleware (Use) around the gin engine so it fires
// for EVERY request (gin's engine-level semantics); route chains run inside the
// matched gin handler.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if len(r.global) == 0 {
		r.e.ServeHTTP(w, req)
		return
	}
	chain := append(append([]httpx.HandlerFunc{}, r.global...), func(c *httpx.Ctx) {
		r.e.ServeHTTP(c.Writer, c.Request)
	})
	httpx.Serve(chain, w, req, req.URL.Path, nil)
}
