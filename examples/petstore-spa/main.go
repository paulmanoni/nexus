// Command petstore-spa is a runnable end-to-end demo of the nexus
// frontend extension. One Go file, one HTML page, one Vue setup
// script — the smallest reasonable app exercising:
//
//   - frontend.Plugin replaces ServeFrontend + Client.Enabled in
//     one declaration. The plugin mounts the embedded SPA AND
//     registers a codegen driver so `nexus generate frontend` can
//     project the live registry into typed TS source.
//   - auth.Module's bridge populates the manifest's Auth section
//     so the browser knows where to put the token.
//   - nexus.AuthRoute("login"|"logout"|"me") promotes plain REST
//     handlers into the SDK's auth flows.
//   - AsCRUD[Pet] generates 5 REST routes the browser consumes via
//     the runtime SDK (useCrud('pets')).
//
// Run:
//
//	go run ./examples/petstore-spa
//	open http://localhost:8080
//
// Login as alice / hunter2.
//
// On the codegen story: this example has NO bundler — the browser
// loads Vue from a CDN and imports the runtime SDK at
// /__nexus/client/vue.js, the same way it did before the frontend
// extension landed. A Vite-driven project would instead run
//
//	nexus generate frontend --url http://localhost:8080
//
// to write web/src/__nexus/{_client,types,index,vue}.ts, then import
// the typed per-op functions directly. The plugin's Generate slot
// makes that one command; this example's app.js doesn't exercise
// the typed path because the no-bundler constraint rules out TS.
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"sync"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/client"
	"github.com/paulmanoni/nexus/extension/auth"
	"github.com/paulmanoni/nexus/extension/frontend"
)

//go:embed all:web/dist
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
			Server:    nexus.ServerConfig{Addr: ":8080"},
			Dashboard: nexus.DashboardConfig{Enabled: true, Name: "Petstore SPA"},
			// Client.Enabled keeps /__nexus/client/manifest.json
			// reachable so `nexus generate frontend --url` can pull
			// the typed schema. Production deploys flip this off
			// once the generated tree is committed; the example
			// keeps it on so the regenerate flow is one command.
			Client:        client.Config{Enabled: true},
			TraceCapacity: 200,
		},

		// Auth — Bearer token resolved against the in-memory token
		// table. auth.Module's Invoke bridges the strategy info
		// into the SDK manifest, so the browser SDK knows to send
		// "Authorization: Bearer <token>" without any user-side
		// configuration.
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
		// GET one, POST, PATCH, DELETE) keyed on /pets. The
		// generated tree picks them up as listPets / getPetsById /
		// createPet / etc.
		nexus.AsCRUD[Pet](
			nexus.MemoryResolver[Pet](
				func(p *Pet) string     { return p.ID },
				func(p *Pet, id string) { p.ID = id },
			),
			auth.Required(),
		),

		// Frontend — one declaration replaces what used to be
		// nexus.ServeFrontend + Config.Client wiring. The //go:embed
		// FS satisfies the runtime mount; the registered Generate
		// driver lets `nexus generate frontend --url ... --out
		// web/src/__nexus` regenerate the typed TS surface from
		// the live manifest. This example doesn't exercise the
		// typed path (no bundler), but flipping Framework to React
		// or Svelte + adding a Vite project under web/ is enough
		// to light it up.
		frontend.Plugin(frontend.Config{
			Root:      "web",
			Output:    "dist",
			Framework: frontend.Vue,
			FS:        webFS,
		}),
	)
}
