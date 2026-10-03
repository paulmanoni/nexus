package auth

import (
	"context"
	"net/http"

	"github.com/paulmanoni/nexus/v2/httpx"
)

// LogoutRevoker invalidates the underlying token in the app's own store
// (e.g. an OAuth2 access/refresh token, a DB session row) — the durable
// counterpart to Manager.Invalidate, which only drops the in-memory cached
// identity. Optional; without one, logout just clears the identity cache.
type LogoutRevoker func(ctx context.Context, token string) error

// LogoutHandler is the raw logout handler Config.Endpoints.Logout mounts, exported
// so an app whose revoker needs DI dependencies (e.g. a token server) can
// wire it inside its own AsRestHandler factory — where those deps ARE
// injected — instead of the Backend's RevokeToken capability:
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
