package inertia

import (
	"context"

	"github.com/paulmanoni/nexus/di"

	"github.com/paulmanoni/nexus"
)

// SharedProvider contributes a prop to EVERY Inertia page response. It runs
// per request with the request context (so it can read the authenticated
// user, request-scoped values, etc.) and returns a single key/value pair. A
// provider that returns an empty key is skipped, letting it opt out
// conditionally.
//
//	func SharedAuth(ctx context.Context) (string, any) {
//	    u, _ := auth.User[Me](ctx)
//	    return "auth", map[string]any{"user": u}
//	}
//
// Shared props follow the same inclusion rules as plain props: present on full
// visits, and on a partial reload only when requested.
type SharedProvider func(ctx context.Context) (key string, value any)

// Share registers a SharedProvider. Add one or more to a module alongside
// inertia.Module; the engine collects them via an fx value group and runs each
// on every page render.
//
//	nexus.Module("web",
//	    inertia.Module(inertia.Config{Frontend: webFS, Root: "web/dist"}),
//	    inertia.Share(SharedAuth),
//	    inertia.Share(SharedFlash),
//	)
func Share(fn SharedProvider) nexus.Option {
	return nexus.Raw(di.Provide(
		di.Annotate(
			func() SharedProvider { return fn },
			di.ResultTags(`group:"inertia.shared"`),
		),
	))
}
