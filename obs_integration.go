package nexus

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"

	"go.uber.org/fx"
)

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
	listeners := resolveListeners(app.listeners, cfg.Server.Addr)
	servers := make([]*http.Server, 0, len(listeners))
	for range listeners {
		servers = append(servers, &http.Server{Handler: app})
	}
	// Filtering is opt-in: a single-listener back-compat run skips
	// scope checks entirely so dashboard, REST, GraphQL all stay
	// reachable on the one listener as before.
	scopeFilterOn := len(app.listeners) > 0

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			// Startup tasks run BEFORE listener bind. Migrations are
			// the canonical case: a partially-bound app accepting
			// traffic against an unmigrated DB is worse than a clean
			// boot failure. Tasks fire in registration order; the
			// first error halts boot with the task name surfaced so
			// the operator (and the orchestration platform's logs)
			// see WHICH task failed rather than a bare error from
			// somewhere deeper.
			//
			// Print mode never reaches OnStart (Run short-circuits in
			// options.go), so a print-mode invocation observes
			// declared StartupTasks via the manifest without ever
			// running them — exactly what the orchestration platform
			// needs to plan migrations as a separate phase.
			if err := app.runStartupTasks(ctx); err != nil {
				return err
			}
			// Resolve the effective manifest for this environment:
			// merge per-env overrides into the declared base, then
			// validate required env vars / secrets are present and
			// satisfy their Validation rules. Fail-fast before
			// listeners bind so a misconfigured binary never serves
			// requests.
			//
			// Runs AFTER runStartupTasks because some apps populate
			// env vars in startup tasks (e.g. fetching a runtime
			// secret) and BEFORE the listener bind so probes never
			// see a half-resolved process.
			if err := app.resolveEffectiveManifest(); err != nil {
				return err
			}
			// SDK auto-dump: fires AFTER all AsRest/AsQuery/AsWS
			// fx.Invokes have populated the registry, so the
			// generated .d.ts + manifest reflect every endpoint.
			// Reads the dump knobs from the handler itself rather
			// than cfg.Client — frontend.Plugin mounts the handler
			// without populating cfg.Client, so the legacy path
			// would silently skip the dump after migration.
			// Failures don't crash the app — a permission error on
			// the project tree is dev-tool friction, not a reason
			// to refuse to serve traffic.
			if h := app.ClientHandler(); h != nil {
				if outdir, tsconfig, viteconfig := h.AutoDumpConfig(); outdir != "" {
					if err := h.Dump(outdir, tsconfig, viteconfig, log.Writer()); err != nil {
						log.Printf("nexus client: auto-dump %s: %v", outdir, err)
					}
				}
			}
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
			return nil
		},
		OnStop: func(ctx context.Context) error {
			// Flip alive false BEFORE shutting servers down so an LB
			// pulling readiness during drain sees not-ready and stops
			// sending new traffic.
			app.health.setAlive(false)
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

// fxBootOptions returns the complete baseline fx.Option chain.
// Used by tests directly via integration_test.go (one entry,
// everything mounts).
//
// nexus.Run does NOT use this — see fxEarlyOptions / fxLateOptions
// below. The Run path needs autoMountGraphQL to fire AFTER user
// options so engine middleware they install (notably
// auth.Module's ginAuthMiddleware) is in place before GraphQL
// routes are registered. Gin captures middleware at route-
// registration time; routes registered before a Use() call don't
// pick up that middleware afterwards.
func fxBootOptions(cfg Config) fx.Option {
	return fx.Options(
		fxEarlyOptions(cfg),
		fxLateOptions(),
	)
}

// fxEarlyOptions runs BEFORE user options in nexus.Run.
// Supplies Config, provides *App, registers lifecycle.
func fxEarlyOptions(cfg Config) fx.Option {
	return fx.Options(
		fx.Supply(cfg),
		fx.Provide(New),
		// *Notifier is a framework primitive used by registry /
		// cron / rate-limit (and any user code that wants
		// cross-subsystem fan-out). Always provide it so users
		// don't have to wire it explicitly; constructor is
		// trivially cheap and the value is unused if no one
		// depends on it.
		fx.Provide(NewNotifier),
		fx.Invoke(registerLifecycle),
	)
}

// fxLateOptions runs AFTER user options in nexus.Run. Invokes
// here observe a fully-populated graph and an engine with every
// user middleware already installed via engine.Use(...) — so
// auto-mounted GraphQL routes pick up auth, request-id, CORS,
// and any other middleware that user opts declared earlier.
func fxLateOptions() fx.Option {
	return fx.Options(
		fx.Invoke(autoMountGraphQL),
	)
}
