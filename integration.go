package nexus

import (
	"context"
	"net"
	"net/http"

	"go.uber.org/fx"

	"github.com/paulmanoni/nexus/metrics"
	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/ratelimit"
)

// newApp is the fx provider for *App consumed by everything in the graph.
// Private because nexus.Run is the public entry point; direct fx users
// would previously have called fxmod.NewApp.
func newApp(cfg Config) *App {
	var opts []AppOption
	if cfg.TraceCapacity > 0 {
		opts = append(opts, WithTracing(cfg.TraceCapacity))
	}
	if cfg.EnableDashboard {
		opts = append(opts, WithDashboard())
	}
	if cfg.DashboardName != "" {
		opts = append(opts, WithDashboardName(cfg.DashboardName))
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
