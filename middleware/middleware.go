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
//     transport (Gin for REST + WS upgrades, Graph for GraphQL field
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
	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/graph"
)

type Kind string

const (
	KindBuiltin Kind = "builtin"
	KindCustom  Kind = "custom"
)

// Info is the registry entry shown in the dashboard — pure metadata,
// no execution. Every Middleware bundle carries an Info so its name is
// self-describing when attached.
type Info struct {
	Name        string `json:"name"`
	Kind        Kind   `json:"kind"`
	Description string `json:"description,omitempty"`
}

// Middleware is an executable bundle with per-transport realizations. A
// single definition serves REST (Gin), GraphQL (Graph), and WebSocket
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
	Kind        Kind            // defaults to KindCustom when unset by factories
	Gin         gin.HandlerFunc // REST + WS upgrade path
	Graph       graph.FieldMiddleware
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

// Builtin constructs a builtin Info entry (rarely needed — Builtins has
// the common ones). Use for project-specific middleware you consider
// standard.
func Builtin(name, desc string) Info {
	return Info{Name: name, Kind: KindBuiltin, Description: desc}
}

// Custom constructs a custom Info entry.
func Custom(name, desc string) Info {
	return Info{Name: name, Kind: KindCustom, Description: desc}
}
