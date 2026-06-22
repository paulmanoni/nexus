package nexus

import (
	"fmt"

	"github.com/paulmanoni/nexus/di"

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
//	di.Provide(
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
//
// MiddlewareOption also satisfies the top-level Option interface as a
// no-op so callers can flow it through Option-typed variadic slots
// (notably nexus.AsCRUD, which accepts ...Option). The option still
// only takes effect via applyToRest / applyToGql / applyToWS — the
// no-op nexusOption() exists purely for type-system passage.
type MiddlewareOption struct{ mw middleware.Middleware }

// nexusOption satisfies Option. Empty di.Options because this slot is
// for transport-attaching options (RestOption/GqlOption/WSOption);
// middleware doesn't register anything globally on its own.
func (m MiddlewareOption) nexusOption() di.Option { return di.Options() }

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

// checkBundleTransports enforces fail-closed attachment (redesign §5): a
// bundle that declares some transports but not t is misattached, and the
// registration errors at boot rather than silently no-opping — which is the
// auth-bypass footgun the redesign exists to kill. opID identifies the
// endpoint for the diagnostic ("POST /quick", "createAdvert", …).
//
// A bundle that declares NO transports at all (a pure dashboard label with
// no Gin/Graph realization, e.g. a metadata marker) is left alone: it claims
// to protect nothing, so attaching it anywhere is harmless. Only middleware
// that genuinely enforces something on transport X — and is attached to
// transport Y where it would silently not run — is rejected.
func checkBundleTransports(bundles []middleware.Middleware, t middleware.Transport, opID string) error {
	for _, b := range bundles {
		set := middleware.AsHandler(b).Transports()
		if set != 0 && !set.Has(t) {
			return fmt.Errorf(
				"nexus: middleware %q on %s op %q declares Transports = %s and cannot run on %s; "+
					"scope it with nexus.UseOnRest/UseOnGraph/UseOnWS, or give it a %s realization",
				b.Name, t, opID, set, t, t)
		}
	}
	return nil
}
