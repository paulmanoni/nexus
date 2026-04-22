// An example showing nexus integrated with go.uber.org/fx in the style of the
// oats_admin_backend / applicant services: per-domain fx.Module, fx.Provide for
// the service struct, fx.Invoke to register endpoints against *nexus.App.
package main

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"nexus"
	"nexus/fxmod"
	"nexus/resource"
	"nexus/trace"
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

var petsModule = fx.Module("pets",
	fx.Provide(NewPetService),
	fx.Invoke(func(app *nexus.App, svc *PetService) {
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

var ownersModule = fx.Module("owners",
	fx.Provide(NewOwnerService),
	fx.Invoke(func(app *nexus.App, svc *OwnerService) {
		owners := app.Service("owners").Describe("Pet owners")
		owners.REST("GET", "/owners").Describe("List owners").Handler(svc.List)
	}),
)

// --- Graph domain (GraphQL, schema defined in schema.go) ---

var graphModule = fx.Module("graph",
	fx.Invoke(func(app *nexus.App) {
		graph := app.Service("graph").Describe("GraphQL demo")
		graph.MountGraphQL("/graphql", buildSchema())
	}),
)

// --- Resources (databases + cache) ---
//
// In a real app you'd wrap your existing DBManager / CacheManager — the
// healthy func is typically `dbm.IsConnected` or `cache.IsRedisConnected`.
// Here we fake it with package-level vars.

var (
	mainDBHealthy = true
	uaaDBHealthy  = true
	sessionHealth = true
)

var resourceModule = fx.Module("resources",
	fx.Invoke(func(app *nexus.App) {
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

		// Attach links resource to services for the Architecture edge view.
		app.Service("pets").Attach(mainDB).Attach(session)
		app.Service("owners").Attach(mainDB).Attach(session)
		app.Service("graph").Attach(mainDB).Attach(uaaDB)
	}),
)

// --- Boot ----------------------------------------------------------------

func main() {
	fx.New(
		fx.Supply(fxmod.Config{
			Addr:            ":8080",
			DashboardName:   "Fx Petstore",
			TraceCapacity:   1000,
			EnableDashboard: true,
		}),
		fxmod.Module,
		petsModule,
		ownersModule,
		graphModule,
		resourceModule,
	).Run()
}
