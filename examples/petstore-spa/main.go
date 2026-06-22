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
	"github.com/paulmanoni/nexus/extension/frontend"
)

//go:embed all:web/dist
var webFS embed.FS

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

type LoginResp struct {
	Token string `json:"token"`
	User  User   `json:"user"`
}

type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

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

func login(_ context.Context, c Credentials) (LoginResp, error) {
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
			TraceCapacity: 200,
		},
		auth.Single(resolveToken),
		nexus.AsRest("POST", "/login", login, nexus.AuthRoute("login")),
		nexus.AsRest("POST", "/logout", logout, nexus.AuthRoute("logout"), auth.Required()),
		nexus.AsRest("GET", "/me", me, nexus.AuthRoute("me"), auth.Required()),
		nexus.AsCRUD[Pet](
			nexus.MemoryResolver[Pet](
				func(p *Pet) string { return p.ID },
				func(p *Pet, id string) { p.ID = id },
			),
			auth.Required(),
		),
		frontend.Plugin(frontend.Config{
			Root:       "web",
			Output:     "dist",
			Framework:  frontend.Vue,
			FS:         webFS,
			RuntimeSDK: true,
		}),
	)
}
