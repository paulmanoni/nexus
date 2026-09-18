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

// ShareProvide is Share for a provider that needs DI-resolved state: ctor is
// a constructor whose params are injected and whose single return is the
// SharedProvider. The canonical use is an app-wide permission-gates prop,
// where the provider needs the *nexus.App to reach the endpoint registry:
//
//	inertia.ShareProvide(func(app *nexus.App) inertia.SharedProvider {
//	    return func(ctx context.Context) (string, any) {
//	        return "can", auth.OpGates(ctx, app)
//	    }
//	}),
func ShareProvide(ctor any) nexus.Option {
	return nexus.Raw(di.Provide(
		di.Annotate(ctor, di.ResultTags(`group:"inertia.shared"`)),
	))
}

// ShareScoped projects a request-scoped fact (nexus.NewScoped) to every page
// as a shared prop under key — the bridge between the two halves of a named
// request fact: handlers read handle.Get(ctx), pages read props.<key>, and
// the Scoped memo guarantees ONE compute per request no matter how many of
// either ask. The key is declared at registration (not discovered by calling
// a provider), and a failed derivation omits the key from the render — pages
// degrade, while handler-side Gets still surface the error properly.
//
//	var CanGates = nexus.NewScoped[map[string]bool](func(app *nexus.App) nexus.Compute[map[string]bool] {
//	    return func(ctx context.Context) (map[string]bool, error) {
//	        return auth.OpGates(ctx, app), nil
//	    }
//	})
//
//	nexus.Boot(CanGates, inertia.ShareScoped("can", CanGates), ...)
func ShareScoped[T any](key string, s *nexus.Scoped[T]) nexus.Option {
	return Share(func(ctx context.Context) (string, any) {
		v, err := s.Get(ctx)
		if err != nil {
			return "", nil
		}
		return key, v
	})
}
