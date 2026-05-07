// Package oauth2 wires a go-oauth2/oauth2/v4 server into a nexus app
// and bridges its access-token store to nexus.auth so handlers can
// gate themselves with auth.Required() / auth.Requires().
//
// Minimum config — password grant against a user store:
//
//	nexus.Run(
//	    nexus.Config{Server: nexus.ServerConfig{Addr: ":8080"}},
//	    oauth2.Module(oauth2.Config{
//	        Authenticator: func(ctx context.Context, clientID, username, password string) (string, error) {
//	            user, err := users.Authenticate(ctx, username, password)
//	            if err != nil { return "", err }
//	            return strconv.Itoa(int(user.ID)), nil
//	        },
//	    }),
//	)
//
// Defaults are conservative: in-memory token store, anonymous client
// only, no JTI, "Bearer" token type, 5-minute identity cache.
// Production apps replace TokenStore (Redis-backed via
// NewCacheTokenStore), ClientStore (DB-backed via
// NewLoaderClientStore), and frequently set IdentityResolver to
// populate Identity.Roles / .Extra.
//
// The package owns:
//
//   - POST {Config.TokenPath}    OAuth2 token endpoint
//   - POST {Config.RevokePath}   token revocation (when set)
//   - bridge from token-store ↔ auth.Module
//
// Three-legged authorization-code flow is reachable via
// Config.ServerCustomizer (set SetUserAuthorizationHandler and add
// the route yourself); the package doesn't mount it by default
// because most apps using this run password / client_credentials.
package oauth2