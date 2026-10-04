package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"sync"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/auth"
	"github.com/paulmanoni/nexus/v2/extension/auth/authtest"
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

// users is the demo's accounts: alice / hunter2-hunter2.
var users = authtest.NewUsers().
	Add(auth.Identity{ID: "alice", Perms: []string{"pets.*"}}, "alice", "hunter2-hunter2")

func newToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
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
		// Bearer tokens for the SPA, with the built-in sign-in endpoints the
		// SDK's nx.auth.login / logout / me call.
		auth.Module(auth.Config{Users: auth.StaticUsers(users), Settings: &auth.Settings{
			Schemes:   map[string]auth.SchemeSettings{"api": {Type: auth.SchemeBearer}},
			Endpoints: auth.EndpointSettings{Login: "/login", Logout: "/logout", Me: "/me"},
		}}),
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
