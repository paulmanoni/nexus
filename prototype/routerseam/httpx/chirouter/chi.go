// Package chirouter adapts go-chi to the httpx.Router seam. Same ~40 lines as
// the gin adapter; the only real work is translating canonical ":id"/"*rest"
// route syntax into chi's "{id}"/"*" so existing nexus route strings are
// reused verbatim.
package chirouter

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"routerseam/httpx"
)

type Router struct {
	mux    *chi.Mux
	routes []string
}

func New() *Router { return &Router{mux: chi.NewRouter()} }

func (r *Router) Handle(method, path string, chain ...httpx.HandlerFunc) {
	r.routes = append(r.routes, method+" "+path)
	r.mux.Method(method, toChi(path), http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		httpx.Serve(chain, w, req, path, func(k string) string {
			return chi.URLParam(req, k)
		})
	}))
}

func (r *Router) NoRoute(h httpx.HandlerFunc) {
	r.mux.NotFound(func(w http.ResponseWriter, req *http.Request) {
		httpx.Serve([]httpx.HandlerFunc{h}, w, req, "", nil)
	})
}

func (r *Router) Static(prefix, dir string) {
	fs := http.StripPrefix(prefix, http.FileServer(http.Dir(dir)))
	r.mux.Handle(strings.TrimSuffix(prefix, "/")+"/*", fs)
}

func (r *Router) Routes() []string { return r.routes }

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) { r.mux.ServeHTTP(w, req) }

// toChi rewrites ":id" -> "{id}" and "*rest" -> "*" (chi's catch-all).
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
