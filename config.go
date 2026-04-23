package nexus

import (
	"github.com/paulmanoni/nexus/cache"
	"github.com/paulmanoni/nexus/metrics"
	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/ratelimit"
)

// Config drives how nexus.Run builds the app. Supply it as the first
// argument to nexus.Run; users never construct a *App directly when using
// the top-level builder.
type Config struct {
	// Addr is the HTTP listen address (default ":8080").
	Addr string

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
}
