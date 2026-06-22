package nexus

import (
	"github.com/paulmanoni/nexus/middleware"
)

// EndpointGate is a middleware the framework applies to every endpoint by
// default — the primitive behind deny-by-default auth. An extension supplies
// one (e.g. auth's Authorization.Default = Authenticated()) and the framework
// prepends it to each endpoint's chain across REST, GraphQL, and WS unless
// the endpoint opts out with Public(). It lives here, not in the auth
// extension, so package nexus can honor the policy without importing the
// extension that configures it (auth imports nexus, never the reverse).
type EndpointGate struct {
	// Middleware is the gate. It should declare AllTransports so the same
	// value gates every transport; a transport it doesn't implement is
	// simply not gated.
	Middleware middleware.Middleware
}

// applyDefaultGate stashes a supplied EndpointGate on the app before any
// endpoint mounts. Registered in fxEarlyOptions so it runs ahead of the
// per-endpoint fx.Invokes (fx runs invokes in registration order, and the
// supplied gate is resolved from the graph regardless of where the
// extension that supplied it sits in the option list). The gate is
// optional: apps without deny-by-default supply nothing and pay nothing.
// The *EndpointGate parameter is optional — wired via di.ParamTags
// (`optional:"true"`) on the applyDefaultGate invoke in fxEarlyOptions — so
// apps that supply no gate resolve it to nil and pay nothing.
func applyDefaultGate(app *App, gate *EndpointGate) {
	app.defaultGate = gate
}

// gateBundles prepends the default endpoint gate ahead of an endpoint's own
// bundles, unless no gate is configured or the endpoint is exempt
// (Public() / a login flow). Used by the REST and WS chain builders, which
// share buildEndpointChain.
func (a *App) gateBundles(tags map[string]string, bundles []middleware.Middleware) []middleware.Middleware {
	if a.defaultGate == nil || isPublicEndpoint(tags) {
		return bundles
	}
	out := make([]middleware.Middleware, 0, len(bundles)+1)
	out = append(out, a.defaultGate.Middleware)
	return append(out, bundles...)
}

// defaultGraphGate returns the gate's GraphQL realization to prepend onto a
// field's middleware chain, or nil when no gate applies (none configured,
// endpoint exempt, or the gate has no GraphQL realization).
func (a *App) defaultGraphGate(tags map[string]string) *middleware.Middleware {
	if a.defaultGate == nil || isPublicEndpoint(tags) || a.defaultGate.Middleware.Graph == nil {
		return nil
	}
	return &a.defaultGate.Middleware
}
