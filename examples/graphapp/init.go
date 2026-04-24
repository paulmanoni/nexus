package main

import (
	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/ratelimit"
)

// advertsModule wires the adverts domain. Resolvers are plain Go functions
// (adverts.go); nexus.AsQuery / AsMutation reflect on them. The auto-mount
// Invoke built into nexus assembles the schema and mounts it at the
// service's AtGraphQL path — no MountGraphQL call needed here.
//
// createRateLimit shows the cross-transport middleware pattern: one
// ratelimit.NewMiddleware call produces a middleware.Middleware bundle
// with Gin + Graph realizations. nexus.Use attaches it to the resolver;
// the same bundle could be reused on a REST AsRest registration.
var createRateLimit = nexus.Use(ratelimit.NewMiddleware(
	// Store comes from the app's default ratelimit.MemoryStore at boot;
	// we reference it through the package's default accessor below once
	// the app is up. For a simple demo we build a dedicated store here
	// so the middleware has something to declare against — real apps
	// would pull the app's store via fx.
	defaultStore,
	"adverts.createAdvert",
	ratelimit.Limit{RPM: 30, Burst: 5},
))

// defaultStore is the process-wide store this example uses. Same
// MemoryStore the app's default rate-limit store uses — replacing it
// with a Redis-backed store at boot via nexus.WithRateLimitStore would
// make this line the only thing that changes.
var defaultStore = ratelimit.NewMemoryStore()

var advertsModule = nexus.Module("adverts",
	nexus.ProvideService(NewAdvertsService),
	nexus.AsQuery(NewGetAllAdverts),
	nexus.AsMutation(NewCreateAdvert,
		nexus.GraphMiddleware("auth", "Bearer token validation", AuthMiddleware),
		nexus.GraphMiddleware("permission:ROLE_CREATE_ADVERT", "Requires ROLE_CREATE_ADVERT",
			PermissionMiddleware([]string{"ROLE_CREATE_ADVERT"})),
		createRateLimit,
	),
	// NewListQuestions's signature omits *AdvertsService — the auto-mount
	// routes it to the app's single service automatically.
	nexus.AsQuery(NewListQuestions,
		nexus.GraphMiddleware("auth", "Bearer token validation", AuthMiddleware)),
)
