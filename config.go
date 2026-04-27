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
	// Addr is the HTTP listen address (default ":8080"). Equivalent to
	// declaring a single ScopePublic listener under that address. Ignored
	// when Listeners is non-empty — the explicit map takes over.
	Addr string

	// AdminPortOffset, when non-zero and Listeners is empty, makes
	// the framework auto-derive a public + admin listener pair:
	// public binds at the manifest port (or Addr / :8080 fallback),
	// admin binds at public + AdminPortOffset. The dashboard's
	// /__nexus/* surface lands on admin only — public 404s it via
	// the scope filter.
	//
	// Useful when you want the standard "REST/GraphQL on the public
	// port, dashboard on a sidecar port" split without typing the
	// listener map by hand. Set to 0 (default) to keep the single-
	// listener behavior.
	//
	// Ignored when Listeners is non-empty — the explicit map wins,
	// no auto-derivation happens.
	AdminPortOffset int

	// Listeners declares one or more named listeners with explicit
	// scopes. Use to split user-facing traffic, peer/health checks,
	// and the admin dashboard onto separate ports (and, via the bound
	// host, separate network exposures):
	//
	//	nexus.Config{
	//	    Listeners: map[string]nexus.Listener{
	//	        "public":   {Addr: ":8080",            Scope: nexus.ScopePublic},
	//	        "internal": {Addr: "127.0.0.1:9000",   Scope: nexus.ScopeInternal},
	//	        "admin":    {Addr: "127.0.0.1:7000",   Scope: nexus.ScopeAdmin},
	//	    },
	//	}
	//
	// When Listeners is set, Config.Addr is ignored and every declared
	// listener binds. The framework installs a scope-filter middleware
	// that 404s out-of-scope routes per listener (e.g. requests to
	// /__nexus/* on the public listener). When Listeners is empty,
	// behavior is unchanged: a single listener bound to Addr serves
	// every route.
	Listeners map[string]Listener

	// DashboardName is the brand shown in the dashboard header and tab title
	// (default "Nexus"). Served over /__nexus/config so you can change it
	// per-environment without rebuilding the UI.
	DashboardName string

	// TraceCapacity is the ring-buffer size for request traces. 0 disables
	// tracing — the Traces tab will stay empty.
	TraceCapacity int

	// EnableDashboard mounts /__nexus/* if true.
	EnableDashboard bool

	// GraphQL — environment-level flags that apply to every service's
	// mounted schema. Set once on the app, not per-service.

	// GraphQLPath overrides the default mount path for auto-generated
	// GraphQL services. Empty falls back to "/graphql". Per-service
	// paths via (*Service).AtGraphQL(p) still win over this default;
	// use this Config field to change where the auto-mount fallback
	// service (the one created when no *Service dep is present on a
	// handler) and any other service that doesn't call AtGraphQL
	// will mount their schema.
	//
	//    nexus.Config{GraphQLPath: "/api/graphql"}
	GraphQLPath string

	// DisablePlayground turns OFF the GraphQL Playground served on GET
	// <service>/<path>. Default is enabled. Flip in prod wiring to hide
	// the interactive console.
	DisablePlayground bool

	// GraphQLDebug skips query validation + response sanitization in
	// go-graph. Dev-only. Default false.
	GraphQLDebug bool

	// GraphQLPretty pretty-prints JSON responses. Convenient while
	// exploring; ship off in prod.
	GraphQLPretty bool

	// GlobalRateLimit applies across every endpoint — the whole app
	// consumes from one bucket. Combine with per-op nexus.RateLimit()
	// declarations for layered protection: the request must pass both
	// the global bucket and the op's bucket. Zero disables.
	//
	// Set PerIP to scope the global bucket per caller IP.
	GlobalRateLimit ratelimit.Limit

	// GlobalMiddleware stacks on the Gin engine root, so every REST
	// endpoint, GraphQL POST, WebSocket upgrade, and dashboard request
	// flows through it in registration order. Use for cross-cutting
	// concerns (request-id, logger, CORS, global rate limit, auth pre-
	// gate, etc.). Each bundle's Gin field runs; nil Gin realizations
	// are skipped silently. Per-op middleware (via nexus.Use on a
	// registration) layers on top.
	GlobalMiddleware []middleware.Middleware

	// RateLimitStore replaces the default in-memory rate-limit store.
	// Set when you want to share the store between the app and externally-
	// built middleware bundles (ratelimit.NewMiddleware consumes a Store),
	// or for persistence / multi-replica via a Redis-backed implementation.
	// Nil → app builds its own MemoryStore at boot (or cache-backed when
	// Cache is set — see below).
	RateLimitStore ratelimit.Store

	// MetricsStore replaces the default metrics store. Parallel to
	// RateLimitStore — explicit wins over Cache-driven defaults.
	MetricsStore metrics.Store

	// DashboardMiddleware gates the /__nexus surface behind user-supplied
	// middleware — typically auth + permission checks. Each bundle's
	// Gin realization runs in registration order on the /__nexus route
	// group BEFORE any dashboard handler, covering the JSON API,
	// WebSocket events, and the embedded Vue UI in one pass.
	//
	//	nexus.Config{
	//	    EnableDashboard: true,
	//	    DashboardMiddleware: []middleware.Middleware{
	//	        {Name: "auth",  Gin: bearerAuth},
	//	        {Name: "admin", Gin: requireAdminRole},
	//	    },
	//	}
	//
	// Bundles whose Gin field is nil are ignored for the dashboard (no
	// graph-only protection makes sense here — the dashboard itself
	// isn't GraphQL).
	DashboardMiddleware []middleware.Middleware

	// Cache is an optional nexus cache.Manager. When set, nexus uses it
	// as the default backing for metrics + rate-limit stores so counters
	// and overrides benefit from the app's cache tier (Redis when
	// configured via env, go-cache otherwise) without extra wiring.
	//
	// Explicit RateLimitStore / MetricsStore settings still win — this
	// is just the default when those are nil.
	Cache *cache.Manager

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
	// URL is the base URL for HTTP calls when this peer is remote.
	// Sugar for URLs with a single entry; ignored when URLs is set.
	// One of URL or URLs is required when the active deployment is
	// not this peer's tag; ignored for the active peer's own entry.
	URL string

	// URLs lists multiple replica base URLs for the same peer. When
	// non-empty, calls round-robin across replicas and passively
	// eject any replica that returns transport errors / 5xx for a
	// cooldown window. Use this when you scale a peer to N pods and
	// want the framework to balance instead of putting an external
	// load balancer in front of every peer:
	//
	//	"users-svc": {URLs: []string{
	//	    "http://users-1.cluster.local:8080",
	//	    "http://users-2.cluster.local:8080",
	//	    "http://users-3.cluster.local:8080",
	//	}},
	//
	// Setting both URL and URLs is permitted; URLs wins. An empty
	// URLs falls back to URL — back-compat with single-replica
	// configs.
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

// EffectiveURLs returns the resolved replica list for this peer.
// URLs takes precedence; URL is the singleton fallback. Empty result
// means the peer has no remote URL declared (a placeholder for the
// active deployment, or a misconfiguration the codegen / build-time
// validator should catch).
func (p Peer) EffectiveURLs() []string {
	if len(p.URLs) > 0 {
		return p.URLs
	}
	if p.URL != "" {
		return []string{p.URL}
	}
	return nil
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
