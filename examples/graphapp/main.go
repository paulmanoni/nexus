// Example: a complete GraphQL service wired with nexus's reflective
// controller API. Per-resolver middleware names, arg validators, and
// deprecation flow automatically from nexus/graph introspection.
//
//   - per-domain nexus.Module (advertsModule)
//   - managers (DBManager, CacheManager) describe their resources via
//     NexusResources(); nexus.ProvideResources registers + auto-attaches
//     them — no resourcesModule boilerplate
//   - resolvers are plain Go functions; nexus.AsQuery / AsMutation reflect
//     on them and build the typed graph.Resolver under the hood
//   - rate-limit bundles built once (init.go#createRateLimit) and attached
//     via nexus.Use — same value works on REST or GraphQL
//   - nexus owns the full boot story — no go.uber.org/fx import visible
//
// Run:  go run ./examples/graphapp  →  http://localhost:8080/__nexus/
package main

import (
	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/ratelimit"
)

func main() {
	nexus.Run(
		nexus.Config{
			Addr:            ":8080",
			DashboardName:   "GraphApp",
			TraceCapacity:   1000,
			EnableDashboard: true,
			// Share one store between the app (dashboard reads this via
			// /__nexus/ratelimits and operator overrides land here) and
			// the middleware bundle built in init.go — otherwise two
			// stores would drift. A single store via Config closes the loop.
			RateLimitStore: defaultStore,
			// Optional app-wide ceiling: rejects any caller exceeding
			// 600 rpm across all endpoints. Per-op limits layer on top.
			GlobalRateLimit: ratelimit.Limit{RPM: 600, Burst: 50},
		},
		nexus.Provide(NewMainDB, NewQuestionsDB, NewCacheManager),
		advertsModule,
	)
}
