package nexus

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/paulmanoni/nexus/v2/dev"
	"github.com/paulmanoni/nexus/v2/di"
	"github.com/paulmanoni/nexus/v2/internal/maskhook"
)

// Route is a REST registration — what AsRest (and inertia.Page, view.Live's
// page) returns. It is an Option, so it goes in a Module or Boot like any
// other, and a handle: once the app is running, URL builds the path it is
// served at, with the module's Path, the routers it sits in and the app's
// route_prefix applied.
//
//	var ShowUser = nexus.AsRest("GET", "/users/:id", (*Users).Show, nexus.Arg("id"))
//
//	ShowUser.URL(ctx, 7)        // "/api/users/7"
//
// Every route is also reachable by name (see Name and URL).
type Route struct {
	o      di.Option
	cfg    *restConfig
	method string
	path   string
	fn     any
	// also are the other registrations of the same route — one per method
	// of a multi-method page (SameRoute).
	also []*Route
	err  error
}

func (r *Route) nexusOption() di.Option {
	if len(r.also) == 0 {
		return r.o
	}
	opts := make([]di.Option, 0, len(r.also)+1)
	opts = append(opts, r.o)
	for _, a := range r.also {
		opts = append(opts, a.nexusOption())
	}
	return di.Options(opts...)
}

func (r *Route) each(fn func(*Route)) {
	fn(r)
	for _, a := range r.also {
		a.each(fn)
	}
}

func (r *Route) setModule(name string) { r.each(func(x *Route) { x.cfg.module = name }) }
func (r *Route) setRestPrefix(p string) {
	r.each(func(x *Route) { x.cfg.pathPrefix = p + x.cfg.pathPrefix })
}
func (r *Route) setNamespace(ns string) {
	r.each(func(x *Route) {
		if !x.cfg.nsSet {
			x.cfg.namespace, x.cfg.nsSet = ns, true
		}
	})
}

// Method is the route's HTTP method — for a form's method attribute.
func (r *Route) Method() string { return r.method }

// URL is the path the route is served at in the app serving ctx, with
// params filled in (see Reverse). A route that can't be built panics under
// nexus dev and in tests, and logs and returns "#" otherwise.
func (r *Route) URL(ctx context.Context, params ...any) string {
	s, err := r.Reverse(ctx, params...)
	return mustURL(ctx, s, err)
}

// Reverse is URL with the error returned. params fill the route's path
// parameters and query:
//
//   - a scalar (string, number, bool, a fmt.Stringer, a pointer to one)
//     fills the next path parameter, in the order they appear in the path;
//   - P fills path parameters by name;
//   - a struct (or pointer to one) fills them from its path:"x" fields and
//     adds its non-zero query:"x" fields to the query — the handler's own
//     args type works;
//   - Query adds query parameters.
//
// Id parameters are masked when extension/maskid is on, as responses are.
func (r *Route) Reverse(ctx context.Context, params ...any) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	app, err := appForURL(ctx)
	if err != nil {
		return "", err
	}
	e, ok := app.routes.byHandle(r)
	if !ok {
		return "", fmt.Errorf("nexus: %s %s is not registered in this app", r.method, r.path)
	}
	return e.build(params)
}

// FailRoute is a Route that fails the boot with err — for a constructor that
// returns a *Route and finds its arguments invalid.
func FailRoute(err error) *Route {
	return &Route{o: di.Error(err), cfg: &restConfig{}, err: err}
}

// SameRoute joins registrations of one route under several methods (a
// page served on GET and POST) into one handle; URL builds the first.
func SameRoute(first *Route, more ...*Route) *Route {
	first.also = append(first.also, more...)
	return first
}

// Name names a REST route, overriding the default — the handler's name
// with its first letter lowered ((*Users).Show → "show"). Names live in the
// namespace of the module the route is declared in, or of the router chain
// that includes it ("admin:users:show"); Name("") names the route after the
// namespace itself. Two routes given the same name explicitly fail the boot.
func Name(name string) RestOption {
	return restOptionFn(func(c *restConfig) { c.name, c.nameSet = name, true })
}

// DefaultName replaces the name a route takes from its handler — for
// extensions that register routes on an app's behalf (view.Page names a
// page after its component, auth its endpoints "login", "me", …). Unlike
// Name it is a default: Name overrides it, and a clash with another route
// leaves the name ambiguous rather than failing the boot. DefaultName("")
// names the route after its namespace.
func DefaultName(name string) RestOption {
	return restOptionFn(func(c *restConfig) { c.fallback, c.fbSet = name, true })
}

// NoName keeps a route out of the names — for plumbing an extension
// registers (an upload endpoint, a socket) that nothing links to. Its
// handle still builds its URL.
func NoName() RestOption {
	return restOptionFn(func(c *restConfig) { c.noName = true })
}

// P fills path parameters by name in URL and Reverse.
type P map[string]any

// Query adds query parameters in URL and Reverse; a slice value repeats the
// parameter.
type Query map[string]any

// URL builds the URL of the route named name in the app serving ctx (see
// Route.Reverse for params). Like Route.URL it panics under nexus dev and in
// tests when the route can't be built, and logs and returns "#" otherwise.
func URL(ctx context.Context, name string, params ...any) string {
	s, err := Reverse(ctx, name, params...)
	return mustURL(ctx, s, err)
}

// Reverse is URL with the error returned.
func Reverse(ctx context.Context, name string, params ...any) (string, error) {
	app, err := appForURL(ctx)
	if err != nil {
		return "", err
	}
	e, err := app.routes.byName(name)
	if err != nil {
		return "", err
	}
	return e.build(params)
}

// URL builds the URL of the route named name in r's namespace.
func (r *Router) URL(ctx context.Context, name string, params ...any) string {
	s, err := r.Reverse(ctx, name, params...)
	return mustURL(ctx, s, err)
}

// Reverse is Router.URL with the error returned.
func (r *Router) Reverse(ctx context.Context, name string, params ...any) (string, error) {
	return Reverse(ctx, joinName(r.namespace(), name), params...)
}

// URL builds the URL of one of the controller's actions — a method
// expression, (*UsersController).Show.
func (c *ControllerRouter[T]) URL(ctx context.Context, action any, params ...any) string {
	s, err := c.Reverse(ctx, action, params...)
	return mustURL(ctx, s, err)
}

// Reverse is ControllerRouter.URL with the error returned.
func (c *ControllerRouter[T]) Reverse(ctx context.Context, action any, params ...any) (string, error) {
	name := runtimeFuncName(reflect.ValueOf(action))
	if name == "" {
		return "", fmt.Errorf("nexus: Controller[%s].URL: pass a method expression like (%s).Show", c.ctrl, c.ctrl)
	}
	return c.Router.Reverse(ctx, lowerFirst(name), params...)
}

func joinName(ns, name string) string {
	switch {
	case ns == "":
		return name
	case name == "":
		return ns
	}
	return ns + ":" + name
}

// ---- the app's route table ------------------------------------------------

type appCtxKey struct{}

func init() {
	RegisterWSCarrier(func(upgrade, conn context.Context) context.Context {
		if a, ok := upgrade.Value(appCtxKey{}).(*App); ok {
			conn = context.WithValue(conn, appCtxKey{}, a)
		}
		return conn
	})
}

// WithApp is ctx carrying app, so URL and Reverse build its routes — for code
// outside a request (a job, a test) in a process running several apps.
// A request's context carries its app already.
func WithApp(ctx context.Context, app *App) context.Context {
	return context.WithValue(ctx, appCtxKey{}, app)
}

var running = struct {
	sync.Mutex
	apps []*App
}{}

func trackRunning(a *App, on bool) {
	running.Lock()
	defer running.Unlock()
	for i, x := range running.apps {
		if x == a {
			running.apps = append(running.apps[:i], running.apps[i+1:]...)
			break
		}
	}
	if on {
		running.apps = append(running.apps, a)
	}
}

// appForURL is the app serving ctx, else the one app running in the
// process — what a job or a startup task building a link reaches.
func appForURL(ctx context.Context) (*App, error) {
	if ctx != nil {
		if a, ok := ctx.Value(appCtxKey{}).(*App); ok {
			return a, nil
		}
	}
	running.Lock()
	defer running.Unlock()
	switch len(running.apps) {
	case 1:
		return running.apps[0], nil
	case 0:
		return nil, errors.New("nexus: no app to build a URL for — pass a request's context, or build it once the app is running")
	}
	return nil, errors.New("nexus: several apps are running — pass a request's context to say which one builds the URL")
}

func mustURL(ctx context.Context, s string, err error) string {
	if err == nil {
		return s
	}
	if os.Getenv(dev.Env) == "1" || testing.Testing() {
		panic(err)
	}
	if a, aerr := appForURL(ctx); aerr == nil {
		a.Logger().ErrorContext(ctx, "nexus: building a URL failed", "error", err)
	}
	return "#"
}

// routeEntry is one mounted route.
type routeEntry struct {
	method   string
	path     string // the full template: route_prefix, prefixes, ":id", "*rest"
	name     string // its full name; "" when it has none
	explicit bool
}

type namedRoute struct {
	entries  []*routeEntry
	explicit bool
	// clash lists the paths of default-named routes that share a name
	// without sharing a path: the name builds neither.
	clash []string
}

type routeTable struct {
	mu      sync.RWMutex
	names   map[string]*namedRoute
	handles map[*Route]*routeEntry
}

func (t *routeTable) byHandle(r *Route) (*routeEntry, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.handles[r]
	return e, ok
}

func (t *routeTable) byName(name string) (*routeEntry, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n, ok := t.names[name]
	if !ok {
		msg := fmt.Sprintf("nexus: no route named %q", name)
		if s := t.closest(name); s != "" {
			msg += fmt.Sprintf(" — did you mean %q?", s)
		}
		return nil, errors.New(msg)
	}
	if len(n.clash) > 0 {
		return nil, fmt.Errorf("nexus: route name %q is ambiguous (%s) — name one of them with nexus.Name", name, strings.Join(n.clash, ", "))
	}
	return n.primary(), nil
}

// primary is the entry a name builds: the one without a trailing slash
// when a route is also served with one.
func (n *namedRoute) primary() *routeEntry {
	best := n.entries[0]
	for _, e := range n.entries[1:] {
		if len(e.path) < len(best.path) {
			best = e
		}
	}
	return best
}

func (t *routeTable) closest(name string) string {
	best, bestD := "", 4
	for k := range t.names {
		if d := editDistance(name, k); d < bestD || (d == bestD && k < best) {
			best, bestD = k, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// add records a mounted route. It returns the names whose owner changed
// (for the registry) and an error when two routes were given one name.
func (t *routeTable) add(h *Route, e *routeEntry) (changed map[string]string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handles == nil {
		t.handles = map[*Route]*routeEntry{}
		t.names = map[string]*namedRoute{}
	}
	if _, seen := t.handles[h]; !seen {
		t.handles[h] = e
	}
	if e.name == "" {
		return nil, nil
	}
	changed = map[string]string{}
	n, ok := t.names[e.name]
	switch {
	case !ok:
		t.names[e.name] = &namedRoute{entries: []*routeEntry{e}, explicit: e.explicit}
	case samePath(n.entries[0].path, e.path):
		if e.explicit && !n.explicit {
			n.explicit = true
		}
		n.entries = append(n.entries, e)
	case e.explicit && n.explicit:
		return nil, fmt.Errorf("nexus: two routes are named %q: %s %s and %s %s — rename one with nexus.Name",
			e.name, n.entries[0].method, n.entries[0].path, e.method, e.path)
	case e.explicit:
		for _, old := range n.entries {
			changed[old.method+" "+old.path] = ""
			old.name = ""
		}
		t.names[e.name] = &namedRoute{entries: []*routeEntry{e}, explicit: true}
	case n.explicit:
		e.name = ""
		return nil, nil
	default:
		if len(n.clash) == 0 {
			n.clash = append(n.clash, n.entries[0].path)
			for _, old := range n.entries {
				changed[old.method+" "+old.path] = ""
				old.name = ""
			}
		}
		n.clash = append(n.clash, e.path)
		e.name = ""
		return changed, nil
	}
	changed[e.method+" "+e.path] = e.name
	return changed, nil
}

func samePath(a, b string) bool {
	trim := func(s string) string {
		if len(s) > 1 {
			return strings.TrimSuffix(s, "/")
		}
		return s
	}
	return trim(a) == trim(b)
}

// routeName is the full name a route registers under, and whether it was
// given explicitly; "" when it has none (a closure without nexus.Name).
func (c *restConfig) routeName(fn any) (string, bool, error) {
	if c.noName {
		return "", false, nil
	}
	ns := c.namespace
	if !c.nsSet {
		ns = c.module
	}
	if c.nameSet {
		if c.name == "" && ns == "" {
			return "", false, errors.New("nexus.Name(\"\") names a route after its namespace, but it is declared outside any module or router")
		}
		return joinName(ns, c.name), true, nil
	}
	if c.fbSet {
		if c.fallback == "" && ns == "" {
			return "", false, nil
		}
		return joinName(ns, c.fallback), false, nil
	}
	var short string
	if c.action != nil {
		short = lowerFirst(c.action.name)
	} else {
		short = opNameFromFunc(fn, "")
	}
	if short == "" {
		return "", false, nil
	}
	return joinName(ns, short), false, nil
}

// registerRoute adds a mounted route to the app's table and the names to
// the registry.
func registerRoute(app *App, h *Route, method, finalPath string, fn any) error {
	name, explicit, err := h.cfg.routeName(fn)
	if err != nil {
		return fmt.Errorf("nexus: %s %s: %w", method, finalPath, err)
	}
	changed, err := app.routes.add(h, &routeEntry{method: method, path: finalPath, name: name, explicit: explicit})
	if err != nil {
		return err
	}
	for key, n := range changed {
		m, p, _ := strings.Cut(key, " ")
		app.registry.SetEndpointRoute(m, p, n)
	}
	return nil
}

// ---- building a path ------------------------------------------------------

func (e *routeEntry) build(params []any) (string, error) {
	names := routePathParams(e.path)
	vals := make(map[string]string, len(names))
	query := url.Values{}
	next := 0
	set := func(name string, v any) error {
		if !slicesContains(names, name) {
			return fmt.Errorf("nexus: %s has no path parameter %q", e.path, name)
		}
		s, ok := paramString(name, v)
		if !ok {
			return fmt.Errorf("nexus: %s: parameter %q is nil", e.path, name)
		}
		vals[name] = s
		return nil
	}
	for _, p := range params {
		switch v := p.(type) {
		case P:
			for k, x := range v {
				if err := set(k, x); err != nil {
					return "", err
				}
			}
		case Query:
			for k, x := range v {
				addQuery(query, k, x)
			}
		case url.Values:
			for k, xs := range v {
				for _, x := range xs {
					query.Add(k, x)
				}
			}
		default:
			rv := reflect.ValueOf(p)
			for rv.Kind() == reflect.Pointer && !rv.IsNil() && rv.Elem().Kind() == reflect.Struct {
				rv = rv.Elem()
			}
			if rv.Kind() == reflect.Struct && !isURLScalar(rv.Type()) {
				if err := fromStruct(rv, set, query); err != nil {
					return "", err
				}
				continue
			}
			for next < len(names) {
				if _, filled := vals[names[next]]; !filled {
					break
				}
				next++
			}
			if next >= len(names) {
				return "", fmt.Errorf("nexus: %s takes %d path parameter(s); got another: %v", e.path, len(names), p)
			}
			if err := set(names[next], p); err != nil {
				return "", err
			}
			next++
		}
	}
	var b strings.Builder
	for i, seg := range strings.Split(e.path, "/") {
		if i > 0 {
			b.WriteByte('/')
		}
		if len(seg) > 1 && (seg[0] == ':' || seg[0] == '*') {
			v, ok := vals[seg[1:]]
			if !ok {
				return "", fmt.Errorf("nexus: %s: missing path parameter %q", e.path, seg[1:])
			}
			if seg[0] == '*' {
				parts := strings.Split(strings.TrimPrefix(v, "/"), "/")
				for j, p := range parts {
					parts[j] = url.PathEscape(p)
				}
				b.WriteString(strings.Join(parts, "/"))
				continue
			}
			b.WriteString(url.PathEscape(v))
			continue
		}
		b.WriteString(seg)
	}
	out := b.String()
	if out == "" {
		out = "/"
	}
	if len(query) > 0 {
		out += "?" + query.Encode()
	}
	return out, nil
}

func fromStruct(rv reflect.Value, set func(string, any) error, query url.Values) error {
	t := rv.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		fv := rv.Field(i)
		if f.Anonymous && fv.Kind() == reflect.Struct {
			if err := fromStruct(fv, set, query); err != nil {
				return err
			}
			continue
		}
		if name := tagName(f.Tag.Get("path")); name != "" {
			if err := set(name, fv.Interface()); err != nil {
				return err
			}
			continue
		}
		if name := tagName(f.Tag.Get("query")); name != "" && !fv.IsZero() {
			addQuery(query, name, fv.Interface())
		}
	}
	return nil
}

func tagName(tag string) string {
	name, _, _ := strings.Cut(tag, ",")
	if name == "-" {
		return ""
	}
	return name
}

func addQuery(q url.Values, key string, v any) {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() != reflect.Uint8 {
		for i := range rv.Len() {
			if s, ok := paramString(key, rv.Index(i).Interface()); ok {
				q.Add(key, s)
			}
		}
		return
	}
	if s, ok := paramString(key, v); ok {
		q.Add(key, s)
	}
}

var stringerType = reflect.TypeFor[fmt.Stringer]()

func isURLScalar(t reflect.Type) bool { return t.Implements(stringerType) }

// paramString formats one parameter value, masking an id when maskid is on.
func paramString(key string, v any) (string, bool) {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return "", false
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return "", false
	}
	if s, ok := rv.Interface().(fmt.Stringer); ok {
		return s.String(), true
	}
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if s, ok := maskhook.MaskID(key, rv.Int()); ok {
			return s, true
		}
		return strconv.FormatInt(rv.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if n := rv.Uint(); n <= 1<<63-1 {
			if s, ok := maskhook.MaskID(key, int64(n)); ok {
				return s, true
			}
		}
		return strconv.FormatUint(rv.Uint(), 10), true
	case reflect.String:
		return rv.String(), true
	case reflect.Bool:
		return strconv.FormatBool(rv.Bool()), true
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(rv.Float(), 'f', -1, 64), true
	}
	return fmt.Sprint(rv.Interface()), true
}

func slicesContains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// RouteInfo is a named route: what nexus routes and App.Routes list.
type RouteInfo struct {
	Name   string `json:"name"`
	Method string `json:"method"`
	Path   string `json:"path"`
}

// Routes lists the app's named routes, sorted by name.
func (a *App) Routes() []RouteInfo {
	a.routes.mu.RLock()
	defer a.routes.mu.RUnlock()
	out := make([]RouteInfo, 0, len(a.routes.names))
	for k, n := range a.routes.names {
		if len(n.clash) > 0 {
			continue
		}
		e := n.primary()
		out = append(out, RouteInfo{Name: k, Method: e.method, Path: e.path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
