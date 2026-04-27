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
		if len(peer.URLs) > 0 {
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
		fx.Provide(New),
		fx.Invoke(registerLifecycle),
		fx.Invoke(autoMountGraphQL),
	)
}
