package nexus

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/paulmanoni/nexus/v2/di"
	"github.com/paulmanoni/nexus/v2/middleware"
)

// Router is a FastAPI-style registration group: a first-class value that
// carries a URL prefix and shared per-op middleware, collects registrations,
// and nests. Each router becomes one dashboard module; including a router
// stacks prefixes and inherits the parent's shared options.
//
//	billing := nexus.NewRouter("billing", "/billing", auth.Required())
//	billing.Rest("GET", "/invoices", NewListInvoices)
//	billing.Query(NewInvoiceStats)
//
//	v1 := nexus.NewRouter("v1", "/api/v1")
//	v1.Include(billing) // billing mounts at /api/v1/billing
//
//	nexus.Boot(v1) // a *Router is an Option; pass only the root
//
// Shared options are MiddlewareOptions (auth.Required, auth.Requires,
// session.Required, nexus.Use(...)): they apply to every REST, GraphQL and
// WebSocket registration in the router and in every included router, ahead
// of the op's own options. Workers and Provide are grouped but not gated.
type Router struct {
	name     string
	prefix   string
	shared   []MiddlewareOption
	builders []func(prefix string, shared []MiddlewareOption) Option // prefix: the full, stacked route prefix
	attached []Option                                                // pre-built ops (decorator form); shared applies via annotator
	raw      []Option                                                // Provide/Register — grouped, never gated
	children []*Router
	parent   string
	// up is the router that includes this one.
	up       *Router
	errs     []error
	expanded bool

	// enclosing is the REST prefix of the nexus.Module the router sits in
	// (Module stamps it), so a router inside Module("x", Path("/x"), …)
	// mounts under /x. gqlHome is that module's public path: where a
	// REST-only router's GraphQL ops mount.
	enclosing string
	gqlHome   string
	// restOnly makes the prefix a REST-only RoutePrefix: GraphQL ops stay on
	// the enclosing endpoint instead of moving to <prefix>/graphql.
	restOnly bool

	// hooks run once, as the router expands — after every package init — so
	// a controller can take the actions generated code registered for its
	// type. claims are the controller types this router tree serves.
	hooks  []func()
	claims []reflect.Type
	// requireActions, when set, fails the boot with this message if the
	// router ends up with nothing registered.
	requireActions string

	mu         sync.Mutex
	cached     di.Option // the expansion, reused by later builds in the process
	resolvedIn uint64    // the build that last resolved this router
}

// NewRouter creates a router. name labels the dashboard module; prefix ("" or
// "/"-prefixed) prepends every route, stacking under Include.
func NewRouter(name, prefix string, shared ...MiddlewareOption) *Router {
	if prefix == "/" {
		prefix = "" // the enclosing path itself
	}
	r := &Router{name: name, prefix: prefix, shared: shared}
	if name == "" {
		r.errs = append(r.errs, fmt.Errorf("nexus: NewRouter needs a name"))
	}
	if prefix != "" && !strings.HasPrefix(prefix, "/") {
		r.errs = append(r.errs, fmt.Errorf("nexus: router %q prefix %q must start with \"/\"", name, prefix))
	}
	return r
}

// Rest registers a REST endpoint on the router.
func (r *Router) Rest(method, path string, fn any, opts ...RestOption) *Router {
	r.builders = append(r.builders, func(_ string, sh []MiddlewareOption) Option {
		all := make([]RestOption, 0, len(sh)+len(opts))
		for _, m := range sh {
			all = append(all, m)
		}
		return AsRest(method, path, fn, append(all, opts...)...)
	})
	return r
}

// Query registers a GraphQL query on the router.
func (r *Router) Query(fn any, opts ...GqlOption) *Router { return r.gql(AsQuery, fn, opts) }

// Mutation registers a GraphQL mutation on the router.
func (r *Router) Mutation(fn any, opts ...GqlOption) *Router { return r.gql(AsMutation, fn, opts) }

// Subscription registers a GraphQL subscription on the router.
func (r *Router) Subscription(fn any, opts ...GqlOption) *Router {
	return r.gql(AsSubscription, fn, opts)
}

func (r *Router) gql(as func(any, ...GqlOption) Option, fn any, opts []GqlOption) *Router {
	r.builders = append(r.builders, func(_ string, sh []MiddlewareOption) Option {
		all := make([]GqlOption, 0, len(sh)+len(opts))
		for _, m := range sh {
			all = append(all, m)
		}
		return as(fn, append(all, opts...)...)
	})
	return r
}

// WS registers a WebSocket message handler on the router.
func (r *Router) WS(path, msgType string, fn any, opts ...WSOption) *Router {
	r.builders = append(r.builders, func(_ string, sh []MiddlewareOption) Option {
		all := make([]WSOption, 0, len(sh)+len(opts))
		for _, m := range sh {
			all = append(all, m)
		}
		return AsWS(path, msgType, fn, append(all, opts...)...)
	})
	return r
}

// Worker registers a background worker under the router's module (workers
// serve no requests, so shared middleware does not apply).
func (r *Router) Worker(name string, fn any) *Router {
	r.raw = append(r.raw, AsWorker(name, fn))
	return r
}

// Provide adds constructors under the router's module.
func (r *Router) Provide(fns ...any) *Router {
	r.raw = append(r.raw, Provide(fns...))
	return r
}

// Register adds arbitrary options under the router's module, ungated.
func (r *Router) Register(opts ...Option) *Router {
	r.raw = append(r.raw, opts...)
	return r
}

// Include nests child under r: the child's routes mount under r's prefix and
// inherit r's shared options (child's own shared options apply after). Pass
// only the ROOT router to Boot/Run — an included router used directly errors.
func (r *Router) Include(child *Router) *Router {
	switch {
	case child == nil:
		r.errs = append(r.errs, fmt.Errorf("nexus: router %q includes a nil router", r.name))
	case child == r:
		r.errs = append(r.errs, fmt.Errorf("nexus: router %q cannot include itself", r.name))
	case child.parent != "":
		r.errs = append(r.errs, fmt.Errorf("nexus: router %q is already included in %q", child.name, child.parent))
	default:
		child.parent = r.name
		child.up = r
		r.children = append(r.children, child)
	}
	return r
}

// attach adds a pre-built op that receives the shared options at expansion
// (the decorator-form path).
func (r *Router) attach(op Option) { r.attached = append(r.attached, op) }

// nexusOption defers the expansion to boot (di.Defer): a router declared in a
// package-level variable is built before the package's generated init()
// records the actions annotated on its controller type.
func (r *Router) nexusOption() di.Option {
	if r.parent != "" {
		return FailBoot(fmt.Errorf("nexus: router %q is included in %q — pass only the root router", r.name, r.parent)).nexusOption()
	}
	return di.Defer(r.resolve)
}

// resolve expands the router once per process and hands the result to each
// build; within one build a second resolution is a double mount. It records
// the build's claims on the controller types the tree serves.
func (r *Router) resolve() di.Option {
	r.mu.Lock()
	defer r.mu.Unlock()
	gen := currentBuild()
	if r.resolvedIn == gen && r.cached != nil {
		return FailBoot(fmt.Errorf("nexus: router %q mounted twice — the same router passed twice", r.name)).nexusOption()
	}
	r.resolvedIn = gen
	if r.cached == nil {
		r.cached = r.expand(r.enclosing, nil).nexusOption()
	}
	r.claim(gen)
	return r.cached
}

func (r *Router) claim(gen uint64) {
	for _, t := range r.claims {
		claimType(t, gen)
	}
	for _, c := range r.children {
		c.claim(gen)
	}
}

// buildGen numbers builds, so claims and double-mount checks are per build
// even when a process boots several apps (tests).
var buildGen atomic.Uint64

func currentBuild() uint64 { return buildGen.Load() }

// beginBuild starts a new build; Run calls it before collecting options.
func beginBuild() { buildGen.Add(1) }

var claimed = struct {
	sync.Mutex
	m map[reflect.Type]uint64
}{m: map[reflect.Type]uint64{}}

func claimType(t reflect.Type, gen uint64) {
	claimed.Lock()
	claimed.m[t] = gen
	claimed.Unlock()
}

func claimedIn(t reflect.Type, gen uint64) bool {
	claimed.Lock()
	defer claimed.Unlock()
	g, ok := claimed.m[t]
	return ok && g == gen
}

// setEnclosing records the enclosing nexus.Module's REST prefix and public
// path; Module calls it on the routers among its children.
func (r *Router) setEnclosing(prefix, publicPath string) {
	r.enclosing = prefix + r.enclosing
	if r.gqlHome == "" {
		r.gqlHome = publicPath
	}
}

// enclosingAnnotator is implemented by routers (and controllers): options
// that expand into their own module and so need the enclosing module's
// prefix handed to them rather than stamped on their ops.
type enclosingAnnotator interface {
	setEnclosing(prefix, publicPath string)
}

func (r *Router) expand(parentPrefix string, inherited []MiddlewareOption) Option {
	if r.expanded {
		return FailBoot(fmt.Errorf("nexus: router %q mounted twice — a cycle, or the same router passed twice", r.name))
	}
	r.expanded = true
	for _, h := range r.hooks {
		h()
	}
	if len(r.errs) > 0 {
		return FailBoot(r.errs[0])
	}
	if r.requireActions != "" && len(r.builders) == 0 && len(r.attached) == 0 {
		return FailBoot(errors.New(r.requireActions))
	}
	full := parentPrefix + r.prefix
	sh := make([]MiddlewareOption, 0, len(inherited)+len(r.shared))
	sh = append(append(sh, inherited...), r.shared...)

	opts := make([]Option, 0, len(r.builders)+len(r.attached)+len(r.raw)+1)
	switch {
	case r.restOnly:
		if full != "" {
			opts = append(opts, RoutePrefix(full))
		}
		if r.gqlHome != "" {
			registerModulePublicPath(r.name, r.gqlHome)
		}
	case full != "":
		opts = append(opts, Path(full))
	}
	ns := r.namespace()
	for _, b := range r.builders {
		op := b(full, sh)
		if a, ok := op.(namespaceAnnotator); ok {
			a.setNamespace(ns)
		}
		opts = append(opts, op)
	}
	for _, op := range r.attached {
		if a, ok := op.(namespaceAnnotator); ok {
			a.setNamespace(ns)
		}
		if a, ok := op.(sharedMiddlewareAnnotator); ok {
			// Prepend in reverse so the shared options run in declaration
			// order, ahead of the op's own.
			for i := len(sh) - 1; i >= 0; i-- {
				a.prependSharedMiddleware(sh[i])
			}
		}
		opts = append(opts, op)
	}
	opts = append(opts, r.raw...)

	out := []Option{Module(r.name, opts...)}
	for _, c := range r.children {
		if c.gqlHome == "" {
			c.gqlHome = r.gqlHome
		}
		out = append(out, c.expand(full, sh))
	}
	return Options(out...)
}

// namespace is the prefix of the route names the router's routes take: the
// names of the routers that include it, then its own ("v1:billing").
func (r *Router) namespace() string {
	if r.up == nil {
		return r.name
	}
	return joinName(r.up.namespace(), r.name)
}

// namespaceAnnotator is implemented by routes, which a router stamps with
// its namespace as it expands.
type namespaceAnnotator interface{ setNamespace(string) }

// sharedMiddlewareAnnotator lets a router apply its shared options to an op
// that was already constructed (the decorator form registers ops before the
// router tree is assembled). Implemented by the REST/GraphQL/WS option types.
type sharedMiddlewareAnnotator interface {
	prependSharedMiddleware(MiddlewareOption)
}

// ---- decorator-form runtime (//nexus:router + //nexus:on) ----------------------------

// routerSpec is one //nexus:router declaration recorded by generated code.
type routerSpec struct {
	name, prefix, parent string
	shared               []MiddlewareOption
}

var routerRegistry = struct {
	sync.Mutex
	decls   []routerSpec
	members map[string][]Option
}{members: map[string][]Option{}}

// RouterDecl declares a named router for decorator-form registration —
// generated by `//nexus:router <name> <prefix> [parent=<name>]` on a package doc
// comment. Hand-written code uses NewRouter instead. Declarations from every
// package are assembled into one tree when the app boots.
func RouterDecl(name, prefix, parent string, shared ...MiddlewareOption) Option {
	routerRegistry.Lock()
	defer routerRegistry.Unlock()
	routerRegistry.decls = append(routerRegistry.decls, routerSpec{name: name, prefix: prefix, parent: parent, shared: shared})
	return Options()
}

// OnRouter attaches registrations to a declared router by name — generated by
// the `//nexus:on <name>` modifier. The ops mount under the router's stacked
// prefix with its shared options applied.
func OnRouter(name string, ops ...Option) Option {
	routerRegistry.Lock()
	defer routerRegistry.Unlock()
	routerRegistry.members[name] = append(routerRegistry.members[name], ops...)
	return Options()
}

func init() { RegisterDeferredOptions(assembleRouters) }

// assembleRouters drains the decorator-form registry into real Routers: it
// builds the declaration tree, attaches members, and expands the roots. Runs
// as a deferred option source, after every package init has recorded its
// declarations and memberships.
func assembleRouters() []Option {
	routerRegistry.Lock()
	decls := routerRegistry.decls
	members := routerRegistry.members
	routerRegistry.decls = nil
	routerRegistry.members = map[string][]Option{}
	routerRegistry.Unlock()
	if len(decls) == 0 && len(members) == 0 {
		return nil
	}

	routers := map[string]*Router{}
	for _, d := range decls {
		if prev, dup := routers[d.name]; dup {
			if prev.prefix == d.prefix && prev.parent == d.parent {
				continue // agreeing duplicate (re-generated file), harmless
			}
			return []Option{FailBoot(fmt.Errorf("nexus: router %q declared twice with different definitions", d.name))}
		}
		r := NewRouter(d.name, d.prefix, d.shared...)
		r.parent = d.parent // provisional; verified below
		routers[d.name] = r
	}
	names := make([]string, 0, len(routers))
	for n := range routers {
		names = append(names, n)
	}
	sort.Strings(names)

	var roots []*Router
	for _, n := range names {
		r := routers[n]
		if r.parent == "" {
			roots = append(roots, r)
			continue
		}
		p, ok := routers[r.parent]
		if !ok {
			return []Option{FailBoot(fmt.Errorf("nexus: router %q names unknown parent %q (declared: %s)",
				n, r.parent, strings.Join(names, ", ")))}
		}
		r.parent = "" // Include re-sets it; clear the provisional value
		p.Include(r)
	}
	memberNames := make([]string, 0, len(members))
	for n := range members {
		memberNames = append(memberNames, n)
	}
	sort.Strings(memberNames)
	for _, n := range memberNames {
		r, ok := routers[n]
		if !ok {
			return []Option{FailBoot(fmt.Errorf("nexus: //nexus:on names unknown router %q (declared: %s)",
				n, strings.Join(names, ", ")))}
		}
		for _, op := range members[n] {
			r.attach(op)
		}
	}

	out := make([]Option, 0, len(roots))
	for _, r := range roots {
		out = append(out, r.expand("", nil))
	}
	// A cycle leaves some routers unexpanded (never reached from a root).
	for _, n := range names {
		if !routers[n].expanded {
			return []Option{FailBoot(fmt.Errorf("nexus: router %q is unreachable — its parent chain forms a cycle", n))}
		}
	}
	return out
}

// prependSharedMiddleware implementations: a Router applies its shared
// options to already-constructed ops (the decorator form) by prepending each
// bundle, so shared middleware runs in declaration order ahead of the op's
// own — the same order the builder path produces.

func (r *Route) prependSharedMiddleware(m MiddlewareOption) {
	r.each(func(x *Route) {
		x.cfg.bundles = append([]middleware.Middleware{m.mw}, x.cfg.bundles...)
		stampRequiresTag(&x.cfg.baseEndpointConfig, m.mw.Requires)
	})
}

func (g *gqlFieldOption) prependSharedMiddleware(m MiddlewareOption) {
	info := m.mw.AsInfo()
	if m.mw.Graph != nil {
		g.cfg.middlewares = append([]namedMw{{
			name:        info.Name,
			description: info.Description,
			mw:          m.mw.Graph,
		}}, g.cfg.middlewares...)
	}
	g.cfg.bundles = append([]middleware.Middleware{m.mw}, g.cfg.bundles...)
	stampRequiresTag(&g.cfg.baseEndpointConfig, m.mw.Requires)
}

func (w *wsOption) prependSharedMiddleware(m MiddlewareOption) {
	w.cfg.bundles = append([]middleware.Middleware{m.mw}, w.cfg.bundles...)
	stampRequiresTag(&w.cfg.baseEndpointConfig, m.mw.Requires)
}

// pathOption is the marker carrying a module's public URL
// path. nexus.Module() picks it out of its opts list and uses it
// twice: as a RoutePrefix for REST endpoints in the module, and
// to register the module's GraphQL mount path under <path>/graphql.
type pathOption struct{ path string }

func (pathOption) nexusOption() di.Option { return di.Options() }

// Apply lets pathOption satisfy Option for nexus.Module. The
// dual ComponentOption role was dropped with the .nlt template
// engine removal — Path is now a module-prefix concept only.
// normalizedPath returns the path with module-prefix conventions
// applied: leading slash, no trailing slash, and "/" alone treated
// as the empty no-op prefix. Used by Module() when concatenating
// the value into a REST/GraphQL route prefix and by tests that
// assert the loose-input → canonical-prefix behavior.
func (p pathOption) normalizedPath() string { return normalizeRoutePrefix(p.path) }

// Path sets the module's public URL path. Equivalent to
// declaring both nexus.RoutePrefix(path) on the module AND
// service.AtGraphQL(path+"/graphql") on the module's service —
// expressed once, kept in sync.
//
//	var Module = nexus.Module("billing",
//	    nexus.Path("/billing"),
//	    nexus.Provide(NewService),
//	    nexus.AsRest("POST", "/charge", NewCharge),
//	    nexus.AsQuery(NewListInvoices),
//	)
//
// Effect: REST endpoints mount under /billing/* and GraphQL
// fields belonging to this module mount at /billing/graphql.
//
// Why bother (vs a deployment-level prefix in the manifest):
// Path travels with the module — same URL in monolith and split
// deployments. The SPA's calls to /billing/graphql work in both
// shapes without conditional client logic.
//
// Convention: the module name (first arg of nexus.Module) and
// the *Service name (passed to app.Service in the constructor)
// must match for the GraphQL path override to apply. Path looks
// up app.Service(name) by the module's name; if the service
// uses a different name, declare AtGraphQL explicitly for that
// service instead.
//
// Leading slash is added if missing; trailing slash is trimmed.
//
// The raw path is stored unchanged; module-side consumers
// normalize when reading (so "/" → "" for prefix purposes).
func Path(path string) PathOpt {
	return pathOption{path: path}
}

// PathOpt is the static type Path returns. Satisfies Option so the
// value composes into a nexus.Module as a public-path prefix.
// Defined as a named interface so future Path behaviors can be
// added without breaking callers.
type PathOpt interface {
	Option
}

// modulePublicPath maps module name → public path (e.g. "uaa" →
// "/billing"). Populated when nexus.Module() encounters a
// PublicPath option among its children. Read by app.Service when
// constructing a Service whose name matches a registered module —
// the service's GraphQL mount path is then derived as
// <publicPath>/graphql instead of the framework default.
//
// Last write wins so a re-import in tests is deterministic.
var (
	modulePublicPathMu sync.RWMutex
	modulePublicPath   = map[string]string{}
)

// registerModulePublicPath stores the (module, path) mapping.
// Empty inputs are no-ops so PublicPath("") doesn't poison the
// registry.
func registerModulePublicPath(module, path string) {
	if module == "" || path == "" {
		return
	}
	modulePublicPathMu.Lock()
	defer modulePublicPathMu.Unlock()
	modulePublicPath[module] = path
}

// modulePublicPathOf returns the registered public path for the
// given module/service name, or "" when none is set. Read by
// (*App).Service to override the new Service's GraphQL path.
func modulePublicPathOf(name string) string {
	modulePublicPathMu.RLock()
	defer modulePublicPathMu.RUnlock()
	return modulePublicPath[name]
}
