package httpx

// Group is implemented once here, in terms of Router.Handle, so no adapter has
// to reimplement sub-router mounting. A group carries a path prefix and its own
// middleware; routes mounted through it become Router.Handle(prefix+path,
// groupMW ++ chain). The router still prepends its app-wide Use middleware
// inside Handle, so the final order is: app Use → group Use → route chain —
// matching gin's engine-vs-group middleware ordering.

type group struct {
	r      Router
	prefix string
	mw     []HandlerFunc
}

// NewGroup builds a Group over any Router. Adapters return this from Group().
func NewGroup(r Router, prefix string, mw ...HandlerFunc) Group {
	return &group{r: r, prefix: prefix, mw: append([]HandlerFunc{}, mw...)}
}

func (g *group) Handle(method, path string, chain ...HandlerFunc) {
	full := make([]HandlerFunc, 0, len(g.mw)+len(chain))
	full = append(full, g.mw...)
	full = append(full, chain...)
	g.r.Handle(method, g.prefix+path, full...)
}

func (g *group) GET(path string, chain ...HandlerFunc)     { g.Handle("GET", path, chain...) }
func (g *group) POST(path string, chain ...HandlerFunc)    { g.Handle("POST", path, chain...) }
func (g *group) PUT(path string, chain ...HandlerFunc)     { g.Handle("PUT", path, chain...) }
func (g *group) DELETE(path string, chain ...HandlerFunc)  { g.Handle("DELETE", path, chain...) }
func (g *group) PATCH(path string, chain ...HandlerFunc)   { g.Handle("PATCH", path, chain...) }
func (g *group) OPTIONS(path string, chain ...HandlerFunc) { g.Handle("OPTIONS", path, chain...) }
func (g *group) HEAD(path string, chain ...HandlerFunc)    { g.Handle("HEAD", path, chain...) }

func (g *group) Any(path string, chain ...HandlerFunc) {
	for _, m := range StdMethods {
		g.Handle(m, path, chain...)
	}
}

func (g *group) Use(mw ...HandlerFunc) { g.mw = append(g.mw, mw...) }

func (g *group) Group(prefix string, mw ...HandlerFunc) Group {
	return &group{r: g.r, prefix: g.prefix + prefix, mw: append(append([]HandlerFunc{}, g.mw...), mw...)}
}

func (g *group) Static(prefix, dir string) { g.r.Static(g.prefix+prefix, dir) }
