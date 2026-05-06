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
//	"login"   POST endpoint that exchanges credentials for a token /
//	          session cookie. The SDK calls it via auth.login(creds).
//	"logout"  endpoint that revokes the active session. The SDK calls
//	          it via auth.logout() and clears its locally-stored
//	          token on success.
//	"me"      GET endpoint returning the current Identity. The SDK
//	          calls it via auth.me() to bootstrap an existing session
//	          on page load (cookie-based apps) or check token freshness
//	          (bearer apps).
//
// Apps stay in control of the actual handler — AuthRoute is purely a
// metadata marker. Use it on REST endpoints declared via AsRest:
//
//	nexus.AsRest("POST", "/api/login", NewLogin, nexus.AuthRoute("login"))
//	nexus.AsRest("POST", "/api/logout", NewLogout, nexus.AuthRoute("logout"))
//	nexus.AsRest("GET", "/api/me", NewMe, nexus.AuthRoute("me"))
//
// AuthRoute does not gate access — combine with auth.Optional() (for
// /me on bearer apps) or auth.Required() (for /logout) as needed.
//
// Unknown flow values are accepted but ignored by the SDK. The
// recognized set may grow in future framework versions; the tag is
// the contract.
func AuthRoute(flow string) RestOption {
	return restOptionFn(func(c *restConfig) {
		if c.tags == nil {
			c.tags = map[string]string{}
		}
		c.tags[AuthFlowTag] = flow
	})
}