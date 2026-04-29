package nexus

import (
	"sync"

	"go.uber.org/fx"
)

// pathOption is the marker carrying a module's public URL
// path. nexus.Module() picks it out of its opts list and uses it
// twice: as a RoutePrefix for REST endpoints in the module, and
// to register the module's GraphQL mount path under <path>/graphql.
type pathOption struct{ path string }

func (pathOption) nexusOption() fx.Option { return fx.Options() }

// Path sets the module's public URL path. Equivalent to
// declaring both nexus.RoutePrefix(path) on the module AND
// service.AtGraphQL(path+"/graphql") on the module's service —
// expressed once, kept in sync.
//
//	var Module = nexus.Module("uaa",
//	    nexus.DeployAs("uaa-svc"),
//	    nexus.Path("/oats-uaa"),
//	    nexus.Provide(NewService),
//	    nexus.AsRest("POST", "/oauth/token", TokenHandler),
//	    nexus.AsQuery(NewSearchUsers),
//	)
//
// Effect: REST endpoints mount under /oats-uaa/* and GraphQL
// fields belonging to this module mount at /oats-uaa/graphql.
//
// Why bother (vs a deployment-level prefix in the manifest):
// Path travels with the module — same URL in monolith and split
// deployments. The SPA's calls to /oats-uaa/graphql work in both
// shapes without conditional client logic.
//
// Convention: the module name (first arg of nexus.Module) and
// the *Service name (passed to app.Service in the constructor)
// must match for the GraphQL path override to apply. Path looks
// up app.Service(name) by the module's name; if the service
// uses a different name, declare AtGraphQL explicitly for that
// service instead.
//
// Leading slash is added if missing; trailing slash is trimmed.
func Path(path string) Option {
	return pathOption{path: normalizeRoutePrefix(path)}
}

// modulePublicPath maps module name → public path (e.g. "uaa" →
// "/oats-uaa"). Populated when nexus.Module() encounters a
// PublicPath option among its children. Read by app.Service when
// constructing a Service whose name matches a registered module —
// the service's GraphQL mount path is then derived as
// <publicPath>/graphql instead of the framework default.
//
// Last write wins so a re-import in tests is deterministic.
var (
	modulePublicPathMu sync.RWMutex
	modulePublicPath   = map[string]string{}
)

// registerModulePublicPath stores the (module, path) mapping.
// Empty inputs are no-ops so PublicPath("") doesn't poison the
// registry.
func registerModulePublicPath(module, path string) {
	if module == "" || path == "" {
		return
	}
	modulePublicPathMu.Lock()
	defer modulePublicPathMu.Unlock()
	modulePublicPath[module] = path
}

// modulePublicPathOf returns the registered public path for the
// given module/service name, or "" when none is set. Read by
// (*App).Service to override the new Service's GraphQL path.
func modulePublicPathOf(name string) string {
	modulePublicPathMu.RLock()
	defer modulePublicPathMu.RUnlock()
	return modulePublicPath[name]
}
