package nexus

import (
	"context"
	"os"
	"time"

	"github.com/paulmanoni/nexus/cache"
	"github.com/paulmanoni/nexus/metrics"
	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/ratelimit"
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

	// Dashboard bundles the /__nexus surface knobs (whether it
	// mounts at all, the brand label). Middleware that gates the
	// dashboard lives under Middleware.Dashboard so all middleware
	// configuration stays in one place.
	//
	//	nexus.Config{
	//	    Dashboard: nexus.DashboardConfig{Enabled: true, Name: "MyApp"},
	//	}
	Dashboard DashboardConfig

	// TraceCapacity is the ring-buffer size for request traces. 0 disables
	// tracing — the Traces tab will stay empty.
	TraceCapacity int

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

	// Deployment names the deployment unit this binary runs as. Empty
	// = monolith mode (every module is local — current behavior).
	// When set, the framework knows which DeployAs-tagged modules are
	// "local" vs "remote" so future codegen'd clients can pick the
	// in-process or HTTP path accordingly. Today this field is
	// metadata only — surfaced on /__nexus/config so the dashboard
	// can render the active deployment.
	//
	// Convention: pass DeploymentFromEnv() in main() so a single
	// binary can boot as different units across environments without
	// recompiling.
	Deployment string

	// Version stamps the binary's version on /__nexus/config. Used by
	// generated clients to detect peer-version skew across services
	// in a split deployment ("service A is on v2, service B on v1"
	// is the source of most weird microservice bugs). Defaults to
	// "dev" when unset. Stamp via -ldflags at release:
	//
	//    go build -ldflags "-X main.version=$GIT_SHA"
	//    nexus.Config{Version: version}
	Version string

	// Topology declares the peer table for split deployments — one
	// entry per DeployAs tag the binary calls into. Codegen'd clients
	// look up the active peer here at construction time instead of
	// reading hard-coded env vars (USERS_SVC_URL, etc.), so peer URLs,
	// timeouts, auth, and version floors live in one declarative place.
	//
	// Empty is the monolith default — every module is local, no peer
	// lookups happen. When non-empty in split mode, the active
	// Deployment must be a key in Peers (Run fails fast otherwise).
	//
	//	nexus.Config{
	//	    Deployment: "checkout-svc",
	//	    Topology: nexus.Topology{
	//	        Peers: map[string]nexus.Peer{
	//	            "users-svc":    {URL: os.Getenv("USERS_SVC_URL"), Timeout: 2 * time.Second},
	//	            "checkout-svc": {},
	//	        },
	//	    },
	//	}
	Topology Topology
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
	Cache *cache.Manager
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

	// DisablePlayground turns OFF the GraphQL Playground served on
	// GET <service>/<path>. Default is enabled — flip in prod
	// wiring to hide the interactive console.
	DisablePlayground bool

	// Debug skips query validation + response sanitization in
	// go-graph. Dev-only. Default false.
	Debug bool

	// Pretty pretty-prints JSON responses. Convenient while
	// exploring; ship off in prod.
	Pretty bool
}

// Topology is the peer table for cross-module HTTP calls in a split
// deployment. Each entry names a deployment unit (matching a
// DeployAs tag) and binds the transport details for reaching it.
type Topology struct {
	// Peers is keyed by DeployAs tag. The active deployment's own
	// entry is permitted (URL is ignored for the active unit since
	// it's never called over HTTP from itself); a placeholder entry
	// keeps the map a complete inventory of every unit in the
	// deployment, which is convenient for dashboards and validation.
	Peers map[string]Peer
}

// Peer is the per-deployment binding consumed by codegen'd remote
// clients. Every field is optional — zero values map to the
// framework's default behavior for that knob.
type Peer struct {
	// URLs is the replica list for this peer. The runtime round-
	// robins across entries and passively ejects any replica that
	// returns transport errors / 5xx for a cooldown window — single-
	// replica peers just declare a one-element slice.
	//
	//	"users-svc": {URLs: []string{
	//	    "http://users-1.cluster.local:8080",
	//	    "http://users-2.cluster.local:8080",
	//	    "http://users-3.cluster.local:8080",
	//	}},
	//
	// Required when the active deployment is not this peer's tag;
	// ignored for the active peer's own entry.
	URLs []string

	// Timeout caps each remote call. Zero falls back to the
	// RemoteCaller default (30s). Recommended: set to your
	// infrastructure-level timeout minus a small slack so client-side
	// errors fire before any LB resets the connection.
	Timeout time.Duration

	// Auth is invoked once per remote call to produce an
	// Authorization header value (e.g. "Bearer <token>"). Returning
	// an error aborts the call. Nil disables explicit auth — the
	// default forwarding propagator still threads the inbound
	// Authorization header from the request context, so most
	// edge-token flows work without setting this.
	Auth func(ctx context.Context) (string, error)

	// MinVersion is the lowest peer Version (read from the peer's
	// /__nexus/config) accepted on the first call. When non-empty
	// it replaces the local-binary version as the comparison floor
	// in the existing skew-probe path. Empty disables the floor and
	// falls back to comparing against the local binary's Version.
	// Soft-fail: a mismatch logs a single warning line and the call
	// proceeds, same as today's WithLocalVersion behavior.
	MinVersion string

	// Retries caps the number of automatic retries on transport
	// errors (connection reset, timeout, DNS failure). Only
	// idempotent verbs (GET, HEAD, PUT, DELETE, OPTIONS, TRACE)
	// retry — POST and PATCH never do, regardless of this value.
	// Zero disables retries entirely. Backoff between attempts is
	// 50ms * 2^n with full jitter.
	Retries int
}

// DeploymentFromEnv reads NEXUS_DEPLOYMENT. The single-binary, multi-shape
// pattern: the same compiled binary boots as different deployment units
// based on the env var alone — no rebuild, no flags. Returns "" when
// unset, which the framework treats as monolith mode.
//
//	func main() {
//	    nexus.Run(nexus.Config{
//	        Deployment: nexus.DeploymentFromEnv(),
//	        // ...
//	    }, allModules...)
//	}
func DeploymentFromEnv() string { return os.Getenv(nexusDeploymentEnv) }
