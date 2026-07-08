package auth

import (
	"context"
	"net/http"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/httpx"
)

// Endpoints opts auth.Module into mounting its own HTTP front doors, so a
// single auth.Module(auth.Config{...}) call owns the whole auth surface
// instead of the app hand-wiring AsRest lines next to it. Each endpoint is
// off unless its path is set, and every handler draws entirely on the
// Config.Backend's capabilities:
//
//   - Login  → Backend.Login, then Backend.Issue shapes the response
//     (no Issue → returns {"identity": …}).
//   - Logout → Manager.Invalidate + Backend.RevokeToken.
//   - Token  → Backend.TokenHandler (e.g. an OAuth2 grant endpoint).
//   - Revoke → Manager.Invalidate + Backend.RevokeToken.
//
// All four are Public — you can't require a token to obtain or drop one.
// The zero value mounts nothing, so existing configs are unaffected. This
// supersedes the standalone LoginEndpoint / LogoutEndpoint options.
type Endpoints struct {
	// Login mounts a Public POST that reads {username, password}, runs
	// Backend.Login, and returns Backend.Issue's body (or {"identity": …}).
	Login string
	// Logout mounts a Public, idempotent POST that drops the presented
	// token from the cache and, when the backend can, revokes it.
	Logout string
	// Token mounts a Public POST bound to Backend.TokenHandler — the raw
	// token grant endpoint (OAuth2 password/refresh/client_credentials).
	Token string
	// Revoke mounts a Public POST that revokes the presented token — the
	// same behavior as Logout under a token-server-flavored path.
	Revoke string
	// LogoutExtract pulls the token to revoke on Logout / Revoke. Nil →
	// Bearer(). Set to auth.Cookie("access_token") for cookie sessions.
	LogoutExtract Extractor
}

// any reports whether at least one endpoint path is configured.
func (e Endpoints) any() bool {
	return e.Login != "" || e.Logout != "" || e.Token != "" || e.Revoke != ""
}

// --- token-server capabilities (discovered by assertion, like resolve/login/authorize) ---

// issueCapable is a backend that can mint a login response (typically a
// token pair) for an already-authenticated identity. Powers Endpoints.Login.
type issueCapable interface {
	Issue(ctx context.Context, id *Identity) (any, error)
}

// revokeCapable is a backend that can invalidate a raw token in its own
// store (an OAuth2 access token, a DB session row). Powers Endpoints.Logout
// and Endpoints.Revoke; also available to LogoutEndpoint via Manager.
type revokeCapable interface {
	RevokeToken(ctx context.Context, token string) error
}

// tokenServerCapable is a backend that serves a raw token grant endpoint —
// e.g. an OAuth2 server's HandleTokenRequest. Powers Endpoints.Token.
type tokenServerCapable interface {
	TokenHandler() httpx.HandlerFunc
}

// issuer returns the backend's login issuer, or nil when the backend can't
// issue. Read at request time so it reflects the finalized backend.
func (m *Manager) issuer() LoginIssuer {
	if ic, ok := m.state.backend.(issueCapable); ok {
		return ic.Issue
	}
	return nil
}

// revoker returns the backend's token revoker, or nil when the backend
// can't revoke. Read at request time.
func (m *Manager) revoker() LogoutRevoker {
	if rc, ok := m.state.backend.(revokeCapable); ok {
		return rc.RevokeToken
	}
	return nil
}

// tokenHandler returns the backend's token grant handler, or nil when the
// backend is not a token server. Read at request time.
func (m *Manager) tokenHandler() httpx.HandlerFunc {
	if ts, ok := m.state.backend.(tokenServerCapable); ok {
		return ts.TokenHandler()
	}
	return nil
}

// endpointOptions builds the AsRestHandler options for the configured
// Endpoints. Each factory injects the *Manager and produces a handler that
// resolves the backend capability at request time — safe regardless of
// whether the backend finalize invoke ran before or after route building.
func endpointOptions(e Endpoints) []nexus.Option {
	var opts []nexus.Option
	if e.Login != "" {
		opts = append(opts, nexus.AsRestHandler("POST", e.Login,
			func(m *Manager) httpx.HandlerFunc {
				return func(c *httpx.Ctx) { LoginHandler(m, m.issuer())(c) }
			},
			nexus.Describe("Authenticate a username/password via the auth backend."),
			nexus.Public(),
		))
	}
	if e.Logout != "" {
		opts = append(opts, nexus.AsRestHandler("POST", e.Logout,
			func(m *Manager) httpx.HandlerFunc {
				return func(c *httpx.Ctx) {
					LogoutHandler(m, logoutExtractor(e), m.revoker())(c)
				}
			},
			nexus.Describe("Invalidate the presented auth token (logout)."),
			nexus.Public(),
		))
	}
	if e.Token != "" {
		opts = append(opts, nexus.AsRestHandler("POST", e.Token,
			func(m *Manager) httpx.HandlerFunc {
				return func(c *httpx.Ctx) {
					h := m.tokenHandler()
					if h == nil {
						c.JSON(http.StatusNotImplemented, httpx.H{"error": "no token server configured"})
						return
					}
					h(c)
				}
			},
			nexus.Describe("Token grant endpoint (OAuth2 password/refresh/client_credentials)."),
			nexus.Public(),
		))
	}
	if e.Revoke != "" {
		opts = append(opts, nexus.AsRestHandler("POST", e.Revoke,
			func(m *Manager) httpx.HandlerFunc {
				return func(c *httpx.Ctx) {
					LogoutHandler(m, logoutExtractor(e), m.revoker())(c)
				}
			},
			nexus.Describe("Revoke the presented auth token."),
			nexus.Public(),
		))
	}
	return opts
}

func logoutExtractor(e Endpoints) Extractor {
	if e.LogoutExtract != nil {
		return e.LogoutExtract
	}
	return Bearer()
}
