package nexus

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/paulmanoni/nexus/di"
)

// ratelimitGlobalKey is the store key for the app-wide bucket. Re-declared
// here (alongside ratelimit.GlobalKey) so the integration layer stays
// self-contained — the middleware consults both via this name.
const ratelimitGlobalKey = "_global"

// Shutdown windows applied when Config.Server.ShutdownTimeout is zero.
//
// Production gets a real drain so in-flight requests finish before the process
// exits. Dev gets almost none: `nexus dev` replaces the process on every save,
// nothing in flight is worth preserving, and every millisecond spent here is a
// millisecond of Ctrl-C the developer sits through.
const (
	DefaultShutdownTimeout = 10 * time.Second
	DevShutdownTimeout     = 250 * time.Millisecond
)

// DefaultIdleTimeout caps idle keep-alive connections. Go's own default is
// "fall back to ReadTimeout", and with ReadTimeout unset that means never —
// so an unauthenticated client can park connections until the process runs
// out of file descriptors.
const DefaultIdleTimeout = 120 * time.Second

// idleTimeout resolves the keep-alive window. A negative configured value is
// the explicit "use Go's default" opt-out.
func idleTimeout(cfg Config) time.Duration {
	switch {
	case cfg.Server.IdleTimeout > 0:
		return cfg.Server.IdleTimeout
	case cfg.Server.IdleTimeout < 0:
		return 0
	default:
		return DefaultIdleTimeout
	}
}

// maxBodyBytes resolves the request-body cap. Off unless the operator sets
// one: nexus can't know whether an app streams large uploads, and silently
// rejecting them at some framework-chosen ceiling would be a worse failure
// than the exhaustion risk it guards against.
func maxBodyBytes(cfg Config) int64 {
	if cfg.Server.MaxBodyBytes > 0 {
		return cfg.Server.MaxBodyBytes
	}
	return 0
}

// shutdownTimeout resolves the drain window: explicit config wins, then the
// dev/production default.
func shutdownTimeout(cfg Config) time.Duration {
	if cfg.Server.ShutdownTimeout > 0 {
		return cfg.Server.ShutdownTimeout
	}
	if IsDev() {
		return DevShutdownTimeout
	}
	return DefaultShutdownTimeout
}

// registerLifecycle binds the configured HTTP listeners and cron
// scheduler to fx's start/stop hooks. Bind happens synchronously so
// port conflicts abort di.Start() with a clean error.
//
// When app.listeners is non-empty, every entry binds and registers
// its bound address with the scope-filter table. Otherwise a single
// listener binds to cfg.Addr (or :8080 default) with ScopePublic but
// no scope filtering — the back-compat path with no behavioral
// change for callers who haven't declared Listeners.
func registerLifecycle(lc di.Lifecycle, app *App, cfg Config) {
	listeners := resolveListeners(app.listeners, cfg.Server.Addr)
	// Every in-flight request's context descends from reqCtx via BaseContext,
	// so cancelReqs unblocks handlers that select on their context — an SSE
	// stream, a long poll, a slow query with a cancellable driver. Without it
	// Shutdown has no way to ask a handler to stop and can only wait it out,
	// which is what pinned shutdown at the full grace window.
	reqCtx, cancelReqs := context.WithCancel(context.Background())
	servers := make([]*http.Server, 0, len(listeners))
	for range listeners {
		// ReadHeaderTimeout caps how long a client can take to send the
		// request line + headers; without it a slowloris-style attacker
		// can hold a connection open indefinitely with a trickle of
		// bytes, exhausting the listener's accept queue. 10s is wider
		// than any reasonable real-world header upload and tight enough
		// to make the attack uneconomic.
		servers = append(servers, &http.Server{
			Handler:           app,
			ReadHeaderTimeout: 10 * time.Second,
			// Without IdleTimeout Go falls back to ReadTimeout, which is
			// unset — so idle keep-alive connections are held forever and a
			// few thousand cheap connections exhaust the process's file
			// descriptors. Read/Write timeouts stay off by default: they'd
			// cut SSE streams and large uploads, and the framework can't
			// know which of those an app serves.
			IdleTimeout:    idleTimeout(cfg),
			ReadTimeout:    cfg.Server.ReadTimeout,
			WriteTimeout:   cfg.Server.WriteTimeout,
			MaxHeaderBytes: cfg.Server.MaxHeaderBytes,
			BaseContext:    func(net.Listener) context.Context { return reqCtx },
		})
	}
	// Filtering is opt-in: a single-listener back-compat run skips
	// scope checks entirely so dashboard, REST, GraphQL all stay
	// reachable on the one listener as before.
	scopeFilterOn := len(app.listeners) > 0

	lc.Append(di.Hook{
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
			// di.Invokes have populated the registry, so the
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
			// No-listener mode: the app is driven as an http.Handler
			// (InProcess / nexustest / embedding). Skip bind + Serve +
			// banner entirely, but keep cron and liveness so scheduled
			// work and lifecycle teardown behave exactly as in a bound
			// run. The http.Server values stay un-Served; OnStop's
			// Shutdown on them is a safe no-op.
			if cfg.Server.NoListener {
				app.cronSched.Start()
				app.health.setAlive(true)
				return nil
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
				// Per-listener TLS: wrap the raw TCP listener so the
				// http.Server speaks HTTPS on this port without a
				// separate ServeTLS path. r.TLS is populated for
				// downstream handlers via Go's standard TLS conn
				// state — scheme detection (extension/openapi) and
				// the scope filter both keep working unchanged.
				scheme := "http"
				if l.TLS != nil {
					ln = tls.NewListener(ln, l.TLS)
					scheme = "https"
				}
				servers[i].Addr = ln.Addr().String()
				if scopeFilterOn {
					app.listenerScopes.set(ln.Addr().String(), l.Scope)
				}
				if !strings.HasSuffix(servers[i].Addr, ":0") {
					if scopeFilterOn {
						fmt.Fprintf(os.Stdout, "nexus: listening on %s://%s (%s, %s)\n", scheme, servers[i].Addr, l.name, l.Scope)
					} else {
						fmt.Fprintf(os.Stdout, "nexus: listening on %s://%s\n", scheme, servers[i].Addr)
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
			defer cancelReqs()

			// Bound the drain independently of the caller's ctx: the
			// lifecycle deadline covers every hook, and spending all of it
			// here would starve the resource Close hooks that run next.
			drain := shutdownTimeout(cfg)
			// In dev there is nothing worth draining — an unfinished request
			// belongs to a process that's about to be replaced — so cut the
			// handlers loose immediately and let Shutdown collect them.
			if IsDev() {
				cancelReqs()
			}
			shutCtx, cancel := context.WithTimeout(ctx, drain)
			defer cancel()

			var firstErr error
			for _, s := range servers {
				if err := s.Shutdown(shutCtx); err != nil {
					// The window closed with requests still running.
					// Cancel their contexts, then Close to drop whatever
					// still won't budge — Shutdown alone leaves those
					// connections open and the listener goroutines alive.
					cancelReqs()
					_ = s.Close()
					if firstErr == nil {
						firstErr = err
					}
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
	TLS   *tls.Config
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
		out = append(out, resolvedListener{name: n, Addr: ls[n].Addr, Scope: ls[n].Scope, TLS: ls[n].TLS})
	}
	return out
}

// fxBootOptions returns the complete baseline di.Option chain.
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
func fxBootOptions(cfg Config) di.Option {
	return di.Options(
		fxEarlyOptions(cfg),
		fxLateOptions(),
	)
}

// fxEarlyOptions runs BEFORE user options in nexus.Run.
// Supplies Config, provides *App, registers lifecycle.
func fxEarlyOptions(cfg Config) di.Option {
	return di.Options(
		di.Supply(cfg),
		di.Provide(New),
		// *Notifier is a framework primitive used by registry /
		// cron / rate-limit (and any user code that wants
		// cross-subsystem fan-out). Always provide it so users
		// don't have to wire it explicitly; constructor is
		// trivially cheap and the value is unused if no one
		// depends on it.
		di.Provide(NewNotifier),
		// Stash any extension-supplied default endpoint gate on the app
		// BEFORE the per-endpoint invokes run, so deny-by-default applies
		// uniformly regardless of where the supplying extension sits in
		// the option list.
		di.Invoke(di.Annotate(applyDefaultGate, di.ParamTags("", `optional:"true"`))),
		di.Invoke(registerLifecycle),
	)
}

// fxLateOptions runs AFTER user options in nexus.Run. Invokes
// here observe a fully-populated graph and an engine with every
// user middleware already installed via engine.Use(...) — so
// auto-mounted GraphQL routes pick up auth, request-id, CORS,
// and any other middleware that user opts declared earlier.
func fxLateOptions() di.Option {
	return di.Options(
		di.Invoke(di.Annotate(autoMountGraphQL, di.ParamTags("", "", `group:"nexus.graph.fields"`))),
		// Dev-only: mount the client SDK manifest when the app didn't,
		// so `nexus dev` can read it to auto-sync the vite proxy's
		// module prefixes. Runs after user opts → explicit mounts win.
		di.Invoke(devAutoMountClientSDK),
	)
}
