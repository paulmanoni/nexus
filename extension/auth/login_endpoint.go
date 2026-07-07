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
	path   string
	issue  LoginIssuer
	status int // success status; default 200
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
	cfg := &loginEndpointConfig{path: "/auth/login", status: http.StatusOK}
	for _, o := range opts {
		o(cfg)
	}
	return nexus.AsRestHandler("POST", cfg.path,
		func(m *Manager) httpx.HandlerFunc { return loginHandlerFunc(m, cfg) },
		nexus.Describe("Authenticate a username/password via the auth backend."),
		nexus.Public(),
	)
}

// loginHandlerFunc builds the raw handler so it owns status codes (401 vs
// 200) and response shaping directly.
func loginHandlerFunc(m *Manager, cfg *loginEndpointConfig) httpx.HandlerFunc {
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
		if cfg.issue != nil {
			body, ierr := cfg.issue(c.Request.Context(), id)
			if ierr != nil {
				c.JSON(http.StatusInternalServerError, httpx.H{"error": ierr.Error()})
				return
			}
			c.JSON(cfg.status, body)
			return
		}
		c.JSON(cfg.status, httpx.H{"identity": id})
	}
}
