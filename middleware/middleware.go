// Package middleware defines nexus's cross-transport middleware model.
//
// Two shapes coexist here:
//
//  1. Info — the static descriptor the registry stores and the dashboard
//     renders. Just metadata: name, kind (builtin/custom), description.
//     Every resolver / route that uses a middleware contributes its name
//     to the endpoint's Middleware list; Info tells the dashboard how to
//     label each entry.
//
//  2. Middleware — an executable BUNDLE. Carries one realization per
//     transport (HTTP for REST + WS upgrades, Graph for GraphQL field
//     resolution). Factories like ratelimit.NewMiddleware produce one,
//     and nexus.Use(mw) accepts it on any registration regardless of
//     transport. Transports pick the field they can honor and ignore
//     the rest.
//
// Keeping these side-by-side means dashboard readers get a uniform view
// (name + kind + description) while implementers get a uniform API
// (write once, use everywhere).
package middleware

import (
	"github.com/paulmanoni/nexus/v2/httpx"

	"github.com/paulmanoni/nexus/v2/gql"
)

type Kind string

const (
	KindBuiltin Kind = "builtin"
	KindCustom  Kind = "custom"
)

// Stage is where an app-wide middleware runs, outermost first:
// Edge, then the framework's own (CORS, security headers, rate limit),
// then Session, Auth and App. Within a stage, declaration order holds,
// across modules. Declaring the stage, not a position, is what keeps
// "compression wraps everything" true wherever a module registers it.
type Stage int

const (
	// App is the default: application middleware, innermost.
	App Stage = iota
	// Edge is outermost: compression, client IP, request IDs.
	Edge
	// Session runs once the request passed the framework's edge checks.
	Session
	// Auth runs after sessions are loaded, before application middleware.
	Auth
)

// Rank orders stages outermost first.
func (s Stage) Rank() int {
	switch s {
	case Edge:
		return 0
	case Session:
		return 1
	case Auth:
		return 2
	}
	return 3
}

func (s Stage) String() string {
	switch s {
	case Edge:
		return "edge"
	case Session:
		return "session"
	case Auth:
		return "auth"
	}
	return "app"
}

// Info is the registry entry shown in the dashboard — pure metadata,
// no execution. Every Middleware bundle carries an Info so its name is
// self-describing when attached.
type Info struct {
	Name        string `json:"name"`
	Kind        Kind   `json:"kind"`
	Description string `json:"description,omitempty"`
	// Stage is set for app-wide middleware (nexus.Middleware): where in
	// the pipeline it runs.
	Stage string `json:"stage,omitempty"`
}

// Middleware is an executable bundle with per-transport realizations. A
// single definition serves REST (HTTP), GraphQL (Graph), and WebSocket
// (WS — runs at upgrade time; per-frame hooks are out of scope for v1).
// Leave a field nil when the middleware doesn't make sense for that
// transport (e.g. graphql-specific auth might only set Graph).
//
// The Info() companion returns the metadata the registry stores —
// factories pre-populate it so dashboard listings "just work" without
// users touching the static side.
type Middleware struct {
	Name        string
	Description string
	Kind        Kind              // defaults to KindCustom when unset by factories
	HTTP        httpx.HandlerFunc // REST + WS upgrade path
	Graph       gql.Middleware    // GraphQL field resolution
	// Stage places an app-wide middleware (nexus.Middleware) in the
	// request pipeline; per-endpoint bundles ignore it. Zero is App.
	Stage Stage
	// Requires is declarative metadata: the permission codenames this
	// bundle enforces (set by auth.Requires). The framework stamps them
	// onto the endpoint's registry entry (registry.AuthRequiresTag) so
	// gate evaluation (auth.OpGates) reads the SAME declaration the
	// enforcing middleware closed over — one source, no drift. Purely
	// metadata; attaching a bundle with Requires set enforces nothing
	// by itself.
	Requires []string
}

// AsInfo returns the registry-side metadata for this bundle, defaulting
// the kind to Custom when a factory didn't supply one.
func (m Middleware) AsInfo() Info {
	k := m.Kind
	if k == "" {
		k = KindCustom
	}
	return Info{Name: m.Name, Kind: k, Description: m.Description}
}

// Builtins are well-known names nexus pre-registers. When your code
// attaches a middleware with one of these names, the dashboard labels it
// "builtin"; any other name falls back to "custom".
var Builtins = []Info{
	{Name: "auth", Kind: KindBuiltin, Description: "Bearer token / session validation"},
	{Name: "cors", Kind: KindBuiltin, Description: "CORS preflight + header policy"},
	{Name: "rate-limit", Kind: KindBuiltin, Description: "Request rate limiting"},
	{Name: "request-id", Kind: KindBuiltin, Description: "Attach X-Request-ID per request"},
	{Name: "logger", Kind: KindBuiltin, Description: "Structured request logger"},
	{Name: "recovery", Kind: KindBuiltin, Description: "Panic recovery"},
	{Name: "permission", Kind: KindBuiltin, Description: "RBAC permission check"},
	{Name: "csrf", Kind: KindBuiltin, Description: "CSRF token validation"},
	{Name: "compression", Kind: KindBuiltin, Description: "Response compression"},
}
