package nexus

// AuthFlowTag is the registry.Endpoint.Tags key used by AuthRoute to
// mark which framework auth flow ("login" | "logout" | "me") an
// endpoint belongs to. The client SDK reads this tag to expose
// top-level auth.login() / auth.logout() / auth.me() calls so users
// don't have to know the underlying route shape — the framework
// surfaces it via convention without owning the handlers themselves.
const AuthFlowTag = "auth.flow"

// AuthRoute marks the endpoint as part of one of the framework-aware
// auth flows. Three values are recognized today:
//
//	"login"   endpoint that exchanges credentials for a token /
//	          session cookie. The SDK calls it via auth.login(creds).
//	"logout"  endpoint that revokes the active session. The SDK calls
//	          it via auth.logout() and clears its locally-stored
//	          token on success.
//	"me"      endpoint returning the current Identity. The SDK calls
//	          it via auth.me() to bootstrap an existing session on
//	          page load (cookie-based apps) or check token freshness
//	          (bearer apps).
//
// Apps stay in control of the actual handler — AuthRoute is purely a
// metadata marker. Works on both REST (AsRest) and GraphQL ops
// (AsQuery, AsMutation). The SDK auto-dispatches to the right
// transport based on the manifest's recorded transport tag:
//
//	nexus.AsRest("POST", "/api/login", NewLogin, nexus.AuthRoute("login"))
//	nexus.AsMutation(NewLogin, nexus.AuthRoute("login"))
//	nexus.AsQuery(NewMe, nexus.AuthRoute("me"))
//
// AuthRoute does not gate access — combine with auth.Optional() (for
// /me on bearer apps) or auth.Required() (for /logout) as needed.
//
// Unknown flow values are accepted but ignored by the SDK. The
// recognized set may grow in future framework versions; the tag is
// the contract.
func AuthRoute(flow string) AuthRouteOption {
	return AuthRouteOption{flow: flow}
}

// AuthRouteOption is the cross-transport carrier returned by
// AuthRoute. Implements both RestOption and GqlOption so the same
// expression can flow through AsRest, AsQuery, or AsMutation
// without callers caring which transport the endpoint lives on.
//
// Mirrors the pattern in nexus.Use / MiddlewareOption: a single
// concrete value satisfying multiple per-transport apply
// interfaces, with each transport reading what it needs.
type AuthRouteOption struct{ flow string }

func (a AuthRouteOption) applyToRest(c *restConfig) {
	if c.tags == nil {
		c.tags = map[string]string{}
	}
	c.tags[AuthFlowTag] = a.flow
}

func (a AuthRouteOption) applyToGql(c *gqlConfig) {
	if c.tags == nil {
		c.tags = map[string]string{}
	}
	c.tags[AuthFlowTag] = a.flow
}