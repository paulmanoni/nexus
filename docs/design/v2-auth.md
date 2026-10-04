# nexus 2.0 — authentication

Status: **built** (2.1–2.9, 2026-10-04). It first shipped additively — see
[Shipping in 2.x](#shipping-in-2-x) — and in 2.9 the v1 API was removed, nothing
depending on it yet. Proposed 2026-10-03. Companion to [v2.md](v2.md); it follows the same
principles (one way, same behaviour on every transport, stdlib at the edges, safe
by default, migration is a command). Nothing here is built. Each section ends with
its decision.

## Diagnosis

Measured on v1.78 (`extension/auth`, `extension/oauth2`, `extension/session`,
`extension/security`) and on a production app's auth module.

### Too many concepts, overlapping

`extension/auth` alone exports ~90 names (`go doc -short ./extension/auth`). A
reader has to learn all of these to wire one login:

- **Two things called "backend".** `auth.Backend` is an interface with
  `Authenticate`/`GetUser` (backend.go:32-44). `Config.Backend` is a
  `BackendOption` whose value is discovered by type assertion for `Resolve`,
  `Login`, `Authorize`, `Issue`, `RevokeToken` and `TokenHandler`
  (config_backend.go:57-65, endpoints.go:53-68). Its `Login` is not
  `Backend.Authenticate`; `Manager.Login` silently returns
  `ErrInvalidCredentials` when the capability is missing (auth.go:393-398).
  Capabilities are invisible to the compiler: a typo in a method name turns a
  feature off.
- **Three permission knobs with precedence rules.** `Authorization.Authority`,
  `Authorization.Permissions` (overrides Authority) and `Backend.Authorize`
  (overrides both) (authorization.go:59-99, config_backend.go:94-96). `AnyOf` and
  `AllOf` ignore the permissions named at the gate (permissions.go:16-21, 48-51), so
  `auth.Requires("x")` under `AnyOf("admin")` checks `admin`, not `x`.
- **Two permission buckets.** `Identity.Roles` and `Identity.Scopes` are matched
  identically (auth.go:84-98, authorization.go:103-115); apps use one for
  permissions and the other for flags (`is_superuser`, `is_staff`).
- **Five ways to mount login.** `Config.Endpoints` (endpoints.go:26), the
  deprecated `LoginEndpoint`/`LogoutEndpoint`, raw `LoginHandler`/`LogoutHandler`,
  `oauth2.Config.LoginPath`/`LogoutPath` (oauth2.go:108-112), and hand-written
  handlers tagged `nexus.AuthRoute("login")` (routing_auth.go:39).
- **Two identity mechanisms.** `auth.Module` and the older per-service
  `UserDetailsFn` for GraphQL (routing_service.go:30-37).
- **Three cookies with separate settings.** `auth.SessionCookie` (cookie.go:33),
  `session.Config.CookieName` (session.go:73), and the CSRF pair, whose names are
  set twice: `auth.Config.CSRFCookie`/`CSRFHeader` for the SDK (auth.go:236-237)
  and `secure.CSRFConfig.CookieName`/`HeaderName` for the middleware
  (secure.go:86-87).
- **Two password paths.** `auth.Hashers` (hasher.go:48) and
  `oauth2.VerifyBcrypt`/`VerifySpringPassword` (passwords.go:21, 44), which don't
  share formats or rehash-on-login.
- **Third-party types in the API.** `oauth2.Config.ClientStore`/`TokenStore`
  take go-oauth2 interfaces (oauth2.go:57, 62); `IdentityResolver` takes an
  `oauth2lib.TokenInfo` (oauth2.go:36).

### What apps re-implement

The production app's auth module is **3,208 lines** of non-test Go on top of
`extension/auth`. Most of it is framework work:

| App file | Lines | What it re-implements |
|---|---|---|
| `next.go` | 278 | `?next=` after login, with an open-redirect validator (decode loop, backslashes, protocol-relative) |
| `auth.go` | 230 | per-area login URL (`iauth.ErrorHandler` takes one fixed URL and no `next`, iauth.go:50); Inertia 409/302 branching copied from iauth; a forbidden page for page visits |
| `audience.go` | 268 | staff vs customer separation: a path-prefix gate installed with `app.Router().Use` from an `Invoke` whose position must follow `auth.Module` |
| `throttle.go` | 270 | login brute-force limits per account, per IP, global, with lockout |
| `clientip.go` | 53 | client IP for the throttle |
| `sessions.go` | 117 | "a new sign-in ends older sessions" via per-user cutoffs |
| `resolver.go` | 271 | token → identity, plus "token predates password change" checks |
| `server.go`, `token_store_redis.go`, `token_generate.go` | 702 | an OAuth2 server, a Redis token store and a crypto/rand token generator replacing the library's default |
| `mobile_auth.go` | 196 | a second token format (JWT) beside the opaque tokens, told apart by "has dots" |
| `perms.go` | 102 | superuser → all permissions; permission map shared to every page |

Also missing: impersonation ("switch account" is still served by the legacy
system the app is replacing), a test helper (tests write a stub backend:
`pages_test.go:35-87`), and gates the path prefix can't express (the shared root
`/graphql` mount serves both user kinds, so the audience gate can't cover it —
`audience.go:28-32`).

### Not the same on every transport

- **Bad credentials look like no credentials.** The global middleware swallows a
  resolve error and continues anonymously (middleware.go:21-23, 28-33); `Required`
  then answers "unauthenticated" with no reason. "Token expired", "token revoked"
  and "no cookie sent" are indistinguishable even in development.
- **WebSocket identity is frozen at the upgrade** (ws_identity.go:64-69): a
  revoked token keeps working on an open connection. Live view sockets don't use
  the carriers at all — they keep the whole upgrade request context
  (view/live.go:466-474).
- **Jobs get a string.** `jobs.Run` records the enqueuer's id
  (`extension/jobs/record.go:154`); there is no identity or permission check at
  execution time.
- **401/403 rendering is an app concern.** `ErrorHandler` (errorhandler.go:28)
  has to be implemented per app to get redirects, envelopes and forbidden pages.

### Defaults

- **Permit by default** (authorization.go:44-48): an endpoint without a gate is
  public. Deny-by-default is an opt-in `Authorization.Default: Authenticated()`.
- **No login throttle, no session rotation on login, no `next`** out of the box.
- **The identity cache is off unless configured** (auth.go:438), so every request
  re-resolves; when on, it is keyed by the raw token and revocation is manual
  (`Manager.Invalidate`).

**Decision:** v2 auth is rebuilt around a small set of concepts, each with one
API, configured in nexus.toml, with the work apps re-implement moved into the
framework.

---

## 1. Concepts

Six, and nothing else:

| Concept | What it is | Declared in |
|---|---|---|
| **Scheme** | how a credential arrives: `session`, `bearer`, `apikey`, `jwt` | `[auth.schemes.*]` |
| **Users** | the app's one interface: find an account, load it by id | Go, DI-provided |
| **Identity** | who the request is: id, kind, perms, the app's user | returned by Users |
| **Gate** | what an endpoint/page/view requires | `//nexus:auth`, `auth.Required()`… |
| **Area** | a path prefix that belongs to some user kinds, with its own login page | `[auth.areas.*]` |
| **Policy** | a per-object rule (`may this user edit this order?`) | Go, `auth.Policy` |

Everything else — login, logout, tokens, hashing, throttling, `next`,
impersonation, CSRF — is built on these and configured, not implemented.

```go
nexus.Boot(
	auth.Module,                 // one value; settings come from [auth]
	nexus.Provide(NewUsers),     // implements auth.Users
	…
)
```

`auth.Module` is a value, not `auth.Module(auth.Config{…})`: every setting has a
nexus.toml key. `auth.Configure(func(*auth.Config))` exists for the few settings
that are Go values (custom hashers, policies) and for tests.

**Decision:** six concepts. `Config` structs, `Single`, `Backend`/`BackendOption`,
`Scheme{Extract, Resolve}`, `Authentication`, `Authorization`, `Authority`,
`PermissionFn`, `Manager` and `ErrorHandler` are removed from the public API.

## 2. Identity

```go
type Identity struct {
	ID     string
	Kind   string     // "staff", "customer"; "" when the app has one kind
	Perms  []string   // "orders.view", "orders.*", "*"
	User   any        // the app's user, read with auth.User[T]
	Actor  *Identity  // the real user while impersonating (section 10)
	Scheme string     // which scheme authenticated the request; set by nexus
}
```

- **One permission bucket.** `Roles` and `Scopes` merge into `Perms`. Roles are an
  app concept: `Users.Load` expands them into permissions (a role's display name
  belongs in `User`).
- **Wildcards are the default matching rule.** `orders.*` grants
  `orders.view` and `orders.refunds.create`; `*` grants everything, which is how a
  superuser is expressed. v1's opt-in `Wildcard()` (authorization.go:26) becomes
  the only rule.
- **Kind** is a first-class field, so "staff vs customers" is data on the
  identity, not a scope string matched by path prefix.

Reading it is the same on every transport — REST, GraphQL, WebSocket messages,
Inertia pages, templ views, live view events, and jobs:

```go
me := auth.Current(ctx)                 // *Identity, nil when anonymous
u, ok := auth.User[User](ctx)           // the app's user
id, ok := auth.ID[int64](ctx)           // the id parsed to the app's key type
```

**Decision:** adopt. `IdentityFrom` → `Current`; `Subject` → `ID`; `SubjectPtr`
and the `Principal` unwrapping interface are removed (`User` holds the user
directly; what a page shows comes from section 11).

## 3. Users — resolving identity with DI deps

The app implements one interface, constructed by DI like any service:

```go
type Users interface {
	// FindLogin returns the account a sign-in names (username or email) and its
	// encoded password; (nil, "", nil) when there is none.
	FindLogin(ctx context.Context, login string) (*Identity, string, error)
	// Load rebuilds an identity from its id — used for sessions, tokens,
	// impersonation and jobs. (nil, nil) when the account is gone.
	Load(ctx context.Context, id string) (*Identity, error)
}
```

Two optional interfaces, checked **at boot** with an error naming the method
(not discovered silently per request):

```go
type PasswordSetter interface { SetPassword(ctx context.Context, id, encoded string) error } // rehash-on-login
type LoginChecker  interface { CheckLogin(ctx context.Context, id *Identity) error }          // disabled account, expired password
```

```go
type Users struct{ db *MainDB }

func NewUsers(db *MainDB) *Users { return &Users{db: db} }

func (u *Users) Load(ctx context.Context, id string) (*auth.Identity, error) {
	var row User
	if err := u.db.First(&row, "id = ?", id).Error; err != nil {
		return nil, ignoreNotFound(err)
	}
	return &auth.Identity{ID: id, Kind: row.Kind, Perms: u.perms(ctx, row), User: &row}, nil
}

func (u *Users) CheckLogin(ctx context.Context, id *auth.Identity) error {
	if user := id.User.(*User); user.Disabled {
		return nexus.Err(nexus.Forbidden, "this account is disabled")
	}
	return nil
}
```

`Load` results are cached per id (`[auth] cache = "5m"`, on by default) and the
cache is invalidated by every operation that changes what `Load` returns through
nexus: sign-out, password change, `auth.Refresh(ctx, id)` after a role change.
Keying by id instead of by raw token (auth.go:608) means one user with five
sessions costs one load, and no raw token is held in memory.

**Decision:** `Users` replaces `Resolver`, the backend capabilities,
`UserStore`/`ModelBackend` and `oauth2.PasswordAuthenticator`/`IdentityResolver`.
An app that verifies an external token implements a `jwt` or custom scheme
(section 4), not a resolver.

## 4. Schemes — how requests are authenticated

```toml
[auth.schemes.web]
type   = "session"          # cookie → server-side session → user id
cookie = "sid"

[auth.schemes.api]
type    = "bearer"          # opaque tokens issued by nexus
ttl     = "2h"
refresh = "720h"

[auth.schemes.partners]
type   = "apikey"
header = "X-API-Key"        # keys issued and revoked through auth.Keys

[auth.schemes.mobile]
type     = "jwt"            # verify only; nexus does not issue these
secret   = "${MOBILE_JWT_SECRET}"   # or jwks = "https://…/.well-known/jwks.json"
issuer   = "https://id.example.com"
audience = "orders-mobile"
subject  = "sub"            # claim → Users.Load(id)
```

- **Tried in order; the first scheme that finds a credential owns the request**
  (as v1, auth.go:676-686). A credential that is present but invalid is an
  **error**, not anonymity: the request is `Unauthenticated` with a reason
  (`expired`, `revoked`, `superseded`, `malformed`), shown in development and in
  the trace, generic in production.
- **Built-in tokens are stored hashed** (SHA-256 of the token) in the token
  store — `[auth.tokens] store = "cache" | "db" | "memory"`; `cache` uses the
  default cache, so Redis when linked. A leaked store dump is not a set of live
  credentials (the production app documents this exact risk in `server.go`).
  Tokens are 256-bit `crypto/rand` values.
- **`session` uses `extension/session`** — one cookie, the session id; the
  session holds the user id and an epoch (section 7). `auth.SessionCookie` is
  removed.
- **Custom formats** are a scheme too, for credentials nexus can't parse:

```go
auth.Scheme("legacy", func(r *http.Request) (string, bool) { … },   // extract
	func(ctx context.Context, raw string) (string, error) { … })      // verify → user id
```

  A custom scheme returns a **user id**; `Users.Load` builds the identity, so
  every scheme produces identities the same way. Its verify function may take DI
  parameters before `ctx`.

**Decision:** four built-in scheme types plus `auth.Scheme` for the rest.
`Bearer()`, `Cookie()`, `APIKey()`, `Chain()`, `Extractor`, `ExtractorFunc` and
`InspectExtractor` leave the public API; `nexus.AuthRoute` is removed because
nexus now knows which endpoints are login, logout and me.

## 5. Gates

Deny by default: **with `auth.Module` in the app, every endpoint, page, view,
shard and live page requires a signed-in identity** unless it says otherwise.
Login, logout, token and the area login pages are public automatically.

| Directive | Go | Means |
|---|---|---|
| `//nexus:auth Public` | `auth.Public()` | no identity needed |
| `//nexus:auth Required` | `auth.Required()` | signed in (the default; written for clarity) |
| `//nexus:auth Requires orders.view orders.edit` | `auth.Requires("orders.view", "orders.edit")` | all of these |
| `//nexus:auth RequiresAny orders.edit orders.admin` | `auth.RequiresAny(…)` | at least one |
| `//nexus:auth Kind staff` | `auth.Kind("staff")` | this user kind (combines with Requires) |

```go
// RefundOrder refunds an order.
//
//nexus:mutation
//nexus:auth Kind staff
//nexus:auth Requires orders.refund
func (s *OrderService) RefundOrder(ctx context.Context, in Refund) (*Order, error)
```

- Gates apply to controllers (`nexus.Controller[*T]("/orders", auth.Kind("staff"))`
  and type-level `//nexus:auth`), modules, templ pages (`//nexus:page` +
  `//nexus:auth`), shards and `view.Live` the same way, as in v1.
- `Kind` on the op closes the gap a path prefix can't: a shared `/graphql` mount
  serves both kinds, and each field states its own.
- `Requires` stamps its permissions on the registry entry as in v1
  (middleware.go:93-97), so `auth.OpGates` and the dashboard read the same
  declaration.
- `auth.Optional()` is removed: under deny-by-default "reads identity when
  present" is `Public` + `auth.Current(ctx)`.
- The app-wide default is one key: `[auth] default = "signed-in" | "public"`.
  `public` restores v1 behaviour and is reported by `nexus lint` in production.

**Decision:** adopt; deny-by-default is the v2 default (v2.md §6). `nexus.Public()`
moves to `auth.Public()`; the root keeps the generic `EndpointGate` primitive
internally.

## 6. Areas and user kinds

An area is a path prefix that belongs to some kinds, with its own login page,
landing page and forbidden page:

```toml
[auth.areas.admin]
prefix    = "/admin"
kinds     = ["staff"]
login     = "/admin/login"
home      = "/admin"
forbidden = "Errors/Forbidden"   # page component for denied page visits

[auth.areas.account]
prefix = "/account"
kinds  = ["customer"]
login  = "/login"
home   = "/account"
```

- **Every gate under an area also requires one of its kinds.** A customer's
  session on `/admin/orders` is `Forbidden`, not "signed in". Prefixes cover pages,
  REST and per-module GraphQL mounts alike; exemptions are `Public` endpoints.
- **Unauthenticated page visits redirect to the area's login with `next`**:
  an Inertia visit gets 409 + `X-Inertia-Location`, a document load 302, a view
  navigation the view runtime's redirect. API, GraphQL and WS callers get the error
  (section 12). Outside any area, `[auth] login` is used.
- **Forbidden page visits render the area's `forbidden` component** (or the
  Inertia `ErrorPage` when unset) with the refused path.
- **A token is bound to the kind it was issued for**: signing in through the
  admin login refuses a customer before any token exists (`kinds` is checked in
  `auth.Login`).
- Separate kinds can live in separate stores: `Users.Load` sets `Kind`; one
  table or two is the app's choice.

**Decision:** adopt. `iauth.ErrorHandler` and the app-side audience gates,
routed error handlers and forbidden redirects are replaced by `[auth.areas.*]`.

## 7. Login, logout and tokens

Two primitives, used by the built-in endpoints and by any handler the app writes:

```go
id, err := auth.Login(ctx, auth.Password{Login: in.Email, Password: in.Password})
// throttle → Users.FindLogin → verify + rehash → area kinds → LoginChecker
if err != nil { return nil, err }        // nexus.Error: Invalid, TooMany, Forbidden

to, err := auth.SignIn(ctx, id)          // issues the credential; returns the redirect target
auth.SignOut(ctx)                        // ends this session / revokes this token
```

- **`SignIn` issues for the channel the request came on.** A page request (Inertia,
  view form, HTML form) gets a session — cycled to a new id, CSRF token rotated; an
  API request gets a token pair in the response. `auth.SignIn(ctx, id,
  auth.Using("api"))` forces a scheme.
- **Session rules are config**, enforced by a per-user epoch stored with the
  tokens, so no app keeps cutoff maps:

```toml
[auth.sessions]
single                 = false   # true: signing in ends the user's other sessions
end_on_password_change = true    # changing a password ends every other session
idle                   = "2h"    # session idle timeout
```

- **Built-in endpoints** are paths in config, each off unless set:

```toml
[auth.endpoints]
login  = "/api/auth/login"   # {login, password} → token pair or session
logout = "/api/auth/logout"
me     = "/api/auth/me"      # the current identity (section 11 shape)
token  = "/oauth/token"      # OAuth2 grants: password, refresh_token, client_credentials
revoke = "/oauth/revoke"
```

  The SDK's `nx.auth.login/logout/me` call them; the login response shape is
  fixed (`{access_token, refresh_token, expires_in, token_type}`), so
  `LoginTokenField` goes away.
- **OAuth2 grants fold into `bearer` tokens.** `extension/oauth2` keeps only the
  client registry (`[auth.oauth2.clients.*]` or an `auth.Clients` interface for DB
  clients); go-oauth2 leaves the public API (v2 principle 3). The
  authorization-code flow is an open question.
- **Other token operations:** `auth.Revoke(ctx, token)`, `auth.RevokeUser(ctx,
  userID)` ("sign out everywhere"), `auth.Sessions(ctx, userID)` (list, for a
  "your devices" page), `auth.Keys` for API keys (`Create`, `Revoke`, `List`).

**Decision:** adopt. `Manager.Login/Resolve/Invalidate/InvalidateByIdentity/
InvalidateAll`, `Endpoints.LogoutExtract`, `LoginEndpoint`, `LogoutEndpoint`,
`LoginHandler`, `LogoutHandler`, `LoginIssuer`, `LogoutRevoker`,
`oauth2.Module`/`oauth2.Backend`/`LoginPath`/`LogoutPath` are removed.

## 8. Redirects with `next`

- An unauthenticated page visit redirects to `login?next=<path>`.
- `auth.SignIn` returns the validated `next` (POST body first, then query) or the
  area's `home`; the built-in and Inertia/view login flows redirect there.
- `auth.Next(ctx)` reads it for custom flows. **There is one validator** — a
  same-site root-relative path checked as given and after each decoding round,
  rejecting `//`, backslashes, control characters, schemes and authorities — the
  rules the production app's `next.go` arrived at, with its test cases ported.
- A `next` outside the signing-in user's area is dropped (a staff `next` handed to
  a customer login lands on the customer `home`).

**Decision:** adopt; the parameter name is `[auth] next_param = "next"`.

## 9. Passwords and login throttling

```toml
[auth.passwords]
hashers    = ["argon2id", "bcrypt"]   # first hashes new passwords; all verify; stale → rehash
min_length = 10
common     = true                     # refuse the common-passwords list
similar    = true                     # refuse passwords like the username/email
numeric    = false                    # refuse all-digit passwords

[auth.throttle]
account = "5/15m"      # failures per account
ip      = "50/15m"     # failures per client IP (nexus.ClientIP, trusted proxies)
lockout = "30m"        # after the account limit
```

- **Legacy formats are hashers.** `Hasher` gains `Identify(encoded string) bool`
  so formats without nexus's `id$` prefix (raw bcrypt, a framework's `{bcrypt}…`,
  salted SHA-1) are recognised; a legacy hasher verifies and is never chosen for
  new hashes, so every successful login upgrades the stored hash.

```go
auth.Configure(func(c *auth.Config) { c.Hashers = append(c.Hashers, legacySHA1{}) })
```

- `auth.SetPassword(ctx, id, plain)` validates, hashes, stores via
  `PasswordSetter`, and applies `end_on_password_change`.
- **The throttle counts failures only**, lives in `auth.Login` (so every login
  path — endpoint, page, OAuth2 grant — is covered), stores counters in the
  default cache (shared across replicas when Redis is linked), and answers
  `nexus.TooMany` with `Retry-After`. Locks are listed and cleared on the
  dashboard.

**Decision:** adopt. `oauth2.VerifyBcrypt` and `VerifySpringPassword` leave the
framework (the latter becomes an example hasher in the docs); the validators
become config keys plus `auth.PasswordValidator` for custom rules.

## 10. Impersonation

```toml
[auth.impersonation]
permission = "auth.impersonate"   # who may
endpoint   = "/admin/impersonate" # POST {user_id} starts, DELETE stops
```

```go
err := auth.Impersonate(ctx, targetID)   // checks the permission, loads the target
auth.StopImpersonating(ctx)
```

- While impersonating, `auth.Current(ctx)` is the target and `.Actor` is the real
  user. Gates evaluate the target; the trace, job records and `auth.reject` events
  carry both ids.
- A user can't impersonate an identity holding a permission they lack (no
  escalating to `*`), can't nest, and impersonation ends with the session.
- Works on sessions and bearer tokens (the token record carries the actor).

**Decision:** adopt.

## 11. Permissions in the UI, and per-object checks

```go
auth.Can(ctx, "orders.refund")               // bool, same rule as the gate
auth.OpGates(ctx)                            // {opName: bool} from every Requires
```

- With Inertia, nexus shares `auth` on every page automatically:
  `{user, can}` where `can` is `OpGates` and `user` is what the app's
  `Users` returns from an optional `Public(id *Identity) any` method (nothing
  when absent — the identity's `User` is never serialized by accident). Views read
  `auth.Can(ctx, …)` in templ. `client.d.ts` types `can` by op name.
- **Policies** add per-object rules, typed by the object:

```go
var OrderPolicy = auth.Policy(func(ctx context.Context, me *auth.Identity, perm string, o *Order) bool {
	return o.CustomerID == me.ID || auth.Has(me, "orders.*")
})

func (s *OrderService) CancelOrder(ctx context.Context, in Cancel) (*Order, error) {
	o, err := s.find(ctx, in.ID)
	if err != nil { return nil, err }
	if err := auth.Check(ctx, "orders.cancel", o); err != nil { // perm AND policy for *Order
		return nil, err                                         // nexus.Forbidden
	}
	…
}
```

  `auth.Check(ctx, perm, obj)` runs the permission rule and then the policy
  registered for the object's type; `auth.Can(ctx, perm, obj)` is its bool form.
  A policy is a `nexus.Option`, listed on the dashboard with the types it covers.

**Decision:** adopt. `Gates(ctx, perms…)` stays as the multi-permission form of
`Can`; `AnyOf`/`AllOf`/`DefaultPermissions`/`ExactAuthority`/`Authority`/
`PermissionFn` are removed.

## 12. Errors, transports and CSRF

Auth failures are v2 errors (v2.md §2): `nexus.Unauthenticated` and
`nexus.Forbidden`, with a `reason` field. One table:

| Transport | Unauthenticated | Forbidden |
|---|---|---|
| REST | 401 `{message, reason}` | 403 |
| GraphQL | `extensions.code = UNAUTHENTICATED` | `FORBIDDEN` |
| WebSocket | error envelope, connection closed | error envelope |
| Inertia / view page visit | redirect to the area login with `next` | area `forbidden` page |
| Inertia / view form submit | same redirect | flash under `errors._global` |

An app's JSON shape (`{status:false, message}`) comes from `nexus.Envelope`, not
from an auth error handler.

**Connections re-check.** A WebSocket or live view connection keeps the identity
it opened with, and checks its session/token epoch on each message (one cached
comparison): a revoked or superseded credential closes the connection with
`Unauthenticated`. Live view sockets use the same carrier as `AsWS`.

**Jobs carry identities.** `Enqueue` records the identity id and actor;
`run.Identity()` reloads it through `Users.Load` when the job starts, so a job
runs with the user's permissions at execution time and `auth.Current(ctx)` works
inside it. A job enqueued by an account that no longer exists fails permanently
unless defined with `jobs.AsSystem()`.

**CSRF follows the scheme.** CSRF (on by default when a `session` scheme exists,
v2.md §6) applies to requests authenticated by a cookie scheme and to anonymous
form posts (including login — login CSRF); bearer, apikey and jwt requests skip it,
as v1's `DefaultSkip` does for an `Authorization` header (secure.go:110-112).
`SignIn`/`SignOut` rotate the token. The cookie and header names live in one place,
`[runtime.middleware.security]`, and the SDK manifest reads them from there.

**Decision:** adopt. `ErrorHandler`, `OnError`, `iauth`, `OnFail`, `OnResolve`
(→ trace events `auth.resolve`/`auth.reject`), `auth.Config.CSRFCookie/CSRFHeader`
and the service-level `UserDetailsFn` are removed.

## 13. Discoverability

- **Dashboard Auth tab:** schemes (type, where the credential is read, resolves
  and failures per reason), areas, a gate matrix (every endpoint/page/view with
  its gate; `Public` ones listed separately for review), active sessions per user
  with revoke, throttle locks with unlock, live impersonations, policies.
- **Boot errors**, each with the key or line: a `Kind` gate naming a kind no area
  or identity declares; an area login path that is not a registered page; a
  `session` scheme without `extension/session` storage; a `jwt` scheme with
  neither `secret` nor `jwks`; an optional interface method with the wrong
  signature (`Users.CheckLogin(ctx, *Identity)` expected).
- **`nexus docs auth`** and `nexus auth check` print the effective setup: schemes
  in order, default gate, areas, endpoints, which public endpoints exist.

**Decision:** adopt.

## 14. Testing

```go
app := nexustest.New(t, orders.Module)

staff := app.As(auth.Identity{ID: "7", Kind: "staff", Perms: []string{"orders.*"}})
staff.GET("/admin/orders").Status(200)

app.GET("/admin/orders").Status(302).Location("/admin/login?next=%2Fadmin%2Forders")
app.As(customer).GraphQL(`mutation { refundOrder(id: 1) { id } }`).Code("FORBIDDEN")

ctx := auth.WithIdentity(context.Background(), &auth.Identity{ID: "7"}) // unit tests
```

- `app.As(identity)` authenticates requests through a test-only scheme that
  `nexustest` installs, so every gate, area rule, CSRF exemption and transport
  mapping runs as in production. `app.As("7")` loads the id through the app's real
  `Users`.
- `authtest.Users` is an in-memory `Users` for tests and examples (replaces
  `MemoryUserStore`); `viewtest.As(…)` (v2.md §7.5) and `jobstest` use the same
  identities.

**Decision:** adopt.

---

## v1 → v2

| v1 | v2 | `nexus migrate v2` |
|---|---|---|
| `auth.Module(auth.Config{…})`, `auth.Single(fn)` | `auth.Module` + `[auth]` | literal extractors and endpoint paths move to nexus.toml; resolvers flagged |
| `Config.Authentication.Schemes` with `Bearer()`/`Cookie(n)`/`APIKey(h)`/`Chain` | `[auth.schemes.*]` | rewritten when extractors are literals |
| `Scheme.Resolve`, `Resolver`, `Config.Backend`, `UseBackend`, `StaticBackend`, `auth.Backend`, `ModelBackend`, `UserStore` | `auth.Users` (+ `PasswordSetter`, `LoginChecker`), custom `auth.Scheme` | flagged: method set differs |
| `Identity{Roles, Scopes}` | `Identity{Perms}` (+ `Kind`, `Actor`) | rewritten: `Perms: append(roles, scopes…)` |
| `Authorization.Default: Authenticated()` / `Permit()` | default `signed-in` / `[auth] default = "public"` | rewritten |
| `Authorization.Authority: Wildcard()` | default rule | removed |
| `Authorization.Permissions`, `AnyOf`, `AllOf`, `PermissionFn`, `Backend.Authorize` | `*` perms, `RequiresAny`, `auth.Policy` | flagged |
| `IdentityFrom` | `Current` | rewritten (two-value form → nil check) |
| `Subject[T]` / `SubjectPtr[T]` | `ID[T]` | `Subject` rewritten; `SubjectPtr` flagged |
| `Principal` | `User` holds the user; `Users.Public` for pages | flagged |
| `nexus.Public()`, `auth.Optional()` | `auth.Public()` | rewritten |
| `Endpoints{Login, Logout, Token, Revoke}`, `LoginEndpoint`, `LogoutEndpoint`, `LoginHandler`, `LogoutHandler` | `[auth.endpoints]` | rewritten when paths are literals |
| `nexus.AuthRoute("login"\|"logout"\|"me")` | built-in endpoints | removed; custom login handlers flagged to call `auth.Login`/`SignIn` |
| `Manager.Login` / `Resolve` / `Invalidate` / `InvalidateByIdentity` / `InvalidateAll` / `Identities` | `auth.Login`, `SignOut`, `Revoke`, `RevokeUser`, `Sessions` | rewritten where 1:1, else flagged |
| `CacheFor(d)`, `CacheOption` | `[auth] cache` | rewritten |
| `OnError`, `ErrorHandler`, `iauth.ErrorHandler(url, api)` | `[auth.areas.*]` + `nexus.Envelope` | `iauth` URL → `[auth] login`; custom handlers flagged |
| `OnResolve`, `OnFail` | trace events | flagged |
| `LoginTokenField`, `CSRFCookie`, `CSRFHeader` | fixed login shape; `[runtime.middleware.security]` | removed / moved |
| `SessionCookie` | `session` scheme | flagged |
| `Hashers`, `DefaultHashers`, `BCrypt()`, `Argon2id()`, `PBKDF2()` | `[auth.passwords] hashers` | rewritten |
| `ValidatePassword`, `MinLength`, `NotCommon`, … | `[auth.passwords]`, `auth.SetPassword` | rewritten for literal arguments |
| `MemoryUserStore` | `authtest.Users` | rewritten |
| `oauth2.Module`, `oauth2.Backend`, `oauth2.Config` | `bearer` scheme + `[auth.endpoints] token` + `[auth.oauth2.clients]` | flagged |
| `oauth2.VerifyBcrypt`, `VerifySpringPassword` | a `Hasher` | flagged |
| `session.Required()`, `//@session Required` | unchanged (`//nexus:session Required`) | prefix only |
| `Service.Auth(UserDetailsFn)` | `auth.Module` | flagged |
| `//@auth Required\|Requires\|Public` | `//nexus:auth …` (+ `RequiresAny`, `Kind`) | prefix only (v2.md §11) |

Kept as is: `auth.Required()`, `auth.Requires(…)`, `auth.Can`, `auth.Gates`,
`auth.OpGates` (now without the `*App` argument), `auth.User[T]`,
`auth.WithIdentity`, `extension/session`'s handle (`session.Get(ctx)`).

## Stages

Each stage lands on `v2` as an alpha (v2.md, Stages); auth work belongs to v2.md's
stage 2 (behaviour).

1. **Core.** `Identity` v2, `Users`, `session`/`bearer`/`apikey` schemes with the
   hashed token store, `Current`/`User`/`ID`, gates with deny-by-default, errors
   through the v2 error model with reasons, the WS/live-view carrier and per-message
   epoch check.
2. **Login.** `auth.Login`/`SignIn`/`SignOut`, hashers with `Identify`, password
   config, throttle, `next`, areas and their redirects, built-in endpoints, CSRF
   tie-in, Inertia `auth` shared prop.
3. **Tokens.** Refresh, `[auth.sessions]` rules, `RevokeUser`/`Sessions`, API keys,
   the `jwt` scheme, OAuth2 grants folded in, `extension/oauth2` reduced to clients.
4. **The rest.** Impersonation, policies, jobs identities, dashboard tab,
   `nexus auth check`, `nexustest.As`/`authtest`.
5. **Migration.** Codemod rows above; the production app migrates. Target: its
   throttle, client IP, `next`, audience gate, routed error handlers, session
   cutoffs, token store and token generator are deleted, leaving `Users`, a legacy
   hasher, a `jwt` scheme for its mobile tokens, and its login checks.

## Shipping in 2.x

v2.0.0 shipped with the v1 auth API, so this design lands **additively**: every
new name arrives beside the one it replaces, the old one gets a `Deprecated:`
comment once its replacement ships, and removals wait for v3. Where the design
changes behaviour (deny-by-default, a bad credential is an error), the new
behaviour belongs to the new configuration path only — an app on
`auth.Module(auth.Config{Authentication: …})` behaves as it does in 2.0.

One change from §4 as built: a credential that arrives but fails leaves the request
**anonymous** — with the reason recorded, shown in the 401 under nexus dev and in the
trace — rather than failing it outright, so a public page (the login page itself)
still loads with a stale cookie or token. Gated endpoints refuse it as before.

`auth.Module(auth.Config{…})` stays the entry point (its signature can't change in
2.x); the design's `auth.Module` value becomes `auth.Module(auth.Config{})` plus
the `[auth]` table, and `Config.Users` names the app's `Users`.

| Slice | New | Replaces (deprecated later) | Release |
|---|---|---|---|
| 1a | `Identity.Kind`, `Identity.Perms` (wildcards), `auth.Current`, `auth.ID`, `auth.RequiresAny`, `auth.Kind`, `//nexus:auth RequiresAny/Kind`, `OpGates` for both | `IdentityFrom`, `Subject`; `Roles`/`Scopes` stay | done (2.1) |
| 1b–1d | `auth.Users` (+ `PasswordSetter`, `LoginChecker`) via `Config.Users: auth.UseUsers(ctor)`, checked at boot; `Load` cached per id; `[auth.schemes.*]` `session`/`bearer`/`apikey` with a hashed `TokenStore`; `auth.Login`/`SignIn`/`SignOut`/`SetPassword`/`Refresh`, `[auth.passwords]`; `[auth] default = "signed-in"` + `auth.Public()`; a failing credential is anonymous with its reason in the 401 (dev) and the trace | `Resolver`, `Backend` capabilities, `UserStore`/`ModelBackend`, `Scheme{Extract, Resolve}`, extractors, `Optional()` | done (2.1) |
| 2 | areas (kind gates, login redirects with `next`, kinds checked in `Login`), the `next` validator, throttle (per process), `[auth.endpoints]` login/logout/me (`Users.Public`), `OpGates` follows sign-in and areas | `Endpoints`, `LoginEndpoint`/`LogoutEndpoint`, `ErrorHandler` | done (2.2) |
| 2b | CSRF token rotation on `SignIn`/`SignOut`; the automatic Inertia `auth` prop `{user, can}`; the area `forbidden` page (a path page visits are redirected to, not a component — auth renders no pages itself); a shared (cache-backed) throttle | — | done (2.3) |
| 3a | per-user epoch in the `TokenStore`; `RevokeUser`, `Revoke`; `[auth.sessions]` single / end_on_password_change / idle; WS and live-view connections re-checked per message (`nexus.RegisterConnectionCheck`) | `Manager.Invalidate*` | done (2.4) |
| 3b | refresh tokens (rotating), `[auth.endpoints]` token (OAuth2 password + refresh_token grants) and revoke, CSRF-exempt via `App.ExemptCSRF`; `jwt` (secret / PEM / JWKS, alg pinned to the key) | `Manager.*`, `extension/oauth2` server half | done (2.5) |
| 3c | `auth.Sessions`/`RevokeSession` (sessions recorded in the token store), `auth.Keys` (named API keys), through an optional `TokenLister` | — | done (2.6) |
| 3d | OAuth2 clients: `[auth.oauth2.clients]` or `auth.Clients`, Basic/body client auth, `client_credentials` (identity `client:<id>`), `require_client` | `extension/oauth2` clients | done (2.8) |
| 4 | impersonation (the credential stays the actor's; a session/token records the target), `auth.Policy`/`Check`/`Allowed`, jobs run as their enqueuer (`nexus.RegisterIdentityRestorer`), `nexus auth check`, `authtest` (`As`/`AsUser`, test-binary-only) | `MemoryUserStore` | done (2.7) |
| 4b | the dashboard Auth tab (schemes, areas, gate matrix, sessions, throttle locks, impersonations) | — | later |
| 5 | `nexus migrate` rows for every deprecated name | — | with each slice |

**2.9 — the v1 API removed.** With no app on v2 yet, the deprecation window was
dropped: resolvers, `Backend`, extractors, `Manager`, `Endpoints`, `ErrorHandler`,
`Optional`, `Subject`/`SubjectPtr`, `IdentityFrom`, `Principal`, `Roles`/`Scopes`/`Extra`,
`MemoryUserStore`/`ModelBackend`/`Authenticate`, `extension/oauth2` and
`extension/inertia/iauth` are gone; `auth.Module(auth.Config{Users: …})` is the only
path. Identity's app user is `User` and `Password`'s login field `Login`, as §2 and §7
proposed. The dashboard's Auth tab (4b) shows the setup, every endpoint's gate,
throttle locks and a sign-out-everywhere form.

**Decided (2026-10-04) and built:** OIDC sign-in as a scheme type (`type =
"oidc"`); an optional permission catalogue (`[auth] perms`, checked at boot); roles as
a config table (`[auth.roles]`, `Identity.Roles` expanded into `Perms`); JWT
revocation opt-in per scheme (`revocable = true`).

**Decided (2026-10-04) and built, second round:**
- *One session per area* — opt-in per area: `[auth.areas.admin] session = "admin"`
  names a `session` scheme that holds that area's sign-in alone, as a stored token in
  its own HttpOnly cookie scoped to the prefix (RevokeUser, Sessions and the token
  store cover it). Areas without it share the app's session.
- *Identity as a handler parameter* — `*auth.Identity` (nil when anonymous) and
  `auth.Identity` (401 without a sign-in), through a general root hook,
  `nexus.RequestParam[T](fill)`.
- *Production token store* — `extension/auth/authdb` (`authdb.Bind[DB]()`, a GORM
  table) beside `CacheTokens`; memory stays the default with a production warning
  and a `nexus doctor` check, not a boot error.
- *`extension/session`* stays a separate package: apps use sessions without auth,
  and the `session` scheme installs it when it isn't already.

## Open questions

- **Authorization-code / OIDC.** "Sign in with an external provider" as a scheme
  type in 2.0 (`type = "oidc"`), or a 2.x addition?
- **Permission catalogue.** Should apps declare permissions (`[auth.perms]` or Go
  constants) so a misspelt `Requires` fails at boot and the dashboard can list
  who holds what, at the cost of one more declaration?
- **Roles in nexus.** Keep roles app-side (expanded in `Users.Load`), or ship a
  role → permissions table (`[auth.roles]` / DB-backed) like Django's groups?
- **An escape hatch for denials.** Areas + `Envelope` cover the production app's
  handlers. Is a per-area render hook needed for anything else, or does its absence
  hold?
- **Identity as a handler parameter.** `func (s *Svc) X(ctx, me *auth.Identity, in
  T)` — depends on v2.md's open question about extra typed parameters.
- **Production token store without a cache.** Memory (everyone signed out on
  restart, wrong with replicas) or a boot error when `environment = "production"`?
- **One session per area.** Should each area get its own session cookie so a
  staff member and a customer can be signed in in one browser, or is one session
  per browser the rule?
- **`jwt` and revocation.** Checking the per-user epoch makes JWTs revocable but
  costs a (cached) lookup per request; is that the default, or opt-in per scheme?
- **`extension/session` placement.** It becomes a hard dependency of the `session`
  scheme — fold it into `auth`, or keep it separate for apps that use sessions
  without auth?

(All of the above except the denial escape hatch are decided — see "Decided" in the
section before.)
