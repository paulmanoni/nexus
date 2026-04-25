package nexus

import (
	"reflect"

	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/registry"
)

// baseEndpointConfig holds the fields every transport-specific config
// (gqlConfig, restConfig, wsConfig) shares: dashboard description, the
// enclosing module name + deployment tag, and the cross-transport
// middleware bundles attached via nexus.Use.
//
// Transport configs embed it so adding a new shared field — a tag map,
// an audit hook, etc. — is a one-line change instead of three. The
// setModule/setDeployment methods below also satisfy the
// moduleAnnotator + deploymentAnnotator interfaces for free, so each
// transport's option struct only needs to delegate.
type baseEndpointConfig struct {
	// description is the human-readable string shown on the dashboard
	// and (where the transport supports it) in generated SDL.
	description string

	// module is stamped by nexus.Module("name", ...) when this option
	// is a direct child of a module. Populates the registry entry's
	// Module field so the dashboard groups endpoints by module.
	module string

	// deployment is stamped by nexus.DeployAs in the enclosing module.
	// Empty for always-local endpoints; populated only when the parent
	// Module declares a deployment tag.
	deployment string

	// bundles holds the full middleware.Middleware values attached via
	// nexus.Use — the registry uses AsInfo() from each to label the
	// endpoint's middleware list. Per-transport realizations (Gin,
	// Graph) are extracted at apply time; this slice is the canonical
	// metadata source for the dashboard.
	bundles []middleware.Middleware
}

func (b *baseEndpointConfig) setModule(name string)    { b.module = name }
func (b *baseEndpointConfig) setDeployment(tag string) { b.deployment = tag }

// resolveEndpointService picks the service name a REST or WebSocket
// endpoint registers under. Priority:
//
//  1. explicit — set via a per-endpoint option (reserved for future use).
//  2. The first *Service-wrapper dep in the handler's deps — same
//     convention AsQuery / AsMutation use, just resolved into a name
//     instead of a value-group key.
//  3. module — the enclosing nexus.Module name. Catches the common
//     "REST/WS handler has no service-wrapper dep" case so metrics
//     events still carry a non-empty service.
//  4. defaultServiceName — ultimate fallback for handlers outside any
//     module. Registers the default service on the app so the
//     registry stays consistent.
//
// AsQuery / AsMutation route by service *type* via the fx value-group
// (see asGqlField), not by name, so they don't go through this helper.
func resolveEndpointService(explicit, module string, deps []reflect.Value, depTypes []reflect.Type, app *App) string {
	if explicit != "" {
		return explicit
	}
	if svc := serviceNameFromDeps(deps, depTypes); svc != "" {
		return svc
	}
	if module != "" {
		app.registry.RegisterService(registry.Service{Name: module})
		return module
	}
	app.Service(defaultServiceName)
	return defaultServiceName
}