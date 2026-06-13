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
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/client"
	"github.com/paulmanoni/nexus/extension/cron"
	"github.com/paulmanoni/nexus/extension/dashboard"
	"github.com/paulmanoni/nexus/extension/metrics"
	"github.com/paulmanoni/nexus/extension/ratelimit"
	"github.com/paulmanoni/nexus/manifest"
	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/resource"
	"github.com/paulmanoni/nexus/trace"
	"github.com/paulmanoni/nexus/transport/gql"
)

// EnvAdminToken is the env var the framework reads for the admin
// token gating GET /__nexus/manifest. Empty value (or unset) leaves
// the endpoint unmounted — fail-closed so a forgotten orchestrator
// config doesn't expose declared services / env / crons.
const EnvAdminToken = "NEXUS_ADMIN_TOKEN" // #nosec G101 -- env var name, not a credential value

const defaultDashboardName = "Nexus"

type App struct {
	engine *gin.Engine
	// extValues is a per-app key/value store extensions use to stash
	// boot-time state they must read at request time without relying on
	// gin-middleware install ordering (which fx.Module route registration
	// can run ahead of). The Inertia engine lives here; the page renderer
	// pulls it via AppFromGin(c) → App.Value(...). See SetValue/Value.
	extValues    sync.Map
	registry     *registry.Registry
	bus          *trace.Bus
	cronSched    *cron.Scheduler
	rlStore      ratelimit.Store
	metricsStore metrics.Store
	// liveNotifier signals "registry state changed" to the dashboard's
	// live snapshot stream. Wired in New so registry mutations push
	// snapshots instead of poll.
	liveNotifier *Notifier
	// schemaRefsMu guards schemaRefs which holds the deduped pool of
	// named-struct shapes referenced by endpoint ArgsSchema /
	// ReturnSchema. Populated lazily as endpoints register; surfaced
	// by /__nexus/client/manifest.json's Refs section. Lazy init via
	// schemaRefs() so non-client apps pay nothing.
	schemaRefsMu  sync.Mutex
	schemaRefsMap map[string]registry.NamedType

	// clientHandler holds the live *client.Handler when the SDK is
	// mounted (Config.Client.Enabled OR via nexus.ClientUse). nil
	// otherwise. Exposed via ClientHandler() so auth.Module's option
	// chain can wire the AuthInfo bridge without forcing every app
	// to thread auth.Manager through Mount manually.
	clientHandler *client.Handler
	// clientCfg retains Config.Client so the dev-only SDK fallback
	// (devAutoMountClientSDK, wired in fxLateOptions) can honor the
	// user's settings + the DevDisabled opt-out without threading cfg
	// through the late invoke.
	clientCfg client.Config
	// cacheMgr is always non-nil — created by New() with a default
	// memory-only config when the user doesn't supply one. Downstream
	// stores (metrics, rate-limit overrides) can rely on it and Redis
	// takes over automatically when env vars enable it.
	cacheMgr      Cache
	dashboardOn   bool
	dashboardName string
	// defaultGate, when non-nil, is a middleware the framework prepends to
	// every endpoint chain (REST/GraphQL/WS) unless the endpoint opts out
	// with Public() — the primitive behind deny-by-default auth. Supplied
	// by an extension (e.g. auth's Authorization.Default) and stashed here
	// by applyDefaultGate before any endpoint mounts. See EndpointGate.
	defaultGate *EndpointGate
	// graphqlPath is the default mount path used by (*App).Service
	// when AtGraphQL isn't called on the returned *Service. Empty
	// means "/graphql" (the DefaultGraphQLPath const). Config.GraphQLPath
	// and the WithGraphQLPath app option both write here.
	graphqlPath string
	// routePrefix is prepended to every user-mounted route (REST,
	// GraphQL, WebSocket) at registration time. Set from
	// Config.Server.RoutePrefix (which DeploymentDefaults populates
	// from nexus.toml's per-deployment `prefix:`). Empty
	// disables prefixing. Normalized once at newApp time so per-
	// route mount sites can concatenate without re-trimming.
	routePrefix string
	// dashboardMw is the ordered list of gin.HandlerFunc realizations
	// that guard /__nexus. WithDashboardMiddleware + Config.DashboardMiddleware
	// populate it; Mount applies them to the route group.
	dashboardMw []gin.HandlerFunc

	// introspect + introspectionNets are the parsed Config knobs used
	// to gate GraphQL __schema queries (in addition to the dashboard).
	// True / non-empty network = bypass; the GraphQL adapter calls
	// AllowIntrospection on every request via WithAllowIntrospection.
	introspect        bool
	introspectionNets []*net.IPNet

	// environment is the named target this binary is booting into,
	// resolved from Config.Environment / NEXUS_ENVIRONMENT / default
	// ("production"). Drives the per-environment Override merge at
	// fx.Start; surfaced on /__nexus/config.
	environment string
	// version is the binary's release tag. Defaults to "dev" via newApp
	// when the user doesn't pass one.
	version string

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

	// plugins records metadata for every extension.Use registration so
	// the dashboard / SDK can enumerate what's wired in. Methods live
	// on *App (plugins.go); the field is lazy-initialized by
	// RegisterPlugin so apps that never use plugins pay nothing.
	plugins *pluginState

	// gqlStats is the per-app registry of DocumentCaches keyed by
	// GraphQL mount path. Populated by autoMountGraphQL when it
	// wires gql.WithStatsRegistry into each Mount call. Read by
	// the dashboard (/__nexus/graphql/cache + live WS snapshot)
	// so operators can verify the cache is hitting.
	gqlStats *gql.StatsRegistry

	// devReloadExclude holds the operator's extra live-reload ignore
	// globs (Config.DevReload.Exclude / [runtime.devreload] exclude).
	// Read by mountFrontend when it starts the NEXUS_DEV=1 watcher.
	devReloadExclude []string
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
	traceCapacity := cfg.TraceCapacity
	if traceCapacity == 0 && cfg.Dashboard.Enabled {
		// 1024 events covers a few hundred requests in a typical dev
		// session; user override via Config.TraceCapacity.
		traceCapacity = 1024
	}

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
		engine:           engine,
		dashboardName:    dashboardName,
		version:          version,
		environment:      cfg.Environment,
		graphqlPath:      cfg.GraphQL.Path,
		dashboardOn:      cfg.Dashboard.Enabled,
		cacheMgr:         cfg.Stores.Cache,
		rlStore:          cfg.Stores.RateLimit,
		metricsStore:     cfg.Stores.Metrics,
		listeners:        listeners,
		routePrefix:      normalizeRoutePrefix(cfg.Server.RoutePrefix),
		gqlStats:         gql.NewStatsRegistry(),
		devReloadExclude: cfg.DevReload.Exclude,
	}
	if traceCapacity > 0 {
		a.bus = trace.NewBus(traceCapacity)
	}

	a.liveNotifier = NewNotifier()
	// NOTE: we deliberately do NOT forward trace.Bus events into the
	// live notifier. Each finished request would push a fresh
	// dashboard snapshot, and the snapshot replaces nodes.value +
	// rawEdges.value on the canvas — Vue Flow regenerates edge SVG
	// paths, the in-flight packet animation's pathEl.isConnected
	// flips false mid-flight, and the packet drops. With pushes
	// arriving every ~160ms (one per request) and packets needing
	// ~850ms to complete, no animation finishes — UI looks stuck.
	//
	// Dashboard live updates therefore come from two sources:
	//   1. Structural changes (registry/cron/ratelimit OnChange)
	//      push immediately within 50ms.
	//   2. Stats updates lag by up to heartbeatInterval (5s).
	//
	// The dashboard's /__nexus/events stream is a separate WS that
	// drives the activity rail + packet animations; it doesn't
	// re-render the canvas, so per-request real-time stays intact.
	// Introspection gate sits BEFORE every other dashboard middleware
	// so an unauthorized peer 404s before any auth/permission stack
	// sees the request. Pre-parsing the CIDR list here surfaces a
	// bad config at boot rather than the first dashboard request;
	// errors panic since New() doesn't return one (parsing is
	// pure-config, deterministic, and must succeed for the binary
	// to be safely runnable).
	introspectionNets, err := parseIntrospectionNetworks(cfg.IntrospectionNetworks)
	if err != nil {
		panic(err)
	}
	a.introspect = cfg.Introspection
	a.introspectionNets = introspectionNets
	if gate := introspectionGate(cfg.Introspection, introspectionNets); gate != nil {
		a.dashboardMw = append(a.dashboardMw, gate)
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
	// Push-on-change: every mutating subsystem wakes the dashboard's
	// snapshot stream within ~50ms. Without this, the dashboard only
	// emits on the heartbeat ticker and operator actions (pause cron,
	// adjust rate limit, worker status flip) lag visibly.
	a.registry.OnChange(a.liveNotifier.Notify)
	a.cronSched = cron.NewScheduler(a.bus, 0)
	a.cronSched.OnChange(a.liveNotifier.Notify)
	// Cache is OPTIONAL: the core never needs one. It's wired only when
	// the app declares it (cache.Bind[T] / Config.Stores.Cache), which is
	// also the only thing that pulls Redis/gocache into the build. A nil
	// cacheMgr just means App.Cache() returns nil.
	if a.rlStore == nil {
		a.rlStore = ratelimit.NewMemoryStore()
	}
	a.rlStore.OnChange(a.liveNotifier.Notify)
	if a.metricsStore == nil {
		// Default to the in-process memory store — no cache dependency.
		// Apps wanting cache-backed (multi-replica) counters set
		// Config.Stores.Metrics = cache.NewMetricsStore(mgr).
		a.metricsStore = metrics.NewMemoryStore()
	}
	// Register ratelimit + metrics as built-in plugins so App.Plugins()
	// lists them alongside user-installed extensions. Routes for
	// /__nexus/ratelimits + /__nexus/stats live in extension/ratelimit
	// and extension/metrics respectively — dashboard.Mount delegates to
	// them. The records below capture metadata only; the routes mount
	// via the dashboard path because these stores are framework-owned,
	// not user-opt-in.
	a.RegisterPlugin(PluginRecord{
		Name:         "ratelimit",
		Version:      "1",
		HasDashboard: true,
	})
	a.RegisterPlugin(PluginRecord{
		Name:         "metrics",
		Version:      "1",
		HasDashboard: true,
	})
	a.RegisterPlugin(PluginRecord{
		Name:         "cron",
		Version:      "1",
		HasDashboard: true,
	})
	// Cache has no dashboard surface — it's plumbing consumed by
	// metrics + user code via App.Cache(). Listed for discovery only.
	a.RegisterPlugin(PluginRecord{
		Name:    "cache",
		Version: "1",
	})
	// Dashboard registers itself only when Config.Dashboard.Enabled
	// flips it on (the actual Mount call below). Conditional record
	// keeps app.Plugins() honest — a binary with the dashboard off
	// shouldn't claim it's wired.
	if a.dashboardOn {
		a.RegisterPlugin(PluginRecord{
			Name:         "dashboard",
			Version:      "1",
			HasDashboard: true,
		})
	}
	if a.dashboardOn {
		dashboard.Mount(a.engine, a.registry, a.bus, a.cronSched, a.rlStore, a.metricsStore, a.liveNotifier, a.gqlStats, dashboard.Config{
			Name:       a.dashboardName,
			Middleware: a.dashboardMw,
			Version:    a.version,
			// Manifest closure builds against the live *App on every
			// request — same Build() path print mode and `nexus build
			// --emit-manifest` use, so the three consumer surfaces
			// stay byte-equivalent (modulo App.GeneratedAt, which
			// ComputeHash excludes).
			Manifest: func() manifest.Manifest {
				// Prefer the effective (post-merge, post-validate)
				// manifest computed at boot — that's what the platform
				// should consume. Pre-boot inspection (print mode,
				// requests during startup) falls back to building from
				// the declared base.
				if eff := a.EffectiveManifest(); eff != nil {
					return *eff
				}
				return manifest.Build(a.manifestInputs())
			},
			// AdminToken from env. Empty → dashboard.Mount leaves the
			// endpoint unmounted (fail-closed). The orchestration
			// platform sets NEXUS_ADMIN_TOKEN at deploy time.
			AdminToken: os.Getenv(EnvAdminToken),
			// Plugins closure adapts app.Plugins() (PluginRecord) into
			// the dashboard's own PluginInfo shape so the package can
			// surface plugin metadata without importing nexus.
			Plugins: func() []dashboard.PluginInfo {
				records := a.Plugins()
				out := make([]dashboard.PluginInfo, 0, len(records))
				for _, r := range records {
					info := dashboard.PluginInfo{
						Name:         r.Name,
						Version:      r.Version,
						Namespace:    r.Namespace,
						HasDashboard: r.HasDashboard,
						HasClient:    r.HasClient,
						HasGenerate:  r.HasGenerate,
						LiveEvents:   r.LiveEvents,
					}
					if r.Tab != nil {
						info.Tab = &dashboard.TabInfo{
							ID:    r.Tab.ID,
							Label: r.Tab.Label,
							Icon:  r.Tab.Icon,
						}
					}
					out = append(out, info)
				}
				return out
			},
		})
	}
	// Client SDK mounting moved into frontend.Plugin (and remains
	// reachable via the option-chain nexus.ClientUse for apps that
	// don't use the frontend extension). cfg.Client is still read
	// here as a back-compat bridge — when set, we synthesize a
	// ClientUse-shaped registration so apps that haven't migrated
	// keep working. The field is deprecated; new code declares
	// frontend.Plugin(...) instead.
	// Retain for the dev-only SDK fallback (devAutoMountClientSDK).
	a.clientCfg = cfg.Client
	switch {
	case cfg.SDK && (IsDev() || cfg.Introspection):
		// One-switch SDK (PocketBase-style): full manifest across REST +
		// GraphQL + WS, plus frontend defaults so the SDK files dump and
		// tsconfig wires when a frontend dir is present. Gated to dev or
		// introspection so the API surface is never exposed by this flag
		// alone in a locked-down production binary. The OnStart dump hook
		// (obs_integration.go) writes files only when a frontend dir was
		// detected — a no-op in a fileless production tree.
		sdkCfg := client.ApplyFrontendDefaults(client.Config{Enabled: true, Public: true})
		a.clientHandler = client.Mount(a.engine, a.registry, nil, a.SchemaRefs, a.routePrefix, sdkCfg)
	case cfg.Client.Enabled:
		clientCfg := client.ApplyVisibilityDefaults(cfg.Client, cfg.Introspection)
		a.clientHandler = client.Mount(a.engine, a.registry, nil, a.SchemaRefs, a.routePrefix, clientCfg)
	}

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

// Static serves the files in the local directory dir at the URL
// prefix urlPrefix — app.Static("/media", "media") makes
// ./media/cat.png reachable at /media/cat.png. The prefix is mounted
// under the deployment route prefix when one is set.
//
// This is the guarded counterpart to Engine().Static(). gin panics at
// boot when a static mount registers a /*filepath catch-all at the
// root, which is what happens whenever the prefix is blank or "/":
// the catch-all collides with framework routes (the /__nexus
// dashboard, your API, the SPA fallback) and takes the whole app
// down. A blank/root prefix is exactly what an unset config value
// yields — e.g. nexus.Get on a key that isn't in the config store
// returns "" — so the failure is easy to hit by accident.
//
// Static refuses to crash for it: a blank, root, or /__nexus-shadowing
// prefix is logged and skipped, the app keeps booting, and the
// misconfiguration shows up in the logs instead of a panic stack. A
// valid prefix mounts exactly like Engine().Static.
func (a *App) Static(urlPrefix, dir string) {
	p := a.routePrefix + normalizeRoutePrefix(urlPrefix)
	switch {
	case p == "":
		log.Printf("nexus: Static(urlPrefix=%q, dir=%q): blank/root prefix would register a root catch-all that collides with framework routes — skipping (use a non-root prefix like %q)", urlPrefix, dir, "/media")
		return
	case p == "/__nexus" || strings.HasPrefix(p, "/__nexus/"):
		log.Printf("nexus: Static(urlPrefix=%q): the /__nexus prefix is reserved by the dashboard — skipping", urlPrefix)
		return
	}
	a.engine.Static(p, dir)
}

// schemaRefs returns the app's shared pool of walked named struct
// types. Lazy-initialized so apps that never run the client SDK
// generator carry no allocation. Called by recordEndpointSchema
// during AsRest / AsWS registration; the client manifest builder
// reads the same map at request time.
//
// The map is mutated in-place by registry.WalkType during walks;
// the lock guards the lazy init only. WalkType itself is single-
// threaded per call (one endpoint at a time during fx graph
// construction), so concurrent walks aren't a concern.
func (a *App) schemaRefs() map[string]registry.NamedType {
	a.schemaRefsMu.Lock()
	if a.schemaRefsMap == nil {
		a.schemaRefsMap = map[string]registry.NamedType{}
	}
	m := a.schemaRefsMap
	a.schemaRefsMu.Unlock()
	return m
}

// SchemaRefs is the public accessor for the deduped named-type
// pool. Used by the client SDK package (client/) at manifest-build
// time to populate the Refs section. Returns the live map; callers
// MUST NOT mutate.
func (a *App) SchemaRefs() map[string]registry.NamedType {
	return a.schemaRefs()
}

// ClientHandler returns the live *client.Handler when the client
// SDK is mounted (Config.Client.Enabled OR a nexus.ClientUse
// option fired in the chain), nil otherwise. Used by auth.Module's
// option chain to wire the AuthInfo bridge without forcing app
// code to thread the manager through Mount manually.
func (a *App) ClientHandler() *client.Handler {
	return a.clientHandler
}

// devAutoMountClientSDK mounts the client SDK's manifest + runtime
// routes under `nexus dev` (NEXUS_DEV=1) when nothing else has — so a
// plain ServeFrontend SPA still exposes /__nexus/client/manifest.json.
// `nexus dev` reads that manifest to auto-sync the vite proxy (folding
// in every module's RoutePrefix), and the SDK + auto-select plugin
// consume it too. Wired into fxLateOptions so it runs AFTER user opts:
// an explicit Config.Client.Enabled / nexus.ClientUse / frontend.Plugin
// mount wins (idempotent — skips once a handler exists).
//
// Dev-only and side-effect-free: OutDir is forced empty so the start-
// time SDK file dump + vite.config auto-edit never fire; only HTTP
// routes are added. Production never sets NEXUS_DEV=1. Opt out with
// Config.Client.DevDisabled — the "closed manually" escape hatch.
func devAutoMountClientSDK(a *App) {
	if !IsDev() || a.clientCfg.DevDisabled {
		return
	}
	if a.clientHandler != nil {
		return // already mounted via Enabled / ClientUse / frontend.Plugin
	}
	cc := a.clientCfg
	cc.Enabled = true
	cc.OutDir = "" // never dump files from the implicit dev mount
	// Dev gets the FULL manifest (every module endpoint) so the proxy
	// sync + nx.query/mutate/crud work — the same visibility
	// introspection grants, which `nexus dev` already bypasses on.
	clientCfg := client.ApplyVisibilityDefaults(cc, true)
	a.clientHandler = client.Mount(a.engine, a.registry, nil, a.SchemaRefs, a.routePrefix, clientCfg)
}

// SetClientAuthInfo installs the manifest's auth-section provider
// on the mounted SDK handler. No-op when the client SDK isn't
// mounted (apps that don't ship the SDK pay nothing). Designed for
// auth.Module's option chain to call via fx.Invoke once both
// auth.Manager and the SDK are in scope.
func (a *App) SetClientAuthInfo(fn func() client.ExtractorInfo) {
	if a.clientHandler != nil {
		a.clientHandler.SetAuthInfo(fn)
	}
}
func (a *App) Scheduler() *cron.Scheduler   { return a.cronSched }
func (a *App) RateLimiter() ratelimit.Store { return a.rlStore }

// Environment returns the resolved environment name ("production",
// "staging", "preview", ...) the binary is booting into. Set from
// Config.Environment or NEXUS_ENVIRONMENT; never empty.
func (a *App) Environment() string { return a.environment }

// Version is the binary's release tag, defaulting to "dev". Surfaced on
// /__nexus/config.
func (a *App) Version() string { return a.version }

func (a *App) Metrics() metrics.Store { return a.metricsStore }
func (a *App) Cache() Cache           { return a.cacheMgr }

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

// AllowIntrospection reports whether the request c is permitted to
// see schema-shape data — drives both the dashboard gate and the
// GraphQL adapter's __schema-query block. Returns true when
// Config.Introspection is true OR the TCP peer falls within
// Config.IntrospectionNetworks. Exposed so transport adapters
// outside the nexus package (notably gql) can consult one source
// of truth.
func (a *App) AllowIntrospection(c *gin.Context) bool {
	if devModeBypass() {
		return true
	}
	return introspectionAllowed(c, a.introspect, a.introspectionNets)
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
