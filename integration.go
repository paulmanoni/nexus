package nexus

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"

	"go.uber.org/fx"

	"github.com/paulmanoni/nexus/metrics"
	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/ratelimit"
)

// newApp is the fx provider for *App consumed by everything in the graph.
// Private because nexus.Run is the public entry point; direct fx users
// would previously have called fxmod.NewApp.
func newApp(cfg Config) *App {
	// Manifest-derived defaults (set by a codegen'd init() block when
	// `nexus build --deployment X` ran). Explicit Config fields win;
	// defaults fill the gaps. Reading happens once so the rest of
	// this function works against a single resolved Config copy.
	if defaults, ok := loadDeploymentDefaults(); ok {
		if cfg.Addr == "" {
			cfg.Addr = defaults.Addr
		}
		if cfg.Deployment == "" {
			cfg.Deployment = defaults.Deployment
		}
		if cfg.Topology.Peers == nil && defaults.Topology.Peers != nil {
			cfg.Topology = defaults.Topology
		}
		if len(cfg.Listeners) == 0 && len(defaults.Listeners) > 0 {
			cfg.Listeners = defaults.Listeners
		}
	}
	var opts []AppOption
	// Default trace capacity when the dashboard is on. Without the
	// bus, /__nexus/events isn't mounted and the dashboard's request
	// view (plus `nexus dev --split`'s per-request log) stays empty.
	// 1024 events covers a few hundred requests in a typical dev
	// session; user can override with an explicit Config.TraceCapacity.
	traceCapacity := cfg.TraceCapacity
	if traceCapacity == 0 && cfg.EnableDashboard {
		traceCapacity = 1024
	}
	if traceCapacity > 0 {
		opts = append(opts, WithTracing(traceCapacity))
	}
	if cfg.EnableDashboard {
		opts = append(opts, WithDashboard())
	}
	if cfg.DashboardName != "" {
		opts = append(opts, WithDashboardName(cfg.DashboardName))
	}
	if cfg.GraphQLPath != "" {
		opts = append(opts, WithGraphQLPath(cfg.GraphQLPath))
	}
	// Explicit stores always win. Config.Cache (when set) replaces the
	// auto-created cache.Manager so the user's app and nexus's internal
	// stores share one cache tier.
	if cfg.Cache != nil {
		opts = append(opts, WithCache(cfg.Cache))
	}
	if cfg.RateLimitStore != nil {
		opts = append(opts, WithRateLimitStore(cfg.RateLimitStore))
	}
	if cfg.MetricsStore != nil {
		opts = append(opts, WithMetricsStore(cfg.MetricsStore))
	}
	if len(cfg.DashboardMiddleware) > 0 {
		opts = append(opts, WithDashboardMiddleware(cfg.DashboardMiddleware...))
	}
	// Deployment: explicit Config wins, otherwise fall back to the
	// NEXUS_DEPLOYMENT env var so main.go doesn't have to call
	// nexus.DeploymentFromEnv() just to thread it through. Empty in
	// both places = monolith.
	deployment := cfg.Deployment
	if deployment == "" {
		deployment = os.Getenv(nexusDeploymentEnv)
	}
	if deployment != "" {
		opts = append(opts, WithDeployment(deployment))
	}
	if cfg.Version != "" {
		opts = append(opts, WithVersion(cfg.Version))
	}
	if cfg.Topology.Peers != nil {
		opts = append(opts, WithTopology(cfg.Topology))
	}
	if len(cfg.Listeners) > 0 {
		// Listener entries with empty Addr are filled in from the
		// resolved cfg.Addr (which absorbed any manifest default
		// above). That lets main.go declare just the listener
		// *shape* — `{Scope: ScopeAdmin}` etc. — and have the
		// per-deployment port flow in automatically. Explicit
		// Addrs in the map are passed through unchanged.
		filled := fillListenerAddrs(cfg.Listeners, cfg.Addr, cfg.AdminPortOffset)
		opts = append(opts, WithListeners(filled))
	} else if cfg.AdminPortOffset > 0 {
		// No explicit Listeners but the user asked for the auto
		// public+admin pair via AdminPortOffset. Build it from
		// cfg.Addr.
		if listeners, err := autoListeners(cfg.Addr, cfg.AdminPortOffset); err == nil {
			opts = append(opts, WithListeners(listeners))
		}
	}
	// When the user didn't set MetricsStore explicitly, New() already
	// defaults to metrics.NewCacheStore(a.cacheMgr) using whatever cache
	// is active — the user-supplied one or nexus's own. metrics is kept
	// unused here deliberately; import retained for future TODO on
	// ratelimit cache-backing.
	_ = metrics.NewCacheStore
	app := New(opts...)

	// Flush remote-service placeholders registered by codegen'd
	// init() blocks (zz_shadow_gen.go) into the live registry. Has
	// to run AFTER New() so app.registry exists; before fx start so
	// the dashboard's first /__nexus/endpoints poll already includes
	// every peer module.
	app.applyRemoteServicePlaceholders()
	// Apply cross-module dep registrations (consumer-module → peer
	// module) detected by the build's static AST scan and emitted
	// in zz_deploy_gen.go's init(). Runs after the placeholder pass
	// so the consumer service exists in the registry when its
	// ServiceDeps slice is appended.
	app.applyCrossModuleDeps()

	// Declare the global rate limit now so the store is primed before any
	// op runs. Per-op declarations land later via the auto-mount.
	if !cfg.GlobalRateLimit.Zero() {
		app.rlStore.Declare(ratelimitGlobalKey, cfg.GlobalRateLimit)
		// Install the built-in global rate-limit middleware on the engine
		// root so every HTTP path (REST + GraphQL + WS upgrade + the
		// dashboard itself) consults the bucket. Users who want finer
		// control can skip this and attach their own via GlobalMiddleware.
		app.engine.Use(ratelimit.GinMiddleware(app.rlStore, ratelimit.GlobalKey, nil))
		app.registry.RegisterMiddleware(middleware.Info{
			Name:        "rate-limit",
			Kind:        middleware.KindBuiltin,
			Description: "Global rate limit (per-app bucket)",
		})
		app.registry.RegisterGlobalMiddleware("rate-limit")
	}

	// Install user-supplied global middlewares in registration order.
	for _, m := range cfg.GlobalMiddleware {
		if m.Gin != nil {
			app.engine.Use(m.Gin)
		}
		app.registry.RegisterMiddleware(m.AsInfo())
		app.registry.RegisterGlobalMiddleware(m.Name)
	}

	return app
}

// ratelimitGlobalKey is the store key for the app-wide bucket. Re-declared
// here (alongside ratelimit.GlobalKey) so the integration layer stays
// self-contained — the middleware consults both via this name.
const ratelimitGlobalKey = "_global"

// registerLifecycle binds the configured HTTP listeners and cron
// scheduler to fx's start/stop hooks. Bind happens synchronously so
// port conflicts abort fx.Start() with a clean error.
//
// When app.listeners is non-empty, every entry binds and registers
// its bound address with the scope-filter table. Otherwise a single
// listener binds to cfg.Addr (or :8080 default) with ScopePublic but
// no scope filtering — the back-compat path with no behavioral
// change for callers who haven't declared Listeners.
func registerLifecycle(lc fx.Lifecycle, app *App, cfg Config) {
	listeners := resolveListeners(app.listeners, cfg.Addr)
	servers := make([]*http.Server, 0, len(listeners))
	for range listeners {
		servers = append(servers, &http.Server{Handler: app})
	}
	// Filtering is opt-in: a single-listener back-compat run skips
	// scope checks entirely so dashboard, REST, GraphQL all stay
	// reachable on the one listener as before.
	scopeFilterOn := len(app.listeners) > 0

	// Peer prober runs in the background while the app is alive, polling
	// every declared peer's /__nexus/health to keep readiness honest in
	// split deployments. Started in OnStart, stopped in OnStop via
	// proberCancel — captured here so the closure can refer to a stable
	// cancel func even when the prober isn't started (no peers).
	var proberCancel context.CancelFunc

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			for i, l := range listeners {
				ln, err := net.Listen("tcp", l.Addr)
				if err != nil {
					// Close any listeners that bound earlier in this
					// loop so a partial start doesn't leak ports.
					for j := 0; j < i; j++ {
						_ = servers[j].Close()
					}
					return fmt.Errorf("nexus: listen %s (%s): %w", l.name, l.Addr, err)
				}
				servers[i].Addr = ln.Addr().String()
				if scopeFilterOn {
					app.listenerScopes.set(ln.Addr().String(), l.Scope)
				}
				if !strings.HasSuffix(servers[i].Addr, ":0") {
					if scopeFilterOn {
						fmt.Fprintf(os.Stdout, "nexus: listening on %s (%s, %s)\n", servers[i].Addr, l.name, l.Scope)
					} else {
						fmt.Fprintf(os.Stdout, "nexus: listening on %s\n", servers[i].Addr)
					}
				}
				srv := servers[i]
				go func() { _ = srv.Serve(ln) }()
			}
			app.cronSched.Start()
			// Liveness flips after the listeners are up — premature true
			// would let an LB route traffic before Serve actually accepts.
			app.health.setAlive(true)
			// Spawn the peer prober only when there's something to probe
			// (declared peers other than self). Skip in monolith.
			if hasRemotePeers(app.topology, app.deployment) {
				proberCtx, cancel := context.WithCancel(context.Background())
				proberCancel = cancel
				prober := newPeerProber(app.topology, app.deployment, app.health)
				go prober.run(proberCtx)
			}
			return nil
		},
		OnStop: func(ctx context.Context) error {
			// Flip alive false BEFORE shutting servers down so an LB
			// pulling readiness during drain sees not-ready and stops
			// sending new traffic.
			app.health.setAlive(false)
			if proberCancel != nil {
				proberCancel()
			}
			app.cronSched.Stop()
			var firstErr error
			for _, s := range servers {
				if err := s.Shutdown(ctx); err != nil && firstErr == nil {
					firstErr = err
				}
			}
			return firstErr
		},
	})
}

// hasRemotePeers reports whether topology declares any peer other than
// the active deployment. Drives the prober's start gate — a monolith
// (no Topology, or Topology with only self) has nothing to probe.
func hasRemotePeers(topology Topology, deployment string) bool {
	for tag, peer := range topology.Peers {
		if tag == deployment {
			continue
		}
		if peer.URL != "" {
			return true
		}
	}
	return false
}

// resolvedListener is one bound listener with its name and scope ready
// for the lifecycle loop. Names land in startup logs and error
// messages so operators can map a bind failure back to the manifest
// entry instantly.
type resolvedListener struct {
	name  string
	Addr  string
	Scope ListenerScope
}

// resolveListeners flattens the listener config into a deterministic
// slice. When ls is empty (no Listeners declared) it returns a single
// "default" listener bound to fallbackAddr — the back-compat path that
// keeps existing apps booting on Config.Addr (or :8080 when unset).
//
// Names are sorted for stable startup logs and predictable bind
// ordering across restarts.
func resolveListeners(ls map[string]Listener, fallbackAddr string) []resolvedListener {
	if len(ls) == 0 {
		addr := fallbackAddr
		if addr == "" {
			addr = ":8080"
		}
		return []resolvedListener{{name: "default", Addr: addr, Scope: ScopePublic}}
	}
	names := make([]string, 0, len(ls))
	for n := range ls {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]resolvedListener, 0, len(names))
	for _, n := range names {
		out = append(out, resolvedListener{name: n, Addr: ls[n].Addr, Scope: ls[n].Scope})
	}
	return out
}

// fxBootOptions returns the baseline fx.Option chain nexus.Run assembles:
// supply Config, provide *App, register lifecycle, auto-mount GraphQL.
// Exposed to tests via integration_test.go; users go through nexus.Run.
func fxBootOptions(cfg Config) fx.Option {
	return fx.Options(
		fx.Supply(cfg),
		fx.Provide(newApp),
		fx.Invoke(registerLifecycle),
		fx.Invoke(autoMountGraphQL),
	)
}
