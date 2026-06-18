// Package ginrouter adapts gin to the httpx.Router seam. This is the whole
// gin dependency surface once the framework targets httpx: ~40 lines, here,
// and nowhere else. Note gin's own chain (c.Next) is never used — we drop to
// httpx.Serve immediately, so gin contributes only path matching.
package ginrouter

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"routerseam/httpx"
)

type Router struct{ e *gin.Engine }

func New() *Router {
	gin.SetMode(gin.ReleaseMode)
	return &Router{e: gin.New()}
}

func (r *Router) Handle(method, path string, chain ...httpx.HandlerFunc) {
	r.e.Handle(method, path, func(gc *gin.Context) {
		httpx.Serve(chain, gc.Writer, gc.Request, gc.FullPath(), gc.Param)
	})
}

func (r *Router) NoRoute(h httpx.HandlerFunc) {
	r.e.NoRoute(func(gc *gin.Context) {
		httpx.Serve([]httpx.HandlerFunc{h}, gc.Writer, gc.Request, "", gc.Param)
	})
}

func (r *Router) Static(prefix, dir string) { r.e.Static(prefix, dir) }

func (r *Router) Routes() []string {
	out := make([]string, 0)
	for _, ri := range r.e.Routes() {
		out = append(out, ri.Method+" "+ri.Path)
	}
	return out
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) { r.e.ServeHTTP(w, req) }
