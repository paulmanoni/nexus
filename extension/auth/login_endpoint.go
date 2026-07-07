package auth

import (
	"context"
	"net/http"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/httpx"
)

// LoginRequest is the JSON body the built-in login endpoint accepts. Field
// names are the conventional "username" / "password".
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoginIssuer turns a freshly-authenticated identity into the response body
// — typically issuing and returning a token. When a LoginEndpoint has no
// issuer, the endpoint returns the identity itself.
type LoginIssuer func(ctx context.Context, id *Identity) (any, error)

// loginEndpointConfig holds the resolved LoginEndpoint options.
type loginEndpointConfig struct {
	path  string
	issue LoginIssuer
}

// LoginOption configures LoginEndpoint.
type LoginOption func(*loginEndpointConfig)

// LoginAt overrides the endpoint path (default "/auth/login").
func LoginAt(path string) LoginOption {
	return func(c *loginEndpointConfig) { c.path = path }
}

// WithIssuer sets the function that shapes the success response — e.g. mints
// a JWT or session token from the identity. Without it the endpoint returns
// the identity as {"identity": ...}.
func WithIssuer(issue LoginIssuer) LoginOption {
	return func(c *loginEndpointConfig) { c.issue = issue }
}

// LoginEndpoint registers a POST endpoint that authenticates a username /
// password through Manager.Login (i.e. the login-capable Config.Backend) and
// returns the result — the HTTP front door for the cohesive backend, so an
// app no longer hand-writes a login handler just to reach Manager.Login.
//
//	auth.Module(auth.Config{ Backend: auth.UseBackend(NewAuthBackend) }),
//	auth.LoginEndpoint(auth.LoginAt("/auth/login"), auth.WithIssuer(mintJWT)),
//
// It is Public (you can't require a token to obtain one). Invalid
// credentials return 401 with {"error": ...}; success returns the issuer's
// body, or {"identity": ...} when no issuer is set. Requires a Config.Backend
// that implements Login — without one every request gets 401.
func LoginEndpoint(opts ...LoginOption) nexus.Option {
	cfg := &loginEndpointConfig{path: "/auth/login"}
	for _, o := range opts {
		o(cfg)
	}
	return nexus.AsRestHandler("POST", cfg.path,
		func(m *Manager) httpx.HandlerFunc { return LoginHandler(m, cfg.issue) },
		nexus.Describe("Authenticate a username/password via the auth backend."),
		nexus.Public(),
	)
}

// LoginHandler is the raw login handler LoginEndpoint installs, exported so
// an app whose issuer needs DI dependencies (e.g. a token server) can wire
// it inside its own AsRestHandler factory — where those deps ARE injected —
// instead of the static WithIssuer callback:
//
//	nexus.AsRestHandler("POST", "/auth/login",
//	    func(m *auth.Manager, srv *TokenServer) httpx.HandlerFunc {
//	        return auth.LoginHandler(m, func(ctx, id *auth.Identity) (any, error) {
//	            return srv.IssueToken(ctx, id.ID)   // uses the DI-injected srv
//	        })
//	    }, nexus.Public())
//
// It reads {username, password}, runs Manager.Login, and owns the status
// codes: 400 on a bad body, 401 (uniform, no enumeration) on invalid
// credentials, 200 with the issuer's body (or {"identity": …} when issue is
// nil) on success.
func LoginHandler(m *Manager, issue LoginIssuer) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		var req LoginRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, httpx.H{"error": "invalid request body"})
			return
		}
		id, err := m.Login(c.Request.Context(), Password{Username: req.Username, Password: req.Password})
		if err != nil || id == nil {
			// Uniform 401 — never distinguish unknown user from bad password.
			c.JSON(http.StatusUnauthorized, httpx.H{"error": "invalid credentials"})
			return
		}
		if issue != nil {
			body, ierr := issue(c.Request.Context(), id)
			if ierr != nil {
				c.JSON(http.StatusInternalServerError, httpx.H{"error": ierr.Error()})
				return
			}
			c.JSON(http.StatusOK, body)
			return
		}
		c.JSON(http.StatusOK, httpx.H{"identity": id})
	}
}
