package nexus

import (
	"time"

	"github.com/paulmanoni/nexus/client"
	"github.com/paulmanoni/nexus/extension/metrics"
	"github.com/paulmanoni/nexus/extension/ratelimit"
	"github.com/paulmanoni/nexus/httpx"
	"github.com/paulmanoni/nexus/middleware"
)

// Config drives how nexus.Run builds the app. Supply it as the first
// argument to nexus.Run; users never construct a *App directly when using
// the top-level builder.
type Config struct {
	// Server bundles every network-binding knob: the single-listener
	// fallback Addr, and the explicit Listeners map for multi-scope
	// deployments. Both fields are optional; the framework supplies a
	// :8080 default when both are empty.
	//
	//	nexus.Config{
	//	    Server: nexus.ServerConfig{
	//	        Listeners: map[string]nexus.Listener{
	//	            "public": {Addr: ":8080"},
	//	            "admin":  {Addr: "127.0.0.1:7000", Scope: nexus.ScopeAdmin},
	//	        },
	//	    },
	//	}
	Server ServerConfig

	// WebSocket governs the upgrade origin policy for every transport that
	// speaks WebSocket — AsWS endpoints, GraphQL subscriptions, and the
	// dashboard streams. The default is same-origin; see WebSocketConfig.
	WebSocket WebSocketConfig

	// Router selects the HTTP router backend. Nil means the default
	// stdlib net/http.ServeMux (zero third-party deps). Set it to an
	// opt-in adapter — ginrouter.New() / chirouter.New() — or use the
	// nexus.WithRouter(...) option, which sets this field. Not decoded
	// from nexus.toml (an interface value); code-only.
	Router httpx.Router `toml:"-"`

	// Dashboard bundles the /__nexus surface knobs (whether it
	// mounts at all, the brand label). Middleware that gates the
	// dashboard lives under Middleware.Dashboard so all middleware
	// configuration stays in one place.
	//
	//	nexus.Config{
	//	    Dashboard: nexus.DashboardConfig{Enabled: true, Name: "MyApp"},
	//	}
	Dashboard DashboardConfig

	// Client bundles the auto-generated JS/TS client SDK knobs —
	// whether the SDK routes mount at all, what URL prefix they
	// land under, and any middleware that gates them. Mirrors the
	// Dashboard knob; defaults to disabled so apps that don't ship
	// an SDK pay no embed cost.
	//
	// When Enabled, four routes appear under cfg.Client.Path
	// (default "/__nexus/client"):
	//
	//	GET <path>/manifest.json    SDK-tailored, public
	//	GET <path>/client.js        runtime ESM
	//	GET <path>/client.d.ts      generated TS types
	//	GET <path>/vue.js           Vue 3 composables
	//
	// For per-deployment gating (ship the SDK only from the
	// public-facing service), use nexus.IfDeployment([...],
	// nexus.ClientUse(...)) instead of setting Client.Enabled at
	// the Config level — IfDeployment composes with the option
	// chain while Config is a static value across the binary.
	Client client.Config

	// SDK is the one-switch front door to the typed client SDK —
	// PocketBase-style. Set it (Config.SDK or `[runtime] sdk = true`
	// in nexus.toml) and nexus generates + serves the full typed
	// client covering REST, GraphQL, and WebSocket — and, when a
	// frontend dir is present, dumps the SDK files + wires tsconfig so
	// `import 'nexus-client'` resolves with types. No client.Config
	// ceremony, no manifest wiring.
	//
	// Independent of Introspection. The SDK is what the app's own
	// browser bundle imports, so tying it to the dashboard switch would
	// break the frontend of every production build that (correctly)
	// locks /__nexus down. Introspection governs the dashboard; this
	// flag governs the client; both are explicit opt-ins and neither
	// implies the other.
	//
	// Security: the routes are public — an anonymous browser has to be
	// able to fetch client.js — and the manifest they serve is a full
	// map of your API surface (paths, methods, argument and response
	// shapes). It exposes no data and no route that wasn't already
	// listening, but it does hand a reader the map. Set this only when
	// you intend to serve the SDK from the binary; to ship a frontend
	// without publishing the map, vendor the files at build time
	// (`nexus client --out`) and leave the flag off.
	//
	// The file dump additionally requires a detected frontend dir, so
	// production binaries serve the routes without writing to a project
	// tree that isn't there.
	//
	// Equivalent to enabling Client with the full manifest + frontend
	// defaults; for finer control (custom Path, middleware gating,
	// explicit OutDir) set Client directly instead.
	SDK bool

	// TraceCapacity is the ring-buffer size for request traces. 0 disables
	// tracing — the Traces tab will stay empty.
	TraceCapacity int

	// DevReload tunes the dev-mode live-reload file watcher, which
	// runs only under NEXUS_DEV=1. Production builds never start the
	// watcher, so this field is inert there.
	DevReload DevReloadConfig

	// GraphQL bundles every environment-level GraphQL knob that
	// applies across all services' mounted schemas. Set once on the
	// app, not per-service.
	//
	//	nexus.Config{
	//	    GraphQL: nexus.GraphQLConfig{
	//	        Path:   "/api/graphql",
	//	        Pretty: true,
	//	    },
	//	}
	GraphQL GraphQLConfig

	// Middleware bundles every middleware-related knob: engine-root
	// stacks, dashboard gating, and the built-in global rate limit.
	//
	//	nexus.Config{
	//	    Middleware: nexus.MiddlewareConfig{
	//	        Global:    []middleware.Middleware{requestID, logger, cors},
	//	        Dashboard: []middleware.Middleware{bearerAuth, requireAdminRole},
	//	        RateLimit: ratelimit.Limit{RPM: 600, Burst: 50},
	//	    },
	//	}
	Middleware MiddlewareConfig

	// Stores groups the framework's pluggable backends for state
	// nexus needs to keep around — rate-limit counters, metrics
	// rings, the general-purpose cache. All optional; the framework
	// supplies sensible defaults (in-memory / cache-backed) when
	// fields are zero. Set explicitly to swap in Redis-backed,
	// Prometheus-backed, or other implementations.
	//
	//	nexus.Config{
	//	    Stores: nexus.StoreConfig{
	//	        RateLimit: ratelimit.NewRedisStore(rdb),
	//	        Cache:     myCacheManager,
	//	    },
	//	}
	Stores StoreConfig

	// Environment is the named target the binary is booting into —
	// "production", "staging", "preview", etc. Distinct from
	// Deployment (which is the topology unit, e.g. "users-svc"):
	// one Deployment can run in many Environments. Drives the
	// per-environment Override merge at boot.
	//
	// Resolution priority:
	//   1. Explicit Config.Environment field
	//   2. NEXUS_ENVIRONMENT env var (set by the orchestration platform)
	//   3. Default "production"
	//
	// Empty string is normalized to "production" at resolveConfig time
	// so downstream code doesn't branch on the empty value.
	Environment string

	// Version stamps the binary's version on /__nexus/config. Used by
	// generated clients to detect peer-version skew across services
	// in a split deployment ("service A is on v2, service B on v1"
	// is the source of most weird microservice bugs). Defaults to
	// "dev" when unset. Stamp via -ldflags at release:
	//
	//    go build -ldflags "-X main.version=$GIT_SHA"
	//    nexus.Config{Version: version}
	Version string

	// Introspection is the master gate over developer-facing
	// surfaces under /__nexus (the dashboard, /__nexus/config,
	// /__nexus/manifest, /__nexus/endpoints, etc.). Default false:
	// requests to these surfaces 404 unless the request bypasses
	// the gate via IntrospectionNetworks.
	//
	// Health + readiness probes (/__nexus/health, /__nexus/ready)
	// stay unconditional — orchestrators need them. The SDK manifest
	// at /__nexus/client/manifest.json has its own gate via
	// Config.Client.Public; Introspection does not affect it.
	//
	// Typical prod: leave false, populate IntrospectionNetworks with
	// VPN / office / loopback CIDRs so operators can still reach the
	// dashboard from trusted networks.
	//
	// Set true on dev/internal listeners (compose with
	// nexus.IfDeployment) to expose introspection unconditionally.
	Introspection bool

	// IntrospectionNetworks is the CIDR allowlist that bypasses the
	// Introspection gate. When the request's TCP peer
	// (gin.Context.RemoteIP — UNSPOOFABLE; ignores X-Forwarded-For)
	// falls within any of these networks, the introspection routes
	// serve as if Introspection were true.
	//
	// CIDRs are parsed once at nexus.New() time; an invalid entry
	// fails fast with a clear error so misconfiguration surfaces
	// at boot, not at the first dashboard request.
	//
	//	nexus.Config{
	//	    Introspection: false,
	//	    IntrospectionNetworks: []string{
	//	        "127.0.0.0/8",     // loopback
	//	        "192.168.1.0/24",  // office LAN
	//	        "10.0.0.0/8",      // VPN
	//	    },
	//	}
	//
	// Behind a load balancer the TCP peer is the LB itself — the
	// allowlist won't recognize the original client. For LB-fronted
	// deploys, prefer a separate internal listener bound to the
	// loopback / VPN interface (Server.Listeners with ScopeAdmin).
	IntrospectionNetworks []string
}

// DashboardConfig groups the /__nexus surface knobs. Both fields
// are optional: leave the struct zero-valued and the dashboard
// stays unmounted (default).
type DashboardConfig struct {
	// Enabled mounts /__nexus/* on the engine when true. Pulls in
	// the Architecture / Endpoints / Crons / Rate-limits / Traces
	// tabs and the JSON API the dashboard reads from.
	Enabled bool

	// Name is the brand shown in the dashboard header and the
	// browser tab title. Defaults to "Nexus" when empty. Served
	// over /__nexus/config so you can change it per-environment
	// without rebuilding the UI.
	Name string
}

// DevReloadConfig tunes the NEXUS_DEV=1 live-reload watcher. The
// watcher already ignores hidden files, sourcemaps, and runtime data
// artifacts (SQLite databases + their -wal/-shm/-journal sidecars,
// .log files) out of the box; Exclude adds app-specific patterns on
// top of those built-ins.
type DevReloadConfig struct {
	// Exclude lists glob patterns whose matches never trigger a
	// browser reload. Each changed file is tested (via filepath.Match)
	// three ways, and a match on any one excludes it:
	//
	//   - against the base name           → "*.tmp", "*.db"
	//   - against the path relative to the
	//     watch root                       → "cache/*.json"
	//   - as a directory subtree prefix    → "uploads" skips
	//     everything under uploads/ (a trailing slash is optional)
	//
	// Invalid patterns are logged once at boot and skipped.
	Exclude []string
}

// ServerConfig groups the network-binding knobs. Addr is the
// single-listener fallback (used when Listeners is empty);
// Listeners declares one or more named listeners with explicit
// scopes. Both optional — leaving both zero binds a single
// listener at :8080 with ScopePublic.
//
// When Listeners is set, Addr is ignored and every declared
// listener binds. The framework installs a scope-filter middleware
// that 404s out-of-scope routes per listener (e.g. requests to
// /__nexus/* on the public listener).
type ServerConfig struct {
	// Addr is the HTTP listen address used in single-listener
	// mode (default ":8080"). Ignored when Listeners is non-empty.
	// Manifest-driven defaults via DeploymentDefaults.Addr fill
	// this when zero, so split binaries each pick up their own
	// per-deployment port.
	Addr string

	// Listeners declares one or more named listeners with explicit
	// scopes. Empty Addrs auto-fill from the resolved Addr above
	// (admin = port+1000, internal = port+2000); explicit Addrs
	// are passed through unchanged.
	Listeners map[string]Listener

	// RoutePrefix is prepended to every user-mounted route — REST
	// endpoints, the GraphQL POST mount, and WebSocket upgrades —
	// so a single binary can be served behind a path-based ingress.
	// Framework routes (/__nexus, /health, /ready) are not prefixed.
	//
	// Typical use: per-deployment routing in a shared-domain setup,
	// e.g. /oats-uaa/* on the uaa-svc binary and /oats-interview/*
	// on the interview-svc binary. Set in source via Config or
	// declaratively via nexus.toml's `prefix:` per deployment;
	// the manifest value lands here through DeploymentDefaults.
	//
	// Leading slash is required; trailing slash is trimmed at apply
	// time so paths concatenate cleanly.
	RoutePrefix string

	// NoListener boots the app without binding any network listener:
	// startup tasks, manifest resolution, the SDK dump, cron, and
	// liveness all run, but no net.Listen / Serve happens and the
	// "listening on …" banner is suppressed. The app is still a fully
	// wired http.Handler (App.ServeHTTP), so requests are driven
	// in-process (httptest) or by embedding it in another server.
	//
	// This is what nexus.InProcess (and the nexustest harness) sets;
	// it's also useful for serverless / embedded deployments where an
	// outer runtime owns the socket. Ignored by nexus.Run only in the
	// sense that Run will then serve no traffic on its own — drive the
	// handler yourself.
	NoListener bool

	// ShutdownTimeout bounds the graceful drain on SIGINT/SIGTERM: how
	// long http.Server.Shutdown waits for in-flight requests before the
	// remaining connections are cut and the process exits. Zero picks
	// DefaultShutdownTimeout (production) or DevShutdownTimeout (dev).
	//
	// It also bounds the whole lifecycle stop chain, so a resource whose
	// Close blocks can't wedge shutdown either.
	//
	// The drain only matters for requests still in flight; nexus cancels
	// their contexts once the window closes, so a handler that selects on
	// its context returns immediately and shutdown finishes early.
	ShutdownTimeout time.Duration

	// IdleTimeout caps how long an idle keep-alive connection is held
	// between requests. Zero picks DefaultIdleTimeout; negative disables
	// it (Go's own default, which is to fall back to ReadTimeout — and
	// with ReadTimeout unset that means "hold forever", so a few thousand
	// cheap connections can exhaust the process's file descriptors).
	IdleTimeout time.Duration

	// ReadTimeout / WriteTimeout bound a whole request read and response
	// write. Both default to OFF, and deliberately so: a WriteTimeout kills
	// server-sent-event streams and long downloads mid-flight, and a
	// ReadTimeout kills large uploads — nexus can't tell which of those an
	// app serves. Set them when you know your traffic shape.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// MaxHeaderBytes caps the request header size. Zero picks Go's 1MB
	// default; set explicitly to tighten it.
	MaxHeaderBytes int

	// MaxBodyBytes caps request bodies; over-limit requests get 413.
	//
	// OFF by default. Every JSON-binding handler is otherwise a
	// memory-exhaustion primitive — an anonymous client can stream until
	// the process dies — so setting this is worth doing. It's opt-in
	// because the framework can't know whether an app serves large
	// uploads, and cutting those off at a value nexus picked would be a
	// worse failure than the risk it guards against.
	//
	//	[runtime.server]
	//	max_body_bytes = 33554432   # 32MB
	MaxBodyBytes int64
}

// WebSocketConfig governs WebSocket upgrades across every transport: user
// AsWS endpoints, GraphQL subscriptions, and the dashboard's own streams.
type WebSocketConfig struct {
	// AllowedOrigins extends the default same-origin upgrade policy.
	//
	// WebSocket handshakes are NOT covered by CORS or the same-origin
	// policy, and they carry cookies — so an upgrader that accepts any
	// Origin lets an attacker's page open a socket as the visiting user
	// (cross-site WebSocket hijacking). nexus therefore defaults to
	// same-origin, and this list is how you allow a legitimately
	// cross-origin frontend:
	//
	//	[runtime.websocket]
	//	allowed_origins = ["https://app.example.com", "*.example.com"]
	//
	// "*" disables the check entirely, restoring the pre-1.39 behavior.
	// Only safe when the socket carries no ambient authority.
	//
	// Loopback origins are always allowed under `nexus dev`, where the
	// frontend (:5173) and the app (:8080) are cross-origin by design.
	AllowedOrigins []string
}

// MiddlewareConfig groups every middleware-related knob the
// framework recognizes. All fields are optional — leave the struct
// zero-valued for "no extra middleware" and the framework runs with
// its built-in stack alone.
type MiddlewareConfig struct {
	// Global stacks on the Gin engine root, so every REST endpoint,
	// GraphQL POST, WebSocket upgrade, and dashboard request flows
	// through it in registration order. Use for cross-cutting
	// concerns (request-id, logger, CORS, auth pre-gate, etc.).
	// Each bundle's Gin field runs; nil Gin realizations are
	// skipped silently. Per-op middleware (via nexus.Use on a
	// registration) layers on top.
	Global []middleware.Middleware

	// Dashboard gates the /__nexus surface behind user-supplied
	// middleware — typically auth + permission checks. Each
	// bundle's Gin realization runs in registration order on the
	// /__nexus route group BEFORE any dashboard handler, covering
	// the JSON API, WebSocket events, and the embedded Vue UI in
	// one pass.
	//
	// Bundles whose Gin field is nil are ignored — the dashboard
	// is an HTTP surface, so graph-only bundles don't apply.
	Dashboard []middleware.Middleware

	// RateLimit is the built-in app-wide rate limit. When set,
	// installs as a gin middleware on the engine root so every
	// HTTP path consults the bucket. Combine with per-op
	// nexus.RateLimit() declarations for layered protection: the
	// request must pass both the global bucket and the op's bucket.
	// Zero disables.
	RateLimit ratelimit.Limit

	// CORS configures the built-in CORS middleware. Nil = no CORS
	// handling (the framework installs nothing — same-origin
	// browsers work, cross-origin requests are rejected by the
	// browser). Set to a populated struct to allow cross-origin
	// requests with the listed origins / methods / headers. The
	// middleware lands on the engine root before any route, so
	// REST + GraphQL + WebSocket upgrades all see it.
	//
	// For finer control (per-route CORS, dynamic origin checks),
	// install your own gin middleware via Global instead.
	CORS *CORSConfig

	// Security configures the built-in web-security middleware:
	// response headers (on by default) and CSRF (off by default).
	// Nil means framework defaults — the three safe headers
	// (X-Frame-Options, X-Content-Type-Options, Referrer-Policy) are
	// still applied. Set a struct to tune headers, enable HSTS/CSP, or
	// turn on CSRF. Populated from [runtime.middleware.security] in
	// nexus.toml. See SecurityConfig.
	//
	// For the dashboard "Security" tab or per-route bundles, load the
	// extension/security plugin — the global enforcement here and that
	// plugin's per-route surface share one implementation.
	Security *SecurityConfig
}

// SecurityConfig declares the framework's built-in security middleware.
// The zero value (and a nil *SecurityConfig) yields the secure default:
// the three safe response headers on, CSRF off.
//
// CSRF is off by default on purpose. A nexus app is usually a
// token-authenticated API (bearer / the typed client SDK), where CSRF
// is not the relevant threat — a browser never auto-attaches a bearer
// token cross-site. Enable it (EnableCSRF, or `csrf = true`) when you
// serve cookie/session-authenticated, server-rendered HTML forms (a
// template engine, or Inertia backed by session cookies).
type SecurityConfig struct {
	// DisableHeaders turns off the security response headers. They are
	// on by default: X-Frame-Options: DENY, X-Content-Type-Options:
	// nosniff, Referrer-Policy: strict-origin-when-cross-origin.
	DisableHeaders bool

	// FrameOptions / ReferrerPolicy override the header defaults. Empty
	// keeps the default; "-" omits that header entirely.
	FrameOptions   string
	ReferrerPolicy string

	// CSP sets Content-Security-Policy verbatim. Empty → not sent (CSP
	// is too app-specific to default).
	CSP string

	// HSTSMaxAge, when > 0, sends Strict-Transport-Security with that
	// max-age in seconds. Opt-in — browsers ignore it over plain http,
	// so it's safe to set once you serve https.
	HSTSMaxAge int

	// EnableCSRF turns on double-submit-cookie CSRF enforcement. See
	// the type doc for why it defaults off.
	EnableCSRF bool

	// CSRFCookieSecure forces the CSRF cookie's Secure flag. Nil → auto
	// (Secure when the request arrived over https). Set false only if a
	// dev setup needs the cookie over plain http.
	CSRFCookieSecure *bool
}

// CORSConfig declares the framework's built-in CORS policy. All
// fields are optional; reasonable defaults fill in for the common
// "allow my SPA's origin to hit my API" case.
type CORSConfig struct {
	// AllowOrigins lists allowed Origin header values verbatim.
	// Use "*" for "any origin" — note that AllowCredentials cannot
	// be true with "*" per the CORS spec; the middleware will
	// downgrade to echoing the request's Origin in that case.
	// Empty defaults to ["*"].
	AllowOrigins []string

	// AllowMethods lists HTTP methods allowed on cross-origin
	// requests. Empty defaults to GET, POST, PUT, PATCH, DELETE,
	// OPTIONS — covers every method nexus handlers register.
	AllowMethods []string

	// AllowHeaders lists request headers the browser is allowed to
	// send. Empty defaults to Origin, Content-Type, Accept,
	// Authorization, X-Requested-With.
	AllowHeaders []string

	// ExposeHeaders lists response headers the browser is allowed
	// to read from JavaScript. Empty omits the header (browser
	// only sees the safelisted response headers).
	ExposeHeaders []string

	// AllowCredentials sets Access-Control-Allow-Credentials: true
	// when an origin matches. Required when the SPA sends cookies
	// or Authorization headers cross-origin.
	AllowCredentials bool

	// MaxAge caches the preflight response for this duration.
	// Zero defaults to 12 hours — enough to amortize the OPTIONS
	// round-trip across a session, conservative enough that policy
	// changes propagate within a workday.
	MaxAge time.Duration
}

// StoreConfig groups the framework's pluggable backends. All fields
// are optional — leave them nil and the framework supplies in-
// memory / cache-backed defaults. Set explicitly to share state
// across replicas, push to a monitoring stack, or hand the
// framework an existing cache tier.
type StoreConfig struct {
	// RateLimit replaces the default in-memory rate-limit store.
	// Set when you want to share the store between the app and
	// externally-built middleware bundles (ratelimit.NewMiddleware
	// consumes a Store), or for persistence / multi-replica via a
	// Redis-backed implementation. Nil → app builds its own
	// MemoryStore (or cache-backed when Cache is set).
	RateLimit ratelimit.Store

	// Metrics replaces the default cache-backed metrics store. Use
	// for Prometheus / StatsD / OTel-backed implementations. The
	// dashboard's /__nexus/stats endpoint reads from whichever
	// Store is installed.
	Metrics metrics.Store

	// Cache is the framework's general-purpose cache.Manager. When
	// set, nexus uses it as the default backing for the metrics +
	// rate-limit stores (so counters and overrides benefit from
	// the app's cache tier). Pass your own when user code already
	// runs a cache.Manager — framework + app share one tier.
	//
	// Explicit RateLimit / Metrics settings still win; Cache is
	// just the default when those are nil.
	//
	// Typed as the root Cache interface so the core stays decoupled
	// from extension/cache; *cache.Manager satisfies it.
	Cache Cache
}

// GraphQLConfig groups the framework's environment-level GraphQL
// knobs. Per-service paths via (*Service).AtGraphQL still win over
// these defaults — these only apply to services that don't carry an
// explicit AtGraphQL call.
type GraphQLConfig struct {
	// Path overrides the default mount path for auto-generated
	// GraphQL services. Empty falls back to DefaultGraphQLPath
	// ("/graphql").
	Path string

	// DisablePlayground turns OFF the in-browser GraphQL IDE served on
	// GET <service>/<path>. The IDE is Apollo Sandbox by default.
	// Enabled by default — flip in prod wiring to hide the interactive
	// console.
	DisablePlayground bool

	// Debug skips query validation + response sanitization in
	// go-graph. Dev-only. Default false.
	Debug bool

	// Pretty pretty-prints JSON responses. Convenient while
	// exploring; ship off in prod.
	Pretty bool

	// DocumentCacheSize bounds the parse+validate memo (LRU) the
	// framework installs in front of graphql.Do. Repeat queries
	// re-use the cached AST and validation verdict, skipping the
	// ~89% of GraphQL request allocations that profiling pinned on
	// parse + validate.
	//
	// Zero (the default) means 1024 entries — enough for any app
	// with a fixed query catalog. Set to a negative value to
	// disable the cache entirely.
	//
	// A "miss every request" pattern usually indicates clients are
	// embedding variable values in the query string instead of
	// using $vars. Check the cache stats on the dashboard if hit
	// rate is suspiciously low.
	DocumentCacheSize int
}
