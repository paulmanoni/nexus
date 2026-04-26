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
}

// AppOption is the functional-option type for nexus.New. Named AppOption
// (not Option) so nexus.Option can be reserved for the top-level
// fx-wrapping builder type used by nexus.Provide / Module / Invoke / Run.
type AppOption func(*App)

// WithEngine supplies a pre-configured Gin engine. Without it, nexus builds a
// bare engine with just Recovery so the caller can bring their own logger.
func WithEngine(e *gin.Engine) AppOption {
	return func(a *App) { a.engine = e }
}

// WithTracing enables per-request trace events, buffered in a ring of the given
// capacity. Required for the dashboard's event stream to show anything.
func WithTracing(capacity int) AppOption {
	return func(a *App) { a.bus = trace.NewBus(capacity) }
}

// WithDashboard mounts /__nexus/endpoints (always) and /__nexus/events (if tracing is on).
func WithDashboard() AppOption {
	return func(a *App) { a.dashboardOn = true }
}

// WithDashboardName sets the brand shown in the dashboard header and the
// browser tab title. Defaults to "Nexus". The name is served over
// /__nexus/config so the client picks it up without a rebuild.
func WithDashboardName(name string) AppOption {
	return func(a *App) { a.dashboardName = name }
}

// WithGraphQLPath overrides the default GraphQL mount path used by
// services that don't call (*Service).AtGraphQL themselves. Empty
// falls back to DefaultGraphQLPath ("/graphql").
func WithGraphQLPath(path string) AppOption {
	return func(a *App) { a.graphqlPath = path }
}

// WithRateLimitStore swaps the default in-memory rate-limit store for a
// custom implementation — typically ratelimit.NewRedisStore(...) in a
// multi-replica deploy. Pass this to nexus.New / via nexus.Config when
// you want limit state (counters, overrides) to survive restarts or be
// shared across processes.
func WithRateLimitStore(s ratelimit.Store) AppOption {
	return func(a *App) { a.rlStore = s }
}

// WithMetricsStore swaps the default cache-backed metrics store. Useful
// when you want a Prometheus- or StatsD-backed implementation; the
// built-in dashboard /__nexus/stats endpoint reads from whichever
// Store is installed.
func WithMetricsStore(s metrics.Store) AppOption {
	return func(a *App) { a.metricsStore = s }
}

// WithCache installs a user-provided cache.Manager instead of the
// default one nexus creates. Pass the same Manager users' app code
// receives from fx (e.g. via cache.Module) so every cache consumer —
// user code, metrics, rate-limit overrides — hits one store.
func WithCache(m *cache.Manager) AppOption {
	return func(a *App) { a.cacheMgr = m }
}

// WithDeployment names the deployment unit this binary runs as. Empty =
// monolith. Mirrors Config.Deployment for callers using the lower-level
// nexus.New(...) entry point.
func WithDeployment(name string) AppOption {
	return func(a *App) { a.deployment = name }
}

// WithVersion stamps the binary's version onto /__nexus/config. Used by
// generated clients to detect peer-version skew across services in a
// split deployment.
func WithVersion(v string) AppOption {
	return func(a *App) { a.version = v }
}

// WithTopology installs the peer table consulted by codegen'd remote
// clients. Mirrors Config.Topology for callers using the lower-level
// nexus.New(...) entry point. Each entry binds a DeployAs tag to a
// Peer with URL / Timeout / Auth / MinVersion / Retries.
func WithTopology(t Topology) AppOption {
	return func(a *App) { a.topology = t }
}

// WithDashboardMiddleware gates the /__nexus surface behind one or
// more middleware bundles. Each bundle's Gin realization runs in
// registration order before any dashboard handler. Typical use:
//
//	nexus.WithDashboardMiddleware(
//	    middleware.Middleware{Name: "auth",  Gin: bearerAuth},
//	    middleware.Middleware{Name: "admin", Gin: requireAdminRole},
//	)
//
// Bundles without a Gin realization are skipped silently — the
// dashboard is an HTTP surface, so graph-only bundles don't apply.
func WithDashboardMiddleware(bundles ...middleware.Middleware) AppOption {
	return func(a *App) {
		for _, b := range bundles {
			if b.Gin != nil {
				a.dashboardMw = append(a.dashboardMw, b.Gin)
			}
		}
	}
}

func New(opts ...AppOption) *App {
	a := &App{dashboardName: defaultDashboardName, version: "dev"}
	for _, opt := range opts {
		opt(a)
	}
	if a.engine == nil {
		a.engine = gin.New()
		a.engine.Use(gin.Recovery())
	}
	a.registry = registry.New()
	a.cronSched = cron.NewScheduler(a.bus, 0)
	// Cache is non-optional: if the caller didn't inject one, we build
	// a memory-backed Manager so downstream stores never need to
	// branch on "is there a cache". Redis kicks in automatically when
	// env vars ask for it (see cache.NewConfig / cache.NewManager).
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
