// An example showing nexus's top-level builder: nexus.Run composes modules,
// nexus.Provide / Invoke / Module replace fx.Provide / Invoke / Module —
// no go.uber.org/fx import in user code.
package main

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/resource"
	"github.com/paulmanoni/nexus/trace"
)

// --- Pets domain ---------------------------------------------------------

type PetService struct{}

func NewPetService() *PetService { return &PetService{} }

func (s *PetService) List(c *gin.Context) {
	start := time.Now()
	time.Sleep(3 * time.Millisecond) // fake DB call
	trace.Record(c, "db.pets.list", start, nil)
	c.JSON(http.StatusOK, gin.H{"pets": []string{"Rex", "Whiskers"}})
}

func (s *PetService) Create(c *gin.Context) {
	c.JSON(http.StatusCreated, gin.H{"ok": true})
}

var petsModule = nexus.Module("pets",
	nexus.Provide(NewPetService),
	nexus.Invoke(func(app *nexus.App, svc *PetService) {
		pets := app.Service("pets").Describe("Pet inventory")
		pets.REST("GET", "/pets").Describe("List all pets").Handler(svc.List)
		pets.REST("POST", "/pets").Describe("Create a pet").Handler(svc.Create)
	}),
)

// --- Owners domain (second service, to show the topology is multi-node) ---

type OwnerService struct{}

func NewOwnerService() *OwnerService { return &OwnerService{} }

func (s *OwnerService) List(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"owners": []string{"Amara", "Juma"}})
}

var ownersModule = nexus.Module("owners",
	nexus.Provide(NewOwnerService),
	nexus.Invoke(func(app *nexus.App, svc *OwnerService) {
		owners := app.Service("owners").Describe("Pet owners")
		owners.REST("GET", "/owners").Describe("List owners").Handler(svc.List)
	}),
)

// --- Graph domain (GraphQL, schema defined in schema.go) ---

var graphModule = nexus.Module("graph",
	nexus.Invoke(func(app *nexus.App) {
		graph := app.Service("graph").Describe("GraphQL demo")
		graph.MountGraphQL("/graphql", buildSchema())
	}),
)

// --- Resources (databases + cache) ---

var (
	mainDBHealthy = true
	uaaDBHealthy  = true
	sessionHealth = true
)

var resourceModule = nexus.Module("resources",
	nexus.Invoke(func(app *nexus.App) {
		mainDB := resource.NewDatabase("main-db", "Primary PostgreSQL",
			map[string]any{"engine": "postgres", "host": "localhost:5432", "schema": "app"},
			func() bool { return mainDBHealthy },
		)
		uaaDB := resource.NewDatabase("uaa-db", "User auth / authz",
			map[string]any{"engine": "postgres", "host": "localhost:5432", "schema": "uaa"},
			func() bool { return uaaDBHealthy },
		)
		session := resource.NewCache("session-cache", "Redis + in-memory fallback",
			map[string]any{"backend": "redis", "ttl": "30m"},
			func() bool { return sessionHealth },
		)

		app.Service("pets").Attach(mainDB).Attach(session)
		app.Service("owners").Attach(mainDB).Attach(session)
		app.Service("graph").Attach(mainDB).Attach(uaaDB)
	}),
)

// --- Boot ----------------------------------------------------------------

func main() {
	nexus.Run(
		nexus.Config{
			Server:        nexus.ServerConfig{Addr: ":8080"},
			Dashboard:     nexus.DashboardConfig{Enabled: true, Name: "Fx Petstore"},
			TraceCapacity: 1000,
		},
		petsModule,
		ownersModule,
		graphModule,
		resourceModule,
	)
}
