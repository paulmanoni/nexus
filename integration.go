package nexus

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
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
	// When the user didn't set MetricsStore explicitly, New() already
	// defaults to metrics.NewCacheStore(a.cacheMgr) using whatever cache
	// is active — the user-supplied one or nexus's own. metrics is kept
	// unused here deliberately; import retained for future TODO on
	// ratelimit cache-backing.
	_ = metrics.NewCacheStore
	app := New(opts...)

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

// registerLifecycle binds the HTTP server and cron scheduler to fx's
// start/stop hooks. Bind happens synchronously so port conflicts abort
// fx.Start() with a clean error.
func registerLifecycle(lc fx.Lifecycle, app *App, cfg Config) {
	addr := cfg.Addr
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{Handler: app}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return err
			}
			srv.Addr = ln.Addr().String()
			// One-line startup announcement. Recognizable format that
			// nexus dev's addrFinder parses to surface the actual
			// bind in its banner — and a useful signal for anyone
			// scraping logs in plain `go run` use too. Skipped when
			// the bound port is :0 (test harness) since the
			// fx-managed listener is meaningful but not where users
			// would actually connect.
			if !strings.HasSuffix(srv.Addr, ":0") {
				fmt.Fprintf(os.Stdout, "nexus: listening on %s\n", srv.Addr)
			}
			app.cronSched.Start()
			go func() { _ = srv.Serve(ln) }()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			app.cronSched.Stop()
			return srv.Shutdown(ctx)
		},
	})
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
