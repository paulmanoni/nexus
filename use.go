package nexus

import (
	"github.com/paulmanoni/nexus/middleware"
)

// Use attaches a transport-agnostic middleware bundle to a registration.
// Works on AsRest, AsQuery, AsMutation, (future AsSubscription /
// AsWebSocket) — each transport picks the realization it understands from
// the bundle (Gin for REST/WS upgrade, Graph for GraphQL). Missing fields
// are silently ignored so a single bundle can degrade gracefully across
// transports.
//
//	rl := ratelimit.NewMiddleware(store, key, ratelimit.Limit{RPM: 30})
//	fx.Provide(
//	    nexus.AsMutation(NewCreateAdvert, nexus.Use(rl)),
//	    nexus.AsRest("POST", "/quick", NewQuick, nexus.Use(rl)),
//	)
//
// For app-wide coverage (every REST endpoint + GraphQL POST + WS upgrade
// + the dashboard itself) put the middleware in Config.GlobalMiddleware
// instead of naming it on each registration.
func Use(m middleware.Middleware) MiddlewareOption {
	return MiddlewareOption{mw: m}
}

// MiddlewareOption carries a Middleware across the AsRest/AsQuery/... call
// sites. Each transport's option type embeds / converts this, so a single
// nexus.Use(...) expression can appear wherever the transport accepts it.
type MiddlewareOption struct{ mw middleware.Middleware }

// applyToGql wires this middleware into a GraphQL registration. Called
// by asGqlField for each MiddlewareOption passed to AsQuery/AsMutation.
// Leaves the GqlOption slice untouched when the bundle has no Graph
// realization (e.g. a gin-only rate limit); the registry still records
// the name so the dashboard's middleware list stays accurate.
func (m MiddlewareOption) applyToGql(c *gqlConfig) {
	info := m.mw.AsInfo()
	if m.mw.Graph != nil {
		c.middlewares = append(c.middlewares, namedMw{
			name:        info.Name,
			description: info.Description,
			mw:          m.mw.Graph,
		})
	}
	c.bundles = append(c.bundles, m.mw)
}

// applyToRest wires this middleware into a REST registration. Same
// fallback rule as applyToGql — skip the handler slot if Gin is nil, but
// always record the name for the dashboard.
func (m MiddlewareOption) applyToRest(c *restConfig) {
	c.bundles = append(c.bundles, m.mw)
}

// applyToWS wires this middleware into an AsWS registration. Only the
// first AsWS call for a given path actually installs middleware on the
// upgrade route — subsequent registrations' bundles are ignored (with a
// warning log).
func (m MiddlewareOption) applyToWS(c *wsConfig) {
	c.bundles = append(c.bundles, m.mw)
}
