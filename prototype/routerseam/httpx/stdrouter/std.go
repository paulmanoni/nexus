// Package stdrouter adapts Go 1.22's net/http.ServeMux to the httpx.Router
// seam — zero third-party deps. Proof the seam is not just gin<->chi: the
// framework could ship with no router dependency at all.
package stdrouter

import (
	"net/http"
	"strings"

	"routerseam/httpx"
)

type Router struct {
	mux      *http.ServeMux
	noRoute  httpx.HandlerFunc
	routes   []string
}

func New() *Router { return &Router{mux: http.NewServeMux()} }

func (r *Router) Handle(method, path string, chain ...httpx.HandlerFunc) {
	r.routes = append(r.routes, method+" "+path)
	std := toStd(path)
	r.mux.HandleFunc(method+" "+std, func(w http.ResponseWriter, req *http.Request) {
		httpx.Serve(chain, w, req, path, req.PathValue)
	})
}

func (r *Router) NoRoute(h httpx.HandlerFunc) {
	r.noRoute = h
	// "/" is the least-specific pattern: it catches everything no other
	// pattern claims — the stdlib equivalent of gin's NoRoute.
	r.mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		httpx.Serve([]httpx.HandlerFunc{h}, w, req, "", nil)
	})
}

func (r *Router) Static(prefix, dir string) {
	p := strings.TrimSuffix(prefix, "/") + "/"
	r.mux.Handle(p, http.StripPrefix(p, http.FileServer(http.Dir(dir))))
}

func (r *Router) Routes() []string { return r.routes }

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) { r.mux.ServeHTTP(w, req) }

// toStd rewrites ":id" -> "{id}" and "*rest" -> "{rest...}" (Go 1.22 syntax).
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
