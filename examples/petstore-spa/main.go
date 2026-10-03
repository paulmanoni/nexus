package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"sync"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
	"github.com/paulmanoni/nexus/v2/extension/frontend"
)

//go:embed all:web/dist
var webFS embed.FS

type Pet struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Age  int    `json:"age,omitempty"`
}

// PetInput is the body Create and Update take: the id comes from the path
// (or is assigned), never from the body.
type PetInput struct {
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

// PetsController is the pets resource: nexus.Resource registers its
// conventional actions — Index GET /pets, Show GET /pets/:id, Create POST
// /pets, Update PUT|PATCH /pets/:id, Destroy DELETE /pets/:id — the routes
// the client SDK's nx.crud('pets') calls.
type PetsController struct {
	mu   sync.Mutex
	pets map[string]Pet
}

func NewPetsController() *PetsController { return &PetsController{pets: map[string]Pet{}} }

func (c *PetsController) Index(ctx context.Context) ([]Pet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Pet, 0, len(c.pets))
	for _, p := range c.pets {
		out = append(out, p)
	}
	return out, nil
}

func (c *PetsController) Show(ctx context.Context, id string) (Pet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.pets[id]
	if !ok {
		return Pet{}, nexus.Err(nexus.NotFound, "no such pet")
	}
	return p, nil
}

func (c *PetsController) Create(ctx context.Context, in PetInput) (Pet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := Pet{ID: newToken()[:8], Name: in.Name, Age: in.Age}
	c.pets[p.ID] = p
	return p, nil
}

func (c *PetsController) Update(ctx context.Context, id string, in PetInput) (Pet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.pets[id]; !ok {
		return Pet{}, nexus.Err(nexus.NotFound, "no such pet")
	}
	p := Pet{ID: id, Name: in.Name, Age: in.Age}
	c.pets[id] = p
	return p, nil
}

func (c *PetsController) Destroy(ctx context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pets, id)
	return nil
}

func main() {
	nexus.Run(
		config.Runtime{
			Server:        config.Server{Addr: ":8080"},
			Dashboard:     config.Dashboard{Enabled: true, Name: "Petstore SPA"},
			TraceCapacity: 200,
		},
		auth.Single(resolveToken),
		nexus.AsRest("POST", "/login", login, nexus.AuthRoute("login")),
		nexus.AsRest("POST", "/logout", logout, nexus.AuthRoute("logout"), auth.Required()),
		nexus.AsRest("GET", "/me", me, nexus.AuthRoute("me"), auth.Required()),
		nexus.Resource[*PetsController]("/pets", auth.Required()).Provide(NewPetsController),
		frontend.Plugin(frontend.Config{
			Root:       "web",
			Output:     "dist",
			Framework:  frontend.Vue,
			FS:         webFS,
			RuntimeSDK: true,
		}),
	)
}
