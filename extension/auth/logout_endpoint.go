package auth

import (
	"context"
	"net/http"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/httpx"
)

// LogoutRevoker invalidates the underlying token in the app's own store
// (e.g. an OAuth2 access/refresh token, a DB session row) — the durable
// counterpart to Manager.Invalidate, which only drops the in-memory cached
// identity. Optional; without one, logout just clears the identity cache.
type LogoutRevoker func(ctx context.Context, token string) error

// logoutEndpointConfig holds the resolved LogoutEndpoint options.
type logoutEndpointConfig struct {
	path    string
	extract Extractor
	revoke  LogoutRevoker
}

// LogoutOption configures LogoutEndpoint.
type LogoutOption func(*logoutEndpointConfig)

// LogoutAt overrides the endpoint path (default "/auth/logout").
func LogoutAt(path string) LogoutOption {
	return func(c *logoutEndpointConfig) { c.path = path }
}

// LogoutExtractor sets how the token to revoke is pulled from the request
// (default Bearer()). Match it to how you carry the token — e.g.
// auth.Cookie("access_token") for cookie sessions.
func LogoutExtractor(e Extractor) LogoutOption {
	return func(c *logoutEndpointConfig) { c.extract = e }
}

// WithRevoker sets the function that invalidates the underlying token in the
// app's store (OAuth2 server, DB session, …). Manager.Invalidate is always
// called too, to drop the cached identity immediately.
func WithRevoker(revoke LogoutRevoker) LogoutOption {
	return func(c *logoutEndpointConfig) { c.revoke = revoke }
}

// LogoutEndpoint registers a POST endpoint (default "/auth/logout") that
// invalidates the presented token — the companion to LoginEndpoint. It
// extracts the token, drops it from the identity cache (Manager.Invalidate),
// and, when a revoker is set, invalidates it in the app's own store:
//
//	auth.LogoutEndpoint(auth.WithRevoker(func(ctx, tok string) error {
//	    return srv.Manager.RemoveAccessToken(ctx, tok)   // revoke in the OAuth2 store
//	}))
//
// It is Public and idempotent — it authenticates by the very token it
// revokes, so a stale token can still log out. Always returns 200
// {"ok": true} (revealing nothing about whether the token existed); a
// revoker error surfaces as 500.
//
// Deprecated: set Config.Endpoints.Logout instead, which mounts the same
// handler from inside auth.Module and takes the revoker from the Backend's
// RevokeToken capability. LogoutEndpoint remains a thin wrapper and keeps
// working.
func LogoutEndpoint(opts ...LogoutOption) nexus.Option {
	cfg := &logoutEndpointConfig{path: "/auth/logout", extract: Bearer()}
	for _, o := range opts {
		o(cfg)
	}
	if cfg.extract == nil {
		cfg.extract = Bearer()
	}
	return nexus.AsRestHandler("POST", cfg.path,
		func(m *Manager) httpx.HandlerFunc { return LogoutHandler(m, cfg.extract, cfg.revoke) },
		nexus.Describe("Invalidate the presented auth token (logout)."),
		nexus.Public(),
	)
}

// LogoutHandler is the raw logout handler LogoutEndpoint installs, exported
// so an app whose revoker needs DI dependencies (e.g. a token server) can
// wire it inside its own AsRestHandler factory — where those deps ARE
// injected — instead of the static WithRevoker callback:
//
//	nexus.AsRestHandler("POST", "/auth/logout",
//	    func(m *auth.Manager, srv *TokenServer) httpx.HandlerFunc {
//	        return auth.LogoutHandler(m, auth.Bearer(), func(ctx, tok string) error {
//	            return srv.Revoke(ctx, tok)   // uses the DI-injected srv
//	        })
//	    }, nexus.Public())
//
// extract nil defaults to Bearer(); revoke may be nil (cache-only logout).
// Always returns 200 {"ok": true} — idempotent, leaking nothing about
// whether a session existed.
func LogoutHandler(m *Manager, extract Extractor, revoke LogoutRevoker) httpx.HandlerFunc {
	if extract == nil {
		extract = Bearer()
	}
	return func(c *httpx.Ctx) {
		token, ok := extract.Extract(c.Request)
		if ok && token != "" {
			m.Invalidate(token) // drop the cached identity immediately
			if revoke != nil {
				if err := revoke(c.Request.Context(), token); err != nil {
					c.JSON(http.StatusInternalServerError, httpx.H{"error": err.Error()})
					return
				}
			}
		}
		// Idempotent: succeed whether or not a token was present, so logout
		// never leaks whether a session existed.
		c.JSON(http.StatusOK, httpx.H{"ok": true})
	}
}
