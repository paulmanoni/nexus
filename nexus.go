// Package nexus is a thin framework over Gin that registers every endpoint
// (REST, GraphQL, WebSocket) into a central registry, traces every request
// into an in-memory event bus, and exposes both under /__nexus for tooling
// — notably the Vue dashboard.
//
// nexus does NOT replace the caller's GraphQL layer: hand it a *graphql.Schema
// (typically built with github.com/paulmanoni/nexus/graph) and it mounts + introspects.
package nexus

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"go.uber.org/zap"

	"github.com/paulmanoni/nexus/cache"
	"github.com/paulmanoni/nexus/cron"
	"github.com/paulmanoni/nexus/dashboard"
	"github.com/paulmanoni/nexus/metrics"
	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/ratelimit"
	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/resource"
	"github.com/paulmanoni/nexus/trace"
)

const defaultDashboardName = "Nexus"

type App struct {
	engine        *gin.Engine
	registry      *registry.Registry
	bus           *trace.Bus
	cronSched     *cron.Scheduler
	rlStore       ratelimit.Store
	metricsStore  metrics.Store
	// cacheMgr is always non-nil — created by New() with a default
	// memory-only config when the user doesn't supply one. Downstream
	// stores (metrics, rate-limit overrides) can rely on it and Redis
	// takes over automatically when env vars enable it.
	cacheMgr      *cache.Manager
	dashboardOn   bool
	dashboardName string
	// graphqlPath is the default mount path used by (*App).Service
	// when AtGraphQL isn't called on the returned *Service. Empty
	// means "/graphql" (the DefaultGraphQLPath const). Config.GraphQLPath
	// and the WithGraphQLPath app option both write here.
	graphqlPath string
	// routePrefix is prepended to every user-mounted route (REST,
	// GraphQL, WebSocket) at registration time. Set from
	// Config.Server.RoutePrefix (which DeploymentDefaults populates
	// from nexus.deploy.yaml's per-deployment `prefix:`). Empty
	// disables prefixing. Normalized once at newApp time so per-
	// route mount sites can concatenate without re-trimming.
	routePrefix string
	// dashboardMw is the ordered list of gin.HandlerFunc realizations
	// that guard /__nexus. WithDashboardMiddleware + Config.DashboardMiddleware
	// populate it; Mount applies them to the route group.
	dashboardMw []gin.HandlerFunc

	// deployment is the unit name this binary boots as (from
	// Config.Deployment / NEXUS_DEPLOYMENT). "" = monolith. Today only
	// surfaced on /__nexus/config; future codegen'd clients will
	// consult it to choose local-shortcut vs HTTP for cross-module calls.
	deployment string
	// version is stamped on /__nexus/config so peer services in a split
	// deployment can detect version skew. Defaults to "dev" via newApp
	// when the user doesn't pass one.
	version string

	// topology is the peer table consulted by codegen'd remote clients
	// to resolve URL/Timeout/Auth/MinVersion/Retries by DeployAs tag.
	// Populated from Config.Topology (or WithTopology). Empty Peers
	// map = monolith — no peer lookups happen.
	topology Topology

	// wsEndpoints holds the per-path WebSocket state created by AsWS.
	// Multiple AsWS calls on the same path share one endpoint — the first
	// one mounts the HTTP upgrade route and starts the hub; subsequent
	// ones just add handlers to the type-dispatch table.
	wsMu        sync.Mutex
	wsEndpoints map[string]*wsEndpoint

	// listeners is the configured listener set (name → Listener). Empty
	// means single-listener back-compat mode bound to Config.Addr;
	// non-empty triggers multi-listener binding in registerLifecycle
	// and activates the scope filter middleware.
	listeners map[string]Listener
	// listenerScopes is the runtime scope-lookup table: bound-address
	// string → scope. Populated by registerLifecycle as each listener
	// actually binds, read by scopeFilterMiddleware on every request.
	listenerScopes *listenerScopes

	// health backs /__nexus/health (liveness) and /__nexus/ready
	// (liveness + peer reachability). Always non-nil — set by New()
	// so the endpoints can answer immediately, even before fx Start
	// flips the alive flag.
	health *healthState

	// manifest is the deploy-time self-description store. Populated
	// by the option helpers in manifest_app.go (DeclareEnv,
	// DeclareService, UseVolume, AddStartupTask) which run as
	// fx.Invoke at graph construction. Read by manifestInputs() at
	// print time. Never holds connections or state that needs
	// teardown — pure metadata.
	manifest manifestStore
}

// New constructs an *App from a single Config. The canonical
// (and, since v0.18, only) public constructor — both nexus.Run and
// the lower-level "build app, then app.Run(addr)" pattern feed
// through here.
//
// Behavior:
//   - Manifest-derived defaults (codegen'd by `nexus build
//     --deployment X`) fill any zero Config field; explicit Config
//     fields always win.
//   - The framework owns engine creation, scope-filter middleware,
//     /__nexus/health + /ready, the cache + ratelimit + metrics
//     stores, and (when EnableDashboard is true) the dashboard
//     mount.
//   - Listeners with empty Addrs auto-fill from the resolved
//     Config.Addr (admin = port+1000, internal = port+2000).
//   - Global rate limit and global middlewares are installed at the
//     engine root in registration order.
//
// The returned *App is fully constructed and safe to register
// endpoints/services on, but listeners aren't bound until Run is
// invoked (either nexus.Run or App.Run for direct callers).
func New(cfg Config) *App {
	cfg = resolveConfig(cfg)

	traceCapacity := cfg.TraceCapacity
	if traceCapacity == 0 && cfg.Dashboard.Enabled {
		// 1024 events covers a few hundred requests in a typical dev
		// session; user override via Config.TraceCapacity.
		traceCapacity = 1024
	}
	deployment := cfg.Deployment

	dashboardName := cfg.Dashboard.Name
	if dashboardName == "" {
		dashboardName = defaultDashboardName
	}

	version := cfg.Version
	if version == "" {
		version = "dev"
	}

	// Resolve the listener map: explicit Listeners with empty Addrs
	// auto-filled from cfg.Addr, otherwise nil (single-listener
	// back-compat at cfg.Addr).
	var listeners map[string]Listener
	if len(cfg.Server.Listeners) > 0 {
		listeners = fillListenerAddrs(cfg.Server.Listeners, cfg.Server.Addr)
	}

	// Engine. Recovery middleware first so panics surface as 500s.
	// We use the framework's own recoveryMiddleware (instead of
	// gin.Recovery()) so panic stacks land on the dashboard via the
	// trace + metrics pipeline — see recovery.go.
	engine := gin.New()
	engine.Use(recoveryMiddleware())
	// Per-request access log when gin is in debug mode (the default
	// for `nexus dev`). Mirrors gin.Default()'s behavior. In release
	// mode the logger is suppressed — operators usually wire their
	// own structured logger via Config.Middleware.Global, so we
	// don't compete with that. Set GIN_MODE=release in prod to
	// silence the dev access log.
	if gin.IsDebugging() {
		engine.Use(gin.Logger())
	}

	a := &App{
		engine:        engine,
		dashboardName: dashboardName,
		version:       version,
		deployment:    deployment,
		topology:      cfg.Topology,
		graphqlPath:   cfg.GraphQL.Path,
		dashboardOn:   cfg.Dashboard.Enabled,
		cacheMgr:      cfg.Stores.Cache,
		rlStore:       cfg.Stores.RateLimit,
		metricsStore:  cfg.Stores.Metrics,
		listeners:     listeners,
		routePrefix:   normalizeRoutePrefix(cfg.Server.RoutePrefix),
	}
	if traceCapacity > 0 {
		a.bus = trace.NewBus(traceCapacity)
	}
	for _, b := range cfg.Middleware.Dashboard {
		if b.Gin != nil {
			a.dashboardMw = append(a.dashboardMw, b.Gin)
		}
	}

	// Scope filter is installed as the first engine middleware so it
	// runs before any group middleware (notably dashboard.Mount's
	// admin gate). Empty scope table = no filtering.
	a.listenerScopes = newListenerScopes()
	a.engine.Use(scopeFilterMiddleware(a.listenerScopes))
	// Health/ready endpoints mount unconditionally so liveness and
	// readiness probes work even when EnableDashboard is false.
	a.health = newHealthState()
	mountHealth(a.engine, a.health)
	a.registry = registry.New()
	a.cronSched = cron.NewScheduler(a.bus, 0)
	// Cache is non-optional: if the caller didn't inject one, build a
	// memory-backed Manager so downstream stores never branch on "is
	// there a cache". Redis kicks in automatically when env vars ask
	// for it (see cache.NewConfig).
	if a.cacheMgr == nil {
		a.cacheMgr = cache.NewManager(cache.NewConfig(), zap.NewNop())
		a.cacheMgr.Start()
	}
	if a.rlStore == nil {
		a.rlStore = ratelimit.NewMemoryStore()
	}
	if a.metricsStore == nil {
		a.metricsStore = metrics.NewCacheStore(a.cacheMgr)
	}
	if a.dashboardOn {
		dashboard.Mount(a.engine, a.registry, a.bus, a.cronSched, a.rlStore, a.metricsStore, dashboard.Config{
			Name:       a.dashboardName,
			Middleware: a.dashboardMw,
			Deployment: a.deployment,
			Version:    a.version,
		})
	}

	// Cross-module + remote-service registrations from codegen'd
	// init() blocks. Has to run after New so app.registry exists;
	// before fx start so the dashboard's first /__nexus/endpoints
	// poll already includes every peer module.
	a.applyRemoteServicePlaceholders()
	a.applyCrossModuleDeps()

	// CORS lands first so preflights short-circuit before rate
	// limiting eats the budget; cross-origin OPTIONS shouldn't
	// count against the bucket.
	if cfg.Middleware.CORS != nil {
		a.engine.Use(corsMiddleware(*cfg.Middleware.CORS))
		a.registry.RegisterMiddleware(middleware.Info{
			Name:        "cors",
			Kind:        middleware.KindBuiltin,
			Description: "CORS (built-in)",
		})
		a.registry.RegisterGlobalMiddleware("cors")
	}

	// Global rate limit primed before any op runs.
	if !cfg.Middleware.RateLimit.Zero() {
		a.rlStore.Declare(ratelimitGlobalKey, cfg.Middleware.RateLimit)
		a.engine.Use(ratelimit.GinMiddleware(a.rlStore, ratelimit.GlobalKey, nil))
		a.registry.RegisterMiddleware(middleware.Info{
			Name:        "rate-limit",
			Kind:        middleware.KindBuiltin,
			Description: "Global rate limit (per-app bucket)",
		})
		a.registry.RegisterGlobalMiddleware("rate-limit")
	}

	// User-supplied global middlewares in registration order.
	for _, m := range cfg.Middleware.Global {
		if m.Gin != nil {
			a.engine.Use(m.Gin)
		}
		a.registry.RegisterMiddleware(m.AsInfo())
		a.registry.RegisterGlobalMiddleware(m.Name)
	}

	return a
}

func (a *App) Engine() *gin.Engine          { return a.engine }
func (a *App) Registry() *registry.Registry { return a.registry }
func (a *App) Bus() *trace.Bus              { return a.bus }
func (a *App) Scheduler() *cron.Scheduler   { return a.cronSched }
func (a *App) RateLimiter() ratelimit.Store { return a.rlStore }

// Deployment is the deployment-unit name this binary boots as, or "" for
// monolith. Read by future cross-module clients to choose the in-process
// shortcut over HTTP.
func (a *App) Deployment() string { return a.deployment }

// Version is the binary's release tag, defaulting to "dev". Surfaced on
// /__nexus/config so generated clients can detect peer-version skew.
func (a *App) Version() string { return a.version }

// Peer returns the topology binding for the given DeployAs tag.
// Generated remote clients call this at construction time to resolve
// the peer's URL/Timeout/Auth/MinVersion/Retries. The second return is
// false when the tag isn't declared in Config.Topology — codegen'd
// factories use that signal to fail fast with a precise error message.
func (a *App) Peer(tag string) (Peer, bool) {
	if a.topology.Peers == nil {
		return Peer{}, false
	}
	p, ok := a.topology.Peers[tag]
	return p, ok
}

// Topology returns the configured peer table. Read-only — modifying
// the returned map would not retroactively rewire already-constructed
// clients. Used by the dashboard and by health-check loops that probe
// every declared peer.
func (a *App) Topology() Topology { return a.topology }
func (a *App) Metrics() metrics.Store       { return a.metricsStore }
func (a *App) Cache() *cache.Manager        { return a.cacheMgr }

// RoutePrefix returns the deployment-wide path prefix applied to
// every user-mounted route (REST, GraphQL, WebSocket). Empty when
// no manifest `prefix:` (or Config.Server.RoutePrefix) was set.
func (a *App) RoutePrefix() string { return a.routePrefix }

// PrefixPath returns p with the deployment route prefix prepended.
// Mount sites use this so a single binary can be served behind a
// path-based ingress without each registration re-implementing the
// prepend. Both inputs are expected to start with "/"; the helper
// is a noop when the prefix is empty.
func (a *App) PrefixPath(p string) string {
	if a.routePrefix == "" {
		return p
	}
	return a.routePrefix + p
}

// normalizeRoutePrefix tidies a user-provided prefix so concatenation
// at mount sites stays trivial: ensures a leading "/", strips a
// trailing "/", and treats "/" alone as empty (no-op prefix).
func normalizeRoutePrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(p, "/")
}
func (a *App) Run(addr string) error {
	a.cronSched.Start()
	return a.engine.Run(addr)
}

// Register adds a resource (database, cache, queue) to the app so its health
// shows up on the dashboard. Use Service.Attach(r) to also draw an edge
// between the owning service(s) and the resource.
func (a *App) Register(r resource.Resource) {
	a.registry.RegisterResource(r)
}

// UseReporter is satisfied by any type that exposes an OnUse hook with this
// exact signature. multi.Registry and anything embedding it fit — including
// the project's own DBManager wrapper. This is a structural interface so
// nexus doesn't need to import nexus/multi directly.
type UseReporter interface {
	OnUse(func(ctx context.Context, name string))
}

// OnResourceUse installs an auto-attach hook onto any UseReporter (typically
// a *multi.Registry or a user wrapper around one). Whenever code calls
// target.UsingCtx(ctx, "resource-name") during a request, the hook:
//
//  1. reads the current trace.Span from ctx so we know which service made the call
//  2. AttachResource(service, resource) on the registry — edge appears live
//  3. emits a "downstream" trace event so the Traces tab shows the lookup
//
// Calls with no span in context (e.g. UsingCtx fired from main or a cron
// job outside the trace middleware) are silently ignored — there's no
// service to attribute the usage to.
func (a *App) OnResourceUse(target UseReporter) {
	target.OnUse(func(ctx context.Context, name string) {
		span, ok := trace.SpanFromCtx(ctx)
		if !ok {
			return
		}
		a.registry.AttachResource(span.Service, name)
		if a.bus != nil {
			a.bus.Publish(trace.Event{
				TraceID: span.TraceID,
				Kind:    trace.KindDownstream,
				Service: span.Service,
				Message: "resource.using:" + name,
				Meta:    map[string]any{"resource": name},
			})
		}
	})
}
func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.engine.ServeHTTP(w, r)
}
