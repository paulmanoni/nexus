// Command petstore-spa is a runnable end-to-end demo of the nexus
// client SDK. One Go file, one HTML page, one Vue setup script —
// the smallest reasonable app exercising:
//
//   - Config.Client.Enabled auto-mounts the SDK at /__nexus/client/*
//   - auth.Module's bridge populates the manifest's Auth section
//     so the browser knows where to put the token
//   - nexus.AuthRoute("login"|"logout"|"me") promotes plain REST
//     handlers into the SDK's auth namespace
//   - AsCRUD[Pet] generates 5 REST routes the SDK consumes via
//     useCrud('pets')
//   - ServeFrontend mounts the embedded SPA at /
//
// Run:
//
//	go run ./examples/petstore-spa
//	open http://localhost:8080
//
// Login as alice / hunter2.
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"sync"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/auth"
	"github.com/paulmanoni/nexus/client"
)

//go:embed all:web
var webFS embed.FS

// Pet is the entity behind /pets — kept tiny so the demo focuses
// on the wiring, not the schema. JSON tags drive both the wire
// format AND the SDK's TS interface (manifest's Refs section).
type Pet struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Age  int    `json:"age,omitempty"`
}

// Credentials is the login args body. Mirrors the shape useAuth's
// login(creds) call sends from the browser side.
type Credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoginResp is the login result. Carries a `token` field so the
// SDK auto-stashes it via the configured token store on success.
type LoginResp struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}

type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// tokens is a process-local map of opaque token → user identity.
// Real apps swap in a JWT verifier or a session store; this is the
// minimum viable Resolve so the demo runs without external deps.
var (
	tokensMu sync.Mutex
	tokens   = map[string]auth.Identity{}
)

func newToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func resolveToken(_ context.Context, token string) (*auth.Identity, error) {
	tokensMu.Lock()
	defer tokensMu.Unlock()
	if id, ok := tokens[token]; ok {
		return &id, nil
	}
	return nil, errors.New("invalid token")
}

// login validates credentials, mints a token, registers it for
// future Resolve calls, and returns the token + user shape. The
// SDK side picks up `token` automatically via auth.login().
func login(_ context.Context, c Credentials) (LoginResp, error) {
	// Hardcoded credentials for the demo. A real app validates
	// against its user store; the framework doesn't care how.
	if c.Username != "alice" || c.Password != "hunter2" {
		return LoginResp{}, errors.New("invalid credentials")
	}
	tok := newToken()
	tokensMu.Lock()
	tokens[tok] = auth.Identity{ID: c.Username, Roles: []string{"user"}}
	tokensMu.Unlock()
	return LoginResp{
		Token: tok,
		User:  User{ID: c.Username, Name: "Alice"},
	}, nil
}

// logout drops every token mapped to the requesting user. Called
// via auth.logout() on the browser side; the SDK clears its local
// token even if this returns an error, so the user is never stuck
// in a half-logged-in state.
func logout(ctx context.Context, mgr *auth.Manager) error {
	id, ok := auth.IdentityFrom(ctx)
	if !ok {
		return nil
	}
	tokensMu.Lock()
	for tok, identity := range tokens {
		if identity.ID == id.ID {
			delete(tokens, tok)
		}
	}
	tokensMu.Unlock()
	mgr.InvalidateByIdentity(id.ID)
	return nil
}

// me returns the current Identity. Bootstrapped by useAuth() on
// page load when a token is in the SDK's local store; cookie-based
// apps (different auth.Extract config) work transparently because
// the cookie ride-along makes the same call succeed without a
// token in localStorage.
func me(ctx context.Context) (User, error) {
	id, ok := auth.IdentityFrom(ctx)
	if !ok {
		return User{}, errors.New("no identity")
	}
	return User{ID: id.ID, Name: id.ID}, nil
}

func main() {
	nexus.Run(
		nexus.Config{
			Server:        nexus.ServerConfig{Addr: ":8080"},
			Dashboard:     nexus.DashboardConfig{Enabled: true, Name: "Petstore SPA"},
			Client:        client.Config{Enabled: true},
			TraceCapacity: 200,
		},

		// Auth — Bearer token resolved against the in-memory token
		// table. auth.Module's Invoke also bridges the strategy
		// info into the SDK manifest (added in step 9), so the
		// browser SDK knows to send "Authorization: Bearer <token>"
		// without any user-side configuration.
		auth.Module(auth.Config{
			Extract: auth.Bearer(),
			Resolve: resolveToken,
		}),

		// Auth flow endpoints — AuthRoute markers promote them
		// into the SDK's auth namespace (auth.login / .logout /
		// .me) so the browser code reads idiomatically.
		nexus.AsRest("POST", "/login", login, nexus.AuthRoute("login")),
		nexus.AsRest("POST", "/logout", logout, nexus.AuthRoute("logout"), auth.Required()),
		nexus.AsRest("GET", "/me", me, nexus.AuthRoute("me"), auth.Required()),

		// CRUD — AsCRUD generates the 5 REST routes (GET list,
		// GET one, POST, PATCH, DELETE) the SDK consumes via
		// nx.crud('pets'). MemoryResolver is fine for a demo;
		// real apps wire a GORM-backed Store via the resolver.
		nexus.AsCRUD[Pet](
			nexus.MemoryResolver[Pet](
				func(p *Pet) string    { return p.ID },
				func(p *Pet, id string) { p.ID = id },
			),
			auth.Required(),
		),

		// SPA — embedded HTML + Vue setup script. ServeFrontend
		// catches every route the framework didn't claim and
		// either serves a static asset or falls back to index.html
		// (SPA client-side routing).
		nexus.ServeFrontend(webFS, "web"),
	)
}
