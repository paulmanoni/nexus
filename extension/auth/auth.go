// Package auth is nexus's authentication and authorization: accounts the app
// describes with one interface, credentials nexus issues and checks
// (sessions, bearer tokens, API keys, JWTs from elsewhere), sign-in flows,
// and gates that work the same on REST, GraphQL, WebSocket, Inertia pages
// and views.
//
//	nexus.Boot(auth.Module(auth.Config{Users: auth.UseUsers(NewUsers)}), …)
//
//	id, err := auth.Login(ctx, auth.Password{Login: in.Email, Password: in.Password})
//	cred, err := auth.SignIn(ctx, id)
//
//	nexus.AsMutation((*Orders).Refund, auth.Kind("staff"), auth.Requires("orders.refund"))
//
// Settings come from nexus.toml's [auth] table (Config.Settings in Go):
// schemes, areas, endpoints, session and throttle rules. Every endpoint
// needs a sign-in unless it is auth.Public(). `nexus docs auth`,
// docs/guide/auth.md.
package auth

import (
	"context"
	"fmt"
	"net/http"
	"reflect"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/client"
	"github.com/paulmanoni/nexus/v2/di"
	"github.com/paulmanoni/nexus/v2/extension"
	"github.com/paulmanoni/nexus/v2/extension/dashboard"
	"github.com/paulmanoni/nexus/v2/trace"
)

// Identity is who a request is: the same on every transport, read with
// auth.Current(ctx).
type Identity struct {
	ID string
	// Kind is the user's kind — "staff", "customer" — or "" when the app
	// has one. auth.Kind and areas gate on it.
	Kind string
	// Perms are the identity's permissions, matched with wildcards:
	// "orders.*" grants "orders.view" and "orders.refunds.create", "*"
	// grants everything. Roles are an app concept: Users.Load expands them.
	Perms []string
	// User is the app's user, read with auth.User[T]. It is never sent to
	// a browser unless Users' Public method puts it there.
	User any
	// Actor is the real user while they impersonate this one
	// (auth.Impersonate); nil otherwise. Gates evaluate the identity, not
	// the actor.
	Actor *Identity
	// Scheme names the [auth.schemes] entry that authenticated the
	// request; nexus sets it.
	Scheme string
}

// Has reports whether the identity holds perm, wildcards applied.
func (i *Identity) Has(perm string) bool { return i != nil && i.grants(perm) }

// ErrUnauthenticated is the error of a request that needs a sign-in.
var ErrUnauthenticated error = nexus.Err(nexus.Unauthenticated, "auth: unauthenticated")

// ErrForbidden is the error of a signed-in request a gate refuses.
var ErrForbidden error = nexus.Err(nexus.Forbidden, "auth: forbidden")

// Config wires auth.Module. Only Users is required; everything else has a
// nexus.toml key or a default.
type Config struct {
	// Users is the app's account lookup: auth.UseUsers(NewUsers) for a DI
	// constructor, auth.StaticUsers(u) for a value.
	Users UsersOption

	// Settings is [auth] in Go; nil reads nexus.toml's [auth] table.
	Settings *Settings

	// Tokens stores issued bearer tokens, API keys and session records
	// (hashed). Nil keeps them in memory — lost on restart, not shared
	// between replicas; set CacheTokens(cache) in production.
	Tokens TokenStore

	// Clients finds OAuth2 clients the token endpoint authenticates (a
	// database, say); [auth.oauth2.clients.*] are consulted after it.
	Clients Clients

	// Throttle keeps the [auth.throttle] failure counts. Nil counts in the
	// process (each replica on its own); CacheThrottle(cache) shares them.
	Throttle ThrottleStore
}

// moduleState is one app's auth: its config, schemes and stores.
type moduleState struct {
	cfg     Config
	config  configPath
	schemes []boundScheme // tried in order
	bus     *trace.Bus
	app     *nexus.App
}

// boundScheme is one [auth.schemes] entry ready to run: where its
// credential is read, and how it becomes an identity.
type boundScheme struct {
	name    string
	typ     string
	extract extractor
	resolve resolver
}

// resolver turns a scheme's credential into an identity.
type resolver func(ctx context.Context, credential string) (*Identity, error)

// Module enables auth for the app.
func Module(cfg Config) nexus.Option {
	if !cfg.Users.set {
		return nexus.Raw(di.Error(fmt.Errorf("auth: Config.Users is required — auth.UseUsers(NewUsers) or auth.StaticUsers(u)")))
	}
	state := &moduleState{cfg: cfg}
	state.config.tokens = cfg.Tokens
	users, err := usersOption(state, cfg.Users)
	if err != nil {
		return nexus.Raw(di.Error(fmt.Errorf("auth: %w", err)))
	}

	dashboard.RegisterSnapshotExtra("auth", func() any {
		return map[string]any{"setup": state.dashboardSetup()}
	})
	dashboard.RegisterPageData("auth", state.dashboardSessions)

	return extension.Use(extension.Plugin{
		Name:    "auth",
		Version: "2",
		Options: []nexus.Option{
			users,
			// Every endpoint needs a sign-in unless it is Public (or
			// [auth] default = "public"); one under an area, a kind it admits.
			nexus.Raw(di.Supply(&nexus.EndpointGate{Middleware: state.defaultGate()})),
			nexus.Defer(func() nexus.Option {
				if err := state.resolveConfig(); err != nil {
					return nexus.FailBoot(fmt.Errorf("auth: %w", err))
				}
				return state.endpointOptions()
			}),
			nexus.Invoke(func(app *nexus.App) error {
				state.bus, state.app = app.Bus(), app
				app.SetValue(stateKey{}, state)
				if err := state.installConfigPath(app); err != nil {
					return fmt.Errorf("auth: %w", err)
				}
				if p := state.config.settings.pageProp; p != "" {
					// client.d.ts types the prop: NexusSharedProps["auth"].
					app.RegisterSharedProp(p, reflect.TypeFor[MeResponse]())
				}
				app.Router().Use(authMiddleware(state))
				return nil
			}),
			// Once every route is registered: a sign-in or forbidden page that
			// no route serves sends visitors to a 404.
			nexus.Setup(func(app *nexus.App) { state.warnMissingPages(app) }),
		},
		Dashboard: &extension.Dashboard{
			Tab: &extension.Tab{ID: "auth", Label: "Auth"},
			Routes: []extension.Route{
				{Method: "POST", Path: "/unlock", Handler: dashboardUnlockHandler(state)},
				{Method: "POST", Path: "/revoke-user", Handler: dashboardRevokeUserHandler(state)},
				{Method: "POST", Path: "/revoke-session", Handler: dashboardRevokeSessionHandler(state)},
			},
			LiveEvents: []string{"auth.reject"},
		},
		Client: &extension.Client{
			Namespace: "auth",
			Apply: func(app *nexus.App) error {
				// Where the SDK puts a credential, and where it finds the
				// token in a sign-in response.
				app.SetClientAuthInfo(state.clientInfo)
				app.SetClientAuthMeta(client.AuthMeta{TokenField: "access_token"}.WithDefaults())
				return nil
			},
		},
		Contributor: authContributor{},
	})
}

// authenticateScheme runs the first scheme whose credential the request
// carries; it names that scheme and the credential ("" when none).
func (st *moduleState) authenticateScheme(ctx context.Context, r *http.Request) (*Identity, string, string, error) {
	for i := range st.schemes {
		tok, ok := st.schemes[i].extract.extract(r)
		if !ok {
			continue
		}
		id, err := st.schemes[i].resolve(ctx, tok)
		return id, st.schemes[i].name, tok, err
	}
	return nil, "", "", nil
}

// clientInfo tells the client SDK where credentials go: the schemes, in
// the order they are tried.
func (st *moduleState) clientInfo() client.ExtractorInfo {
	var chain []client.ExtractorInfo
	for _, sc := range st.schemes {
		switch sc.typ {
		case SchemeSession:
			chain = append(chain, client.ExtractorInfo{Strategy: "cookie"})
		case SchemeBearer, SchemeJWT:
			chain = append(chain, client.ExtractorInfo{Strategy: "bearer", HeaderName: "Authorization"})
		case SchemeAPIKey:
			if s, ok := st.config.settings.scheme(sc.name); ok {
				chain = append(chain, client.ExtractorInfo{Strategy: "header", HeaderName: s.Header})
			}
		}
	}
	switch len(chain) {
	case 0:
		return client.ExtractorInfo{Strategy: "bearer", HeaderName: "Authorization"}
	case 1:
		return chain[0]
	}
	return client.ExtractorInfo{Strategy: "chain", Chain: chain}
}
