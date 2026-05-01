package users

import (
	"context"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/ratelimit"
)

// listRateLimit caps GET /users at 60 RPM with a burst of 10. Bundled
// once via ratelimit.NewMiddleware and attached per-op via nexus.Use.
// Surfaces in the dashboard as a per-op middleware chip and as a row
// in the Rate limits tab.
var listRateLimit = nexus.Use(ratelimit.NewMiddleware(
	ratelimit.NewMemoryStore(),
	"users.GET /users",
	ratelimit.Limit{RPM: 60, Burst: 10},
))

// Module is the wired declaration. Demonstrates the breadth of the
// dashboard surface in one place:
//
//   - Provide(NewService, NewCache): the cache implements
//     NexusResources(), so the framework auto-registers it as a
//     resource node and attaches it to the service via constructor.
//   - REST + GraphQL query + GraphQL mutation: three transport
//     flavours land in the same module card.
//   - Use(rate-limit) on the list endpoint: middleware chip + Rate
//     limits tab entry.
//   - AsWorker: a long-lived background worker shows up as a Worker
//     node connected to the cache.
//   - Invoke(app.Cron): a cron job appears in the Crons tab.
//
// DeployAs is inferred from nexus.deploy.yaml's `users-svc.owns:
// [users]` entry. Add an explicit nexus.DeployAs("users-svc") to pin
// it in source.
var Module = nexus.Module("users",
	nexus.DeployAs("users-svc"),
	nexus.Provide(NewService, NewCache),
	nexus.AsRest("GET", "/users/:id", NewGet),
	nexus.AsRest("GET", "/users", NewList, listRateLimit),
	// /boom is a demo-only endpoint that always panics. Exercises
	// the framework's stack-capture path so the dashboard's drawer
	// "Last error" + activity rail show a real Go stack trace under
	// a "▸ stack" disclosure. Not something a real handler would do.
	nexus.AsRest("GET", "/boom", NewBoom),
	nexus.AsQuery(NewSearch),
	nexus.AsMutation(NewCreate),
	nexus.AsWorker("user-counter", NewCounter),
	nexus.Invoke(func(app *nexus.App) {
		app.Cron("cache-warmup", "@every 1m").
			Describe("Top up the hottest user IDs into the cache.").
			Handler(func(ctx context.Context) error { return nil })
	}),
)
