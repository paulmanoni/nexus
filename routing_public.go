package nexus

// PublicTag is the registry.Endpoint.Tags key set by Public(). When an
// extension installs a default endpoint gate (deny-by-default auth),
// endpoints carrying this tag are exempted from it.
const PublicTag = "auth.public"

// Public marks an endpoint as exempt from any framework-installed default
// gate — the explicit opt-out for deny-by-default auth
// (auth.Authorization.Default). With no default gate configured it's a
// harmless no-op marker. Works on REST (AsRest / AsRestHandler), GraphQL
// (AsQuery / AsMutation), and WS (AsWS):
//
//	nexus.AsRest("GET", "/health", NewHealth, nexus.Public())
//
// Login endpoints marked nexus.AuthRoute("login") are auto-exempted (you
// can't require auth to obtain auth), so they need no Public().
func Public() PublicOption { return PublicOption{} }

// PublicOption is the cross-transport carrier returned by Public —
// implements RestOption, GqlOption, and WSOption so one expression flows
// through any endpoint builder, mirroring AuthRouteOption / MiddlewareOption.
type PublicOption struct{}

func (PublicOption) applyToRest(c *restConfig) { tagPublic(&c.tags) }
func (PublicOption) applyToGql(c *gqlConfig)   { tagPublic(&c.tags) }
func (PublicOption) applyToWS(c *wsConfig)     { tagPublic(&c.tags) }

func tagPublic(tags *map[string]string) {
	if *tags == nil {
		*tags = map[string]string{}
	}
	(*tags)[PublicTag] = "true"
}

// isPublicEndpoint reports whether an endpoint is exempt from the default
// gate: either explicitly via Public() or implicitly because it's a
// framework login flow (AuthRoute("login")) — gating those would make
// authentication impossible.
func isPublicEndpoint(tags map[string]string) bool {
	return tags[PublicTag] == "true" || tags[AuthFlowTag] == "login"
}
