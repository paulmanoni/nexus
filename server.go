package nexus

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/dev"
	"github.com/paulmanoni/nexus/v2/di"
	"github.com/paulmanoni/nexus/v2/extension/dashboard"
	"github.com/paulmanoni/nexus/v2/frontend/vitehot"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/internal/gqlhttp"
	"github.com/paulmanoni/nexus/v2/notify"
	"github.com/paulmanoni/nexus/v2/trace/otlp"
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
func idleTimeout(cfg config.Runtime) time.Duration {
	switch {
	case cfg.Server.IdleTimeout > 0:
		return cfg.Server.IdleTimeout
	case cfg.Server.IdleTimeout < 0:
		return 0
	default:
		return DefaultIdleTimeout
	}
}

// DefaultMaxBodyBytes is the request-body cap when the operator sets none.
const DefaultMaxBodyBytes = 32 << 20

// maxBodyBytes resolves the request-body cap: max_body_bytes when set, -1
// to turn the cap off, else DefaultMaxBodyBytes. An endpoint that takes
// larger bodies says so with MaxBody.
func maxBodyBytes(cfg config.Runtime) int64 {
	switch {
	case cfg.Server.MaxBodyBytes > 0:
		return cfg.Server.MaxBodyBytes
	case cfg.Server.MaxBodyBytes < 0:
		return 0
	}
	return DefaultMaxBodyBytes
}

// shutdownTimeout resolves the drain window: explicit config wins, then the
// dev/production default.
func shutdownTimeout(cfg config.Runtime) time.Duration {
	if cfg.Server.ShutdownTimeout > 0 {
		return cfg.Server.ShutdownTimeout
	}
	if dev.Enabled() {
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
func registerLifecycle(lc di.Lifecycle, app *App, cfg config.Runtime) {
	// Trace export starts before the listeners and stops after them, so
	// the last requests' spans are flushed once the servers drained.
	if cfg.Telemetry.OTLPEndpoint != "" && app.bus != nil {
		var exp *otlp.Exporter
		lc.Append(di.Hook{
			OnStart: func(context.Context) error {
				name := cfg.Telemetry.ServiceName
				if name == "" {
					name = cfg.Dashboard.Name
				}
				if name == "" {
					name = defaultDashboardName
				}
				exp = otlp.Start(app.bus, otlp.Config{Endpoint: cfg.Telemetry.OTLPEndpoint, Headers: cfg.Telemetry.OTLPHeaders, ServiceName: name})
				return nil
			},
			OnStop: func(ctx context.Context) error {
				if exp == nil {
					return nil
				}
				if err := exp.Stop(ctx); err != nil {
					app.Logger().Warn("trace export: final flush failed", "error", err)
				}
				return nil
			},
		})
	}
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
			app.autoDumpClientSDK()
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
					if errors.Is(err, syscall.EADDRINUSE) {
						return fmt.Errorf("nexus: port %s is already in use — stop whatever is on it, or change [runtime.server] addr in nexus.toml", l.Addr)
					}
					return fmt.Errorf("nexus: cannot listen on %s (%s listener): %w", l.Addr, l.name, err)
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
			if dev.Enabled() {
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

// autoDumpClientSDK writes the client SDK (runtime + manifest + .d.ts) to
// the mounted handler's OutDir so the frontend's tooling — tsc, the IDE,
// nexus-vite-plugin reading sdk/manifest.json — sees the live API.
//
// Development only, by the rule that decides whether the Vite hot file is
// honoured (vitehot.Enabled): under `nexus dev` (NEXUS_DEV=1) or when the
// app's environment is "development". A production binary writes nothing,
// silently, whatever OutDir holds — it used to write ./web/sdk into its
// working directory on every boot wherever a web/vite.config.ts happened
// to exist. Vendor the files at build time with `nexus client --out`.
//
// The knobs come from the handler, not cfg.Client: frontend.Plugin and
// the dev auto-mount mount a handler without populating cfg.Client. An
// empty OutDir (unset with no frontend detected, or client.Off) skips.
// Failures are logged, never fatal — a permission error on the project
// tree is dev-tool friction, not a reason to refuse traffic. Files that
// didn't change print nothing.
func (a *App) autoDumpClientSDK() {
	if !vitehot.Enabled(dev.Enabled(), a.Environment()) {
		return
	}
	h := a.ClientHandler()
	if h == nil {
		return
	}
	outdir, tsconfig, viteconfig := h.AutoDumpConfig()
	if outdir == "" {
		return
	}
	if err := h.Dump(outdir, tsconfig, viteconfig, log.Writer()); err != nil {
		log.Printf("nexus client: auto-dump %s: %v", outdir, err)
	}
}

// resolvedListener is one bound listener with its name and scope ready
// for the lifecycle loop. Names land in startup logs and error
// messages so operators can map a bind failure back to the manifest
// entry instantly.
type resolvedListener struct {
	name  string
	Addr  string
	Scope config.ListenerScope
	TLS   *tls.Config
}

// resolveListeners flattens the listener config into a deterministic
// slice. When ls is empty (no Listeners declared) it returns a single
// "default" listener bound to fallbackAddr — the back-compat path that
// keeps existing apps booting on Config.Addr (or :8080 when unset).
//
// Names are sorted for stable startup logs and predictable bind
// ordering across restarts.
func resolveListeners(ls map[string]config.Listener, fallbackAddr string) []resolvedListener {
	if len(ls) == 0 {
		addr := fallbackAddr
		if addr == "" {
			addr = ":8080"
		}
		return []resolvedListener{{name: "default", Addr: addr, Scope: config.ScopePublic}}
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

// fxEarlyOptions runs BEFORE user options in nexus.Run.
// Supplies Config, provides *App and the framework primitives.
func fxEarlyOptions(cfg config.Runtime) di.Option {
	return di.Options(
		di.Supply(cfg),
		di.Provide(New),
		// *Notifier is a framework primitive used by registry /
		// cron / rate-limit (and any user code that wants
		// cross-subsystem fan-out). Always provide it so users
		// don't have to wire it explicitly; constructor is
		// trivially cheap and the value is unused if no one
		// depends on it.
		di.Provide(notify.New),
		// *Document is the frontend page shell (nexus.Frontend), for
		// middleware and renderers that need it.
		di.Provide(provideDocument),
		// *slog.Logger is the app's logger (App.Logger), replaceable with
		// WithLogger; installLogger resolves that choice before any user
		// invoke runs. Apps that want zap's encoder wrap a zap core in an
		// slog.Handler and pass it to WithLogger.
		di.Provide(provideLogger),
		di.Invoke(di.Annotate(installLogger, di.ParamTags("", `optional:"true"`))),
		// Stash any extension-supplied default endpoint gate on the app
		// BEFORE the per-endpoint invokes run, so deny-by-default applies
		// uniformly regardless of where the supplying extension sits in
		// the option list.
		di.Invoke(di.Annotate(applyDefaultGate, di.ParamTags("", `optional:"true"`))),
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
		// App-wide middleware (nexus.Middleware) goes on in stage order
		// once every user option has declared its own.
		di.Invoke(func(a *App) { a.installAppMiddleware() }),
		di.Invoke(func(a *App) { a.installAutoCSRF() }),
		// The listeners' lifecycle hook goes last, so it starts after every
		// resource and worker the options registered — setup tasks then see
		// connected resources before the first request — and stops first,
		// so traffic drains before those resources close.
		di.Invoke(registerLifecycle),
	)
}

// ServerTLSConfig builds a *tls.Config for a server-terminating
// Listener. certFile and keyFile are required (PEM-encoded server
// cert + key). caFile is optional: when set, the listener requires
// clients to present a certificate signed by that CA (mTLS), and
// 1.3 handshakes for clients without one will fail at the TLS layer
// before any HTTP request is dispatched. Pass "" to skip client auth.
//
// Defaults: TLS 1.2 minimum (1.0/1.1 are deprecated and unsafe);
// modern cipher suite selection left to Go's defaults, which track
// the IETF recommended list.
//
//	cfg, err := nexus.ServerTLSConfig("admin.crt", "admin.key", "admin-ca.crt")
//	if err != nil { log.Fatal(err) }
//	listener := config.Listener{Addr: "10.0.0.5:9443", Scope: config.ScopeAdmin, TLS: cfg}
func ServerTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("nexus: ServerTLSConfig: certFile and keyFile are required")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("nexus: ServerTLSConfig: load keypair: %w", err)
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}
	if caFile != "" {
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("nexus: ServerTLSConfig: read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("nexus: ServerTLSConfig: %q contains no valid PEM certificates", caFile)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// listenerScopes is the runtime lookup table used by the scope filter
// middleware: port string → scope. Populated by registerLifecycle as
// each net.Listener actually binds (so :0 → random port resolves
// correctly), read on every request.
//
// Keyed by port rather than full address because a dual-stack
// listener bound on `[::]:8080` accepts IPv4 connections that arrive
// with a LocalAddr of `127.0.0.1:8080` — the host parts diverge while
// the port stays stable. Single process / single bind per port is the
// realistic invariant; if you ever need different scopes for the same
// port on different hosts, that's a different feature than this.
type listenerScopes struct {
	mu sync.RWMutex
	m  map[string]config.ListenerScope
}

func newListenerScopes() *listenerScopes {
	return &listenerScopes{m: map[string]config.ListenerScope{}}
}

// addrPort extracts the port from "host:port", "[::]:port",
// "127.0.0.1:port", etc. Falls back to the input verbatim when the
// address has no colon — defensive handling for malformed inputs that
// should never reach here in practice.
func addrPort(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	return addr
}

func (l *listenerScopes) set(addr string, scope config.ListenerScope) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m[addrPort(addr)] = scope
}

func (l *listenerScopes) get(addr string) (config.ListenerScope, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	s, ok := l.m[addrPort(addr)]
	return s, ok
}

func (l *listenerScopes) empty() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.m) == 0
}

// scopeAllowsPath decides whether the given path is exposed on the
// given scope. /__nexus/health and /__nexus/ready are exposed on
// every scope — k8s liveness/readiness probes hit the service's
// public port, and the framework's own peerProber (health.go) probes
// peers through their declared public URL. The rest of /__nexus/* is
// held back from public + internal scopes and only served on admin.
//
// ScopeAdmin allows everything — see ScopeAdmin's doc comment for
// the rationale (operator ergonomics + dashboard testers).
func scopeAllowsPath(scope config.ListenerScope, path string) bool {
	isDash := strings.HasPrefix(path, dashboard.Prefix)
	isHealth := path == dashboard.Prefix+"/health" || path == dashboard.Prefix+"/ready"
	switch scope {
	case config.ScopePublic:
		return !isDash || isHealth
	case config.ScopeInternal:
		return !isDash || isHealth
	case config.ScopeAdmin:
		return true
	}
	return false
}

// offsetAddr returns publicAddr with its port shifted by offset.
// Preserves the host part — `127.0.0.1:8081` + 1000 stays loopback-
// bound on `127.0.0.1:9081`, `:8081` becomes `:9081`.
func offsetAddr(publicAddr string, offset int) (string, error) {
	host, portStr, err := net.SplitHostPort(publicAddr)
	if err != nil {
		return "", fmt.Errorf("offsetAddr: parse %q: %w", publicAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", fmt.Errorf("offsetAddr: non-numeric port in %q", publicAddr)
	}
	return fmt.Sprintf("%s:%d", host, port+offset), nil
}

// fillListenerAddrs walks an explicit Listeners map and fills in any
// empty Addrs from the resolved public address. Lets users declare
// the listener *shape* in main.go (or via the manifest's listeners
// block) without hardcoding ports — the per-deployment port flows
// in via cfg.Addr, so split binaries each bind to their own port
// without per-binary main.go.
//
// Rules:
//   - non-empty Addr: kept verbatim (explicit override wins)
//   - empty Addr + ScopePublic: filled from publicAddr
//   - empty Addr + ScopeAdmin: filled from publicAddr + 1000
//   - empty Addr + ScopeInternal: filled from publicAddr + 2000
//
// Public-listener synthesis: if `in` declares no public-scoped
// listener, one is added automatically at publicAddr. That makes
// the manifest's `port:` and `listeners:` blocks composable —
// `port: 8080` + `listeners: {admin: {scope: admin}}` produces
// public=:8080, admin=:9080 without operators having to repeat
// the public entry in YAML.
//
// The 1000/2000 offsets are framework conventions — operators who
// need different numbers set Addr explicitly. Returns the filled
// map; doesn't mutate the input.
func fillListenerAddrs(in map[string]config.Listener, publicAddr string) map[string]config.Listener {
	if publicAddr == "" {
		publicAddr = ":8080"
	}
	out := make(map[string]config.Listener, len(in)+1)
	hasPublic := false
	for name, l := range in {
		if l.Scope == config.ScopePublic {
			hasPublic = true
		}
		if l.Addr != "" {
			out[name] = l
			continue
		}
		switch l.Scope {
		case config.ScopePublic:
			l.Addr = publicAddr
		case config.ScopeAdmin:
			if a, err := offsetAddr(publicAddr, 1000); err == nil {
				l.Addr = a
			}
		case config.ScopeInternal:
			if a, err := offsetAddr(publicAddr, 2000); err == nil {
				l.Addr = a
			}
		}
		out[name] = l
	}
	if !hasPublic {
		out["public"] = config.Listener{Addr: publicAddr, Scope: config.ScopePublic}
	}
	return out
}

// scopeFilterMiddleware returns a gin middleware that 404s requests
// arriving on a listener whose scope doesn't expose the route. Falls
// through (no filtering) when the scope table is empty — that's the
// back-compat path for users who haven't declared Listeners.
//
// Detection: net/http stores the listener's local Addr on the request
// context under http.LocalAddrContextKey. We stringify it and look up
// the scope. Bound addresses (after net.Listen returns) are what land
// here, so :0 (random port) resolves to the actually-bound port.
func scopeFilterMiddleware(scopes *listenerScopes) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		if scopes == nil || scopes.empty() {
			c.Next()
			return
		}
		addr, _ := c.Request.Context().Value(http.LocalAddrContextKey).(net.Addr)
		if addr == nil {
			c.Next()
			return
		}
		scope, ok := scopes.get(addr.String())
		if !ok {
			c.Next()
			return
		}
		if !scopeAllowsPath(scope, c.Request.URL.Path) {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

// healthState tracks two signals every production deployment needs:
//
//   - liveness ("alive"): toggles true after fx Start completes and
//     false on Stop. Drives /__nexus/health, used by k8s/lb liveness
//     probes — "is the process up at all?"
//
//   - readiness ("ready"): alive AND every declared peer is reachable.
//     Drives /__nexus/ready, used by k8s readiness probes / load
//     balancers to gate traffic — "is this replica ready to serve?"
//
// The peer-readiness check is what makes split deployments honest. A
// monolith with no peers is ready as soon as it's alive; a split unit
// is only ready when its hard dependencies (declared in Topology) are
// also up. That keeps requests from reaching a pod whose downstream
// peer is still booting.
type healthState struct {
	mu    sync.RWMutex
	alive bool
	peers map[string]peerHealth // peer tag → last probe result
}

// peerHealth is the per-peer record updated by the prober.
type peerHealth struct {
	Ready      bool      `json:"ready"`
	LastError  string    `json:"lastError,omitempty"`
	LastProbed time.Time `json:"lastProbed,omitempty"`
}

func newHealthState() *healthState {
	return &healthState{peers: map[string]peerHealth{}}
}

func (h *healthState) setAlive(v bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.alive = v
}

func (h *healthState) isAlive() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.alive
}

// snapshot returns the current liveness flag and a copy of the
// per-peer table. Callers can render the JSON without holding the
// lock through the response write.
func (h *healthState) snapshot() (alive bool, peers map[string]peerHealth) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]peerHealth, len(h.peers))
	for k, v := range h.peers {
		out[k] = v
	}
	return h.alive, out
}

func (h *healthState) recordPeer(tag string, ready bool, errStr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.peers[tag] = peerHealth{Ready: ready, LastError: errStr, LastProbed: time.Now()}
}

// ReportPeerHealth records one peer's probe outcome for the
// /__nexus/ready readiness gate. extension/peer's prober calls this
// on every probe round; anything else that tracks a hard downstream
// dependency may too. A peer reported not-ready flips /__nexus/ready
// to 503 until a later report clears it.
func (a *App) ReportPeerHealth(tag string, ready bool, lastErr string) {
	a.health.recordPeer(tag, ready, lastErr)
}

// mountHealth registers /__nexus/health and /__nexus/ready on the
// engine. Called from New() so the endpoints exist even when
// EnableDashboard is false — they're a framework contract, not a
// dashboard feature. The scope filter (listeners.go) treats this
// pair specially: ScopeInternal exposes them while hiding the rest
// of /__nexus.
//
// /__nexus/health: 200 when alive, 503 otherwise. No body — the
// status code is the contract; orchestrators read it directly.
//
// /__nexus/ready: 200 when alive AND every tracked peer is ready,
// 503 otherwise. JSON body lists per-peer state for human debugging
// — invaluable when "why isn't this pod ready?" is the question.
func mountHealth(e httpx.Router, h *healthState) {
	e.GET(dashboard.Prefix+"/health", func(c *httpx.Ctx) {
		if !h.isAlive() {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		c.Status(http.StatusOK)
	})
	e.GET(dashboard.Prefix+"/ready", func(c *httpx.Ctx) {
		alive, peers := h.snapshot()
		ready := alive
		for _, p := range peers {
			if !p.Ready {
				ready = false
				break
			}
		}
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		c.JSON(status, httpx.H{
			"alive": alive,
			"ready": ready,
			"peers": peers,
		})
	})
}

// devModeBypass reports whether the framework is running under
// `nexus dev` (or a caller that explicitly set NEXUS_DEV=1). When
// true, the introspection gate is fully open — dashboards always
// work in dev. Production binaries never see NEXUS_DEV=1, so the
// strict-by-default stance is preserved where it matters.
func devModeBypass() bool {
	return os.Getenv(dev.Env) == "1"
}

// parseIntrospectionNetworks compiles each CIDR string into a
// *net.IPNet for O(1) membership checks per request. Invalid CIDRs
// fail fast with a wrapped error so the operator sees the problem
// at boot rather than at the first dashboard request.
//
// Empty input returns (nil, nil) — the gate logic treats nil as
// "no allowlist", meaning Introspection alone decides access.
func parseIntrospectionNetworks(cidrs []string) ([]*net.IPNet, error) {
	if len(cidrs) == 0 {
		return nil, nil
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("nexus: IntrospectionNetworks[%q]: %w", cidr, err)
		}
		out = append(out, network)
	}
	return out, nil
}

// introspectionAllowed reports whether the request should bypass
// the Introspection gate. introspect=true short-circuits true (the
// flag opens everything globally); otherwise the request's TCP
// peer (RemoteIP — unspoofable; X-Forwarded-For ignored) is matched
// against the pre-parsed networks.
//
// Empty networks + introspect=false = strict mode: every gated
// route 404s.
func introspectionAllowed(c *httpx.Ctx, introspect bool, networks []*net.IPNet) bool {
	if introspect {
		return true
	}
	if len(networks) == 0 {
		return false
	}
	// RemoteIP is the actual TCP peer. ClientIP would honor
	// X-Forwarded-For if Gin's TrustedProxies is configured, which
	// is spoofable by default — wrong default for a security gate.
	ip := net.ParseIP(c.RemoteIP())
	if ip == nil {
		return false
	}
	for _, n := range networks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// introspectionGate returns a httpx.HandlerFunc that 404s requests
// when the Introspection gate is closed AND the peer IP is not in
// the allowlist. 404 (rather than 401/403) is intentional — it
// makes the gated routes look indistinguishable from "never
// mounted" to anonymous scanners, which removes a useful signal
// from probe traffic.
//
// Returns nil when the gate is fully open (Introspection: true) —
// the caller skips installing the middleware in that case so the
// hot path stays empty for dev/internal deploys that don't need
// gating.
func introspectionGate(introspect bool, networks []*net.IPNet) httpx.HandlerFunc {
	if introspect {
		return nil
	}
	// `nexus dev` sets NEXUS_DEV=1 on its child subprocess. Lifting the
	// gate in that case removes the most common dev footgun: boot the
	// app via the CLI, the banner says "ready, opening browser", browser
	// hits /__nexus/, gets 404 because no IntrospectionNetworks was set.
	// Production binaries never see NEXUS_DEV=1.
	if devModeBypass() {
		return nil
	}
	return func(c *httpx.Ctx) {
		if introspectionAllowed(c, false, networks) {
			c.Next()
			return
		}
		// AbortWithStatus over c.JSON: 404 carries no body so the
		// response is byte-equivalent to a missing route.
		c.AbortWithStatus(http.StatusNotFound)
	}
}

// SetGraphStatus overrides the HTTP status code for the current
// GraphQL request. Call from a gql.Middleware (the Graph
// realization of a middleware.Middleware bundle) or from a
// resolver to translate a decision into a non-200 response code:
//
//	authMw := middleware.Middleware{
//	    Name: "auth",
//	    Graph: func(next gql.Resolver) gql.Resolver {
//	        return func(f gql.Field) (any, error) {
//	            if !authed(f.Context) {
//	                nexus.SetGraphStatus(f.Context, http.StatusUnauthorized)
//	                return nil, errors.New("unauthorized")
//	            }
//	            return next(f)
//	        }
//	    },
//	}
//
// Without this call the framework returns 200 OK with errors in the
// GraphQL response body — the GraphQL-spec default. When called
// multiple times within one request, the LAST value wins.
//
// No-op when ctx didn't pass through the framework's GraphQL
// adapter — useful for resolver code under test with a bare
// engine call.
//
// The GraphQL transport is internal; this is its public status hook.
func SetGraphStatus(ctx context.Context, code int) {
	gqlhttp.SetStatusCode(ctx, code)
}
