package nexus

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/paulmanoni/nexus/di"
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
		return Error(fmt.Errorf("nexus: router %q is included in %q — pass only the root router", r.name, r.parent)).nexusOption()
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
		return Error(fmt.Errorf("nexus: router %q mounted twice — the same router passed twice", r.name)).nexusOption()
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
		return Error(fmt.Errorf("nexus: router %q mounted twice — a cycle, or the same router passed twice", r.name))
	}
	r.expanded = true
	for _, h := range r.hooks {
		h()
	}
	if len(r.errs) > 0 {
		return Error(r.errs[0])
	}
	if r.requireActions != "" && len(r.builders) == 0 && len(r.attached) == 0 {
		return Error(errors.New(r.requireActions))
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
	for _, b := range r.builders {
		opts = append(opts, b(full, sh))
	}
	for _, op := range r.attached {
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

// sharedMiddlewareAnnotator lets a router apply its shared options to an op
// that was already constructed (the decorator form registers ops before the
// router tree is assembled). Implemented by the REST/GraphQL/WS option types.
type sharedMiddlewareAnnotator interface {
	prependSharedMiddleware(MiddlewareOption)
}

// ---- decorator-form runtime (//@router + //@on) ----------------------------

// routerSpec is one //@router declaration recorded by generated code.
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
// generated by `//@router <name> <prefix> [parent=<name>]` on a package doc
// comment. Hand-written code uses NewRouter instead. Declarations from every
// package are assembled into one tree when the app boots.
func RouterDecl(name, prefix, parent string, shared ...MiddlewareOption) Option {
	routerRegistry.Lock()
	defer routerRegistry.Unlock()
	routerRegistry.decls = append(routerRegistry.decls, routerSpec{name: name, prefix: prefix, parent: parent, shared: shared})
	return Options()
}

// OnRouter attaches registrations to a declared router by name — generated by
// the `//@on <name>` modifier. The ops mount under the router's stacked
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
			return []Option{Error(fmt.Errorf("nexus: router %q declared twice with different definitions", d.name))}
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
			return []Option{Error(fmt.Errorf("nexus: router %q names unknown parent %q (declared: %s)",
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
			return []Option{Error(fmt.Errorf("nexus: //@on names unknown router %q (declared: %s)",
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
			return []Option{Error(fmt.Errorf("nexus: router %q is unreachable — its parent chain forms a cycle", n))}
		}
	}
	return out
}
