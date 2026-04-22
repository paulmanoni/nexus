// Example: a complete GraphQL service built with github.com/paulmanoni/go-graph
// and registered into a nexus dashboard. Mirrors oats_admin_backend controllers:
//
//   - per-domain fx.Module (advertsModule)
//   - resources declared ONCE at the top (resourcesModule) and referenced by
//     name via Service.Using(...) or UsingDefaults()
//   - resolvers built with graph.NewResolver[...].WithMiddleware(...).BuildQuery()
//   - fields collected via Fx value groups and assembled by graph.SchemaBuilder
//
// Run:  go run ./examples/graphapp  →  http://localhost:8080/__nexus/
package main

import (
	"go.uber.org/fx"

	"nexus/fxmod"
	"nexus/graphfx"
)

func main() {
	fx.New(
		fx.Supply(fxmod.Config{
			Addr:            ":8080",
			DashboardName:   "GraphApp",
			TraceCapacity:   1000,
			EnableDashboard: true,
		}),

		// Shared infra providers — two named DBManagers + a CacheManager.
		// Fx distinguishes the two DBManager instances by constructor identity
		// (NewMainDB vs NewQuestionsDB); both land in the multi.Registry via
		// ProvideDBs so resolvers say dbs.Using("main") / .Using("questions").
		fx.Provide(
			ProvideDBs,       // multi.Registry[*DBManager] with "main" + "questions"
			NewCacheManager,
		),

		fxmod.Module,
		graphfx.Module,  // provides *graphfx.Bundle from grouped fields
		resourcesModule, // registers nexus.Resources — runs before domain invokes
		advertsModule,   // domain: Using("") resolves to the default DB registered above
	).Run()
}
