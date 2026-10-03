package auth

import (
	"context"
	"net/http"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/httpx"
)

// LoginRequest is the JSON body the built-in login endpoint accepts. Field
// names are the conventional "username" / "password".
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoginIssuer turns a freshly-authenticated identity into the response body
// — typically issuing and returning a token. Without an issuer the login
// endpoint returns the identity itself.
type LoginIssuer func(ctx context.Context, id *Identity) (any, error)

// LoginHandler is the raw login handler Config.Endpoints.Login mounts, exported so
// an app whose issuer needs DI dependencies (e.g. a token server) can wire
// it inside its own AsRestHandler factory — where those deps ARE injected —
// instead of the Backend's Issue capability:
//
//	nexus.AsRestHandler("POST", "/auth/login",
//	    func(m *auth.Manager, srv *TokenServer) httpx.HandlerFunc {
//	        return auth.LoginHandler(m, func(ctx, id *auth.Identity) (any, error) {
//	            return srv.IssueToken(ctx, id.ID)   // uses the DI-injected srv
//	        })
//	    }, nexus.Public())
//
// It reads {username, password}, runs Manager.Login, and owns the status
// codes: 422 on a bad body, 401 (uniform, no enumeration) on invalid
// credentials, 200 with the issuer's body (or {"identity": …} when issue is
// nil) on success.
func LoginHandler(m *Manager, issue LoginIssuer) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		var req LoginRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			nexus.WriteError(c, nexus.Err(nexus.InvalidInput, "invalid request body"))
			return
		}
		id, err := m.Login(c.Request.Context(), Password{Username: req.Username, Password: req.Password})
		if err != nil || id == nil {
			// Uniform 401 — never distinguish unknown user from bad password.
			nexus.WriteError(c, nexus.Err(nexus.Unauthenticated, "invalid credentials"))
			return
		}
		if issue != nil {
			body, ierr := issue(c.Request.Context(), id)
			if ierr != nil {
				nexus.WriteError(c, ierr)
				return
			}
			c.JSON(http.StatusOK, body)
			return
		}
		c.JSON(http.StatusOK, httpx.H{"identity": id})
	}
}
