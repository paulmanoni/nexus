// Package middleware is the metadata side of nexus middleware handling.
// It does NOT ship implementations (auth, CORS, rate limiting) — your code
// brings those. What it provides is a shared vocabulary so the dashboard can
// show, for each endpoint, which middleware ran and whether each is a
// well-known "builtin" name or something custom to your project.
package middleware

type Kind string

const (
	KindBuiltin Kind = "builtin"
	KindCustom  Kind = "custom"
)

// Middleware is the registry entry shown in the dashboard.
type Middleware struct {
	Name        string `json:"name"`
	Kind        Kind   `json:"kind"`
	Description string `json:"description,omitempty"`
}

// Builtins are well-known names nexus pre-registers. When your code calls
// .Use("auth", mw) or graphfx.Middlewares("auth"), nexus labels it builtin;
// any other name falls back to custom.
var Builtins = []Middleware{
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

// Builtin constructs a builtin Middleware entry (rarely needed — Builtins has
// the common ones). Use for project-specific middleware you consider standard.
func Builtin(name, desc string) Middleware {
	return Middleware{Name: name, Kind: KindBuiltin, Description: desc}
}

// Custom constructs a custom Middleware entry.
func Custom(name, desc string) Middleware {
	return Middleware{Name: name, Kind: KindCustom, Description: desc}
}
