package config

import "crypto/tls"

// ListenerScope decides which routes a listener exposes. The framework
// uses the request's bound local address (via http.LocalAddrContextKey)
// to look up the scope and 404s requests to routes outside that scope.
//
// The scope abstraction is opt-in: when Config.Listeners is empty, a
// single listener bound to Config.Addr serves every route (today's
// behavior). The scope filter only fires for explicitly-declared
// listeners.
type ListenerScope int

const (
	// ScopePublic exposes user-facing routes (REST, GraphQL, WebSocket)
	// and hides the /__nexus dashboard surface. The default for any
	// listener whose Scope is left zero — public is the safe default
	// for the listener bound to the world.
	ScopePublic ListenerScope = iota

	// ScopeInternal exposes user-facing routes plus /__nexus/health
	// and /__nexus/ready, so peer services can call your handlers and
	// orchestrators (k8s probes, load balancers) can poll readiness.
	// The rest of /__nexus stays hidden.
	ScopeInternal

	// ScopeAdmin exposes everything — /__nexus surface AND user
	// routes. The admin listener is meant for operators (typically
	// bound to a private subnet or behind an SSH tunnel), so giving
	// it the full route set is a UX win: the dashboard's in-page
	// RestTester / GraphQLTester make relative fetch() calls, and
	// blocking user routes here would silently 404 those.
	//
	// If you need a strictly-dashboard-only listener, that's a
	// future ScopeIntrospection — the current ScopeAdmin trades
	// surface area for ergonomics.
	ScopeAdmin
)

// String returns the lowercase scope name. Dashboards and logs render
// scopes by name; keeping the mapping in one place makes additions
// future-safe.
func (s ListenerScope) String() string {
	switch s {
	case ScopePublic:
		return "public"
	case ScopeInternal:
		return "internal"
	case ScopeAdmin:
		return "admin"
	}
	return "unknown"
}

// Listener declares one bound address with a scope. Multiple listeners
// can share a scope (e.g. one bound to 0.0.0.0:8080 and another to a
// loopback for sidecar health checks).
type Listener struct {
	// Addr is the listen address (e.g. ":8080", "127.0.0.1:9000").
	// Required — an empty Addr is rejected by Run with a precise
	// error message.
	Addr string

	// Scope decides which routes this listener exposes. Zero value
	// is ScopePublic — the conservative default for an exposed port.
	Scope ListenerScope

	// TLS, when non-nil, terminates TLS on this listener. The raw
	// TCP listener is wrapped with tls.NewListener at bind time so
	// the same http.Server serves HTTPS without a second code path.
	// Leave nil for plain HTTP (today's behavior on every listener).
	//
	// Build via ServerTLSConfig for a typical cert/key (and optional
	// client-CA for mTLS), or supply a *tls.Config directly when you
	// need custom cipher suites, SNI via GetCertificate, etc.
	//
	// For public-internet HTTPS with Let's Encrypt auto-issuance,
	// prefer extension/tls.Plugin — it owns its own :443/:80 pair
	// and handles ACME challenges. This field is the right tool for
	// an admin/internal listener fronted by your own cert material
	// (e.g. an internal CA, mTLS-protected dashboard).
	TLS *tls.Config
}
