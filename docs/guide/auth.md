# Auth

`extension/auth` is accounts, credentials and gates in one module. You write one
`Users` type — find an account by its login, load it by id — and nexus does the rest:
it issues and checks sessions, bearer tokens and API keys (and verifies JWTs another
service issued), runs the sign-in flows, and enforces gates the same way on REST,
GraphQL, WebSocket, Inertia pages and views. Settings live in nexus.toml's `[auth]`
table.

## Accounts and sign-in

Implement `auth.Users` — find an account by its login, load it by id — and hand its
constructor to `auth.Module`:

```go
type Users struct{ db *DB }

func NewUsers(db *DB) *Users { return &Users{db: db} }

// FindLogin returns the account a sign-in names and its encoded password.
func (u *Users) FindLogin(ctx context.Context, login string) (*auth.Identity, string, error) {
    var row User
    if err := u.db.Where("email = ?", login).First(&row).Error; err != nil {
        return nil, "", ignoreNotFound(err)
    }
    return u.identity(row), row.Password, nil
}

// Load rebuilds the identity of a signed-in user, by id.
func (u *Users) Load(ctx context.Context, id string) (*auth.Identity, error) {
    var row User
    if err := u.db.First(&row, "id = ?", id).Error; err != nil {
        return nil, ignoreNotFound(err)
    }
    return u.identity(row), nil
}

func (u *Users) identity(row User) *auth.Identity {
    return &auth.Identity{ID: strconv.FormatInt(row.ID, 10), Kind: row.Kind, Perms: row.Perms, User: &row}
}

nexus.Boot(auth.Module(auth.Config{Users: auth.UseUsers(NewUsers)}), …)
```

A type that doesn't implement `Users` fails boot, naming the missing method;
`Config.Users` is the one setting `auth.Module` requires.
`FindLogin`'s identity needs the user's `Kind` when you use areas: signing in under an
area, and the `next` a sign-in returns to, are checked against it. Two
optional methods add to it: `SetPassword(ctx, id, encoded string) error` stores
passwords (`auth.SetPassword`, and a rehash when a stored hash is outdated), and
`CheckLogin(ctx, id *auth.Identity) error` refuses a sign-in, such as a disabled
account.

Sign-in is two calls in any handler:

```go
func (s *AccountService) SignIn(ctx context.Context, in SignInForm) (*auth.Credential, error) {
    id, err := auth.Login(ctx, auth.Password{Login: in.Email, Password: in.Password})
    if err != nil {
        return nil, err // "invalid login or password" (422), or CheckLogin's error
    }
    return auth.SignIn(ctx, id)
}

nexus.AsRest("POST", "/sign-in", (*AccountService).SignIn, auth.Public())
```

- **`auth.Login`** finds the account, checks the password with the `[auth.passwords]`
  hashers, rehashes it when it was stored with an older algorithm, and runs
  `CheckLogin`. A wrong password and an unknown account give the same error in the
  same time.
- **`auth.SignIn`** issues the credential. With a session scheme it cycles the session
  id and stores the user's id in it. With `auth.Using("api")` it issues a bearer token
  (or an API key, for an `apikey` scheme) and returns it once, as
  `{"access_token", "token_type", "expires_in"}`. Only the token's SHA-256 is stored.
- **`auth.SignOut(ctx)`** ends the credential the request came with: it destroys the
  session, or revokes the token.
- **`auth.SetPassword(ctx, id, plain)`** checks the `[auth.passwords]` rules, then
  hashes and stores the password. **`auth.Refresh(ctx, userID)`** drops the cached
  `Load` after you change a user's roles.

Schemes are declared in nexus.toml. Without any, one session scheme named `web` is
used:

```toml
[auth.schemes.web]
type = "session"

[auth.schemes.api]
type = "bearer"
ttl  = "12h"
```

A session scheme brings `extension/session` with it, and turns CSRF protection on. To
use your own session store, put `session.Module` before `auth.Module`. Tokens are kept
in memory by default, which loses them on restart. In production set
`Config.Tokens: auth.CacheTokens(cache)` (Redis through `extension/cache`), or your own
`auth.TokenStore`.

**Every endpoint needs a sign-in** on this path unless it is marked `auth.Public()`;
`[auth] default = "public"` turns that off. `Users.Load` results are cached per user
id for `[auth] cache` (5 minutes by default). When a credential arrives but doesn't
work — an expired or revoked token, a deleted account — the request is anonymous, and
under `nexus dev` the 401 says why. All keys: [`[auth]`](/reference/nexus-toml#auth).

### Areas, `next` and the sign-in page

An area is a path prefix that belongs to some kinds of user, with its own sign-in
page:

```toml
[auth]
login = "/login"             # the sign-in page outside every area

[auth.areas.admin]
prefix = "/admin"
kinds  = ["staff"]           # empty: any signed-in user
login  = "/admin/login"
home   = "/admin"            # default: the prefix
```

- **Every endpoint under the prefix needs a sign-in of one of its kinds** — a
  customer's session on `/admin/orders` is refused with 403 — except `Public` ones,
  such as the area's own sign-in page.
- **An unauthenticated page visit goes to the sign-in page** with `?next=` back to
  it: a document load gets a 302, an Inertia visit a 409 with `X-Inertia-Location`.
  API, GraphQL and WebSocket callers get the 401.
- **Signing in under an area admits only its kinds.** A customer posting to
  `/admin/login` gets the same "invalid login or password" as a wrong password.
- **`Credential.Next` is where to go after signing in:** the request's `?next=` (or
  `auth.ReturnTo(next)`), when it is a same-site path in an area the user may
  enter, else the area's `home`, else `[auth] home`. The check refuses `//host`,
  backslashes, schemes and control characters, as given and after each round of URL
  decoding, so `next` can't send anyone off-site. `auth.Next(ctx)` reads it for
  your own flows.

A refused page visit — a customer opening `/admin/orders` — goes to the area's
`forbidden` page when it has one (`forbidden = "/admin/forbidden"`, a page your app
serves), else `[auth] forbidden`; API calls get the 403.

### On every page

Inertia pages get an `auth` prop on every render — `{user, can}`, the same shape as
the built-in `me` endpoint — so a layout can show the user and hide what they can't
do without a shared prop of your own. `[auth] page_prop` renames it, `"-"` turns it
off, and an app prop with the same key wins.

`auth.SignIn` and `auth.SignOut` give the browser a new CSRF token, so a token planted
before a sign-in is worthless after it.

### Signing out everywhere

```go
auth.RevokeUser(ctx, userID)   // ends every session and token the user holds
auth.Revoke(ctx, token)        // ends one bearer token or API key
```

Each user has an epoch — when their credentials were last ended — and every session and
token records the one it was issued under, so ending them all is one write. Open
WebSocket and live-page connections check it before each message and close with
`Unauthenticated` once it moves. Other replicas notice within `[auth] cache`.

```toml
[auth.sessions]
single                 = false   # true: signing in ends the user's other sessions
end_on_password_change = true    # auth.SetPassword ends every other session and token
idle                   = "2h"    # end a session unused this long (0 = never)
```

### Devices and API keys

```go
list, err := auth.Sessions(ctx, userID)            // where the user is signed in, newest first
err = auth.RevokeSession(ctx, userID, list[1].ID)   // end one of them

key, dev, err := auth.Keys.Create(ctx, userID, "partners", "billing sync")  // an apikey scheme
keys, err := auth.Keys.List(ctx, userID)
err = auth.Keys.Revoke(ctx, userID, dev.ID)
```

Each `auth.Device` has a `Kind` ("session", "token" or "key"), its scheme, when it was
created and expires, the User-Agent and IP it was issued to, and `Current` for the
request's own credential — what a "your devices" page shows. `ID` is a hash, never the
credential. A key is returned once, at `Create`; only its hash is stored.

Listing needs the token store to list a user's tokens (`auth.TokenLister`); the memory
store and `CacheTokens` do. Sessions signed in from 2.6 on are listed — each gets a
record in the token store, which `RevokeSession` deletes.

### Refresh tokens, OAuth2 and tokens from elsewhere

A bearer scheme with `refresh` set returns a refresh token with every sign-in;
`auth.RefreshToken(ctx, refresh)` exchanges it for a new pair, and the old one stops
working. A refresh token isn't accepted as an access token, and one from before the
user's last `RevokeUser` is refused.

```toml
[auth.schemes.api]
type    = "bearer"
ttl     = "1h"
refresh = "720h"

[auth.endpoints]
token  = "/oauth/token"     # RFC 6749: grant_type=password and refresh_token
revoke = "/oauth/revoke"    # RFC 7009
```

The token endpoint takes form-encoded or JSON bodies, answers errors as
`{"error": "invalid_grant", …}`, and skips CSRF even in an app with sessions — it
sets no cookie and answers only the caller, which is also what
`app.ExemptCSRF(path)` is for.

**OAuth2 clients.** The token endpoint also knows clients — services and apps that call
it — from nexus.toml, or from your database through `Config.Clients` (an
`auth.Clients`, consulted first):

```toml
[auth.oauth2]
require_client = false                 # true: password and refresh_token need a known client too

[auth.oauth2.clients.billing]
secret = "${BILLING_CLIENT_SECRET}"    # or secret_hash, as auth.Hashers encodes; neither: a public client
grants = ["client_credentials"]        # empty: all of password, refresh_token, client_credentials
perms  = ["reports.view"]              # what its client_credentials tokens may do
kind   = "service"                     # their identity's Kind (default "client")
```

A client authenticates with HTTP Basic or `client_id`/`client_secret` in the body;
a wrong secret is `invalid_client` (401). With `client_credentials` a confidential
client gets a token as itself — identity `client:<id>`, its `perms` and `kind`, no
refresh token — which every gate, area and `RevokeUser("client:<id>")` treats like
a user's.

A `jwt` scheme verifies tokens another service issued — nexus never issues them — and
loads the user named by the subject claim:

```toml
[auth.schemes.mobile]
type     = "jwt"
jwks     = "https://id.example.com/.well-known/jwks.json"  # or secret (HS256), or public_key (PEM: RS256/ES256)
issuer   = "https://id.example.com"
audience = "orders-mobile"
subject  = "sub"              # the claim holding the user id (default sub)
leeway   = "1m"               # clock skew allowed on exp and nbf
```

Only the algorithms the key is for are accepted (HS256 for a secret, RS256 or ES256
for a public key), so a token can't pick `none`, or HS256 against an RSA key. JWKS
keys are cached for an hour and fetched again for an unknown `kid`. A jwt scheme and
a bearer scheme share the `Authorization` header: a JWT has three dot-separated
parts, a nexus token none.

### Impersonation

```go
err := auth.Impersonate(ctx, targetID)   // the rest of this session acts as targetID
err = auth.StopImpersonating(ctx)
```

```toml
[auth.impersonation]
permission = "auth.impersonate"     # who may (default)
endpoint   = "/admin/impersonate"   # POST {user_id} starts, DELETE stops
```

While impersonating, `auth.Current(ctx)` is the target and `.Actor` the real user;
gates evaluate the target, and `auth.reject` trace events carry both ids. Nobody can
take on a user holding a permission they lack, impersonations don't nest, and the
credential stays the real user's — signing them out (or `RevokeUser` on them) ends
it. Works on sessions, bearer tokens and API keys. The `me` endpoint and the Inertia
`auth` prop add `actor`, for a "you are acting as …" banner.

### Per-object rules

```go
var OrderPolicy = auth.Policy(func(ctx context.Context, me *auth.Identity, perm string, o *Order) bool {
    return o.CustomerID == me.ID || me.Has("orders.*")
})

nexus.Boot(OrderPolicy, …)

if err := auth.Check(ctx, "orders.cancel", order); err != nil {   // perm, then the *Order policy
    return nil, err                                               // Unauthenticated or Forbidden
}
auth.Allowed(ctx, "orders.cancel", order)                         // the bool, for UI toggles
```

A policy is registered per type — `*Order` and `Order` are different — and a type
without one is checked on the permission alone; `perm` may be `""` to check only the
policy.

### Background jobs

A job enqueued from a request records who enqueued it, and runs as that user: their
identity is loaded through `Users.Load` when the job starts, so `auth.Current(ctx)`,
`auth.Can` and `auth.Check` work inside it with the permissions they have *then*. A
job whose user no longer exists fails without retrying.

### Checking the setup

`nexus auth check [nexus.toml]` validates `[auth]` the way boot does — an unknown key,
a scheme type nexus doesn't have, a jwt scheme without a key, a duration like `"30d"`
(Go durations stop at hours: write `"720h"`) — and prints the effective setup:
schemes in the order they are tried, the default gate, pages and areas, endpoints,
and the session, throttle and password rules.

### Testing

```go
import "github.com/paulmanoni/nexus/v2/extension/auth/authtest"

users := authtest.NewUsers().
    Add(auth.Identity{ID: "7", Kind: "staff", Perms: []string{"orders.*"}}, "ana@example.com", "a long password")
app := nexustest.New(t, config.Runtime{}, auth.Module(auth.Config{Users: auth.StaticUsers(users)}), orders.Module)

app.GET("/admin/orders").AssertStatus(401)
app.With(authtest.As(&auth.Identity{ID: "1", Kind: "staff", Perms: []string{"orders.view"}})).
    GET("/admin/orders").AssertOK()
app.With(authtest.AsUser("7")).GET("/admin/orders").AssertOK()   // loaded through Users
```

`authtest.As` and `AsUser` return a header the auth middleware honours **only in a
test binary**, so every gate, area rule and policy runs as for a real sign-in.
`authtest.Users` is an in-memory `Users` (with `SetPassword`) for tests and
examples. `nexustest`'s `App.With(header)` sends any header with every request.

### Throttling sign-ins

`auth.Login` counts failures — per account, and per client IP (`nexus.ClientIP`,
which honours trusted proxies) — and answers 429 "too many failed sign-ins" past
the limit:

```toml
[auth.throttle]
account = "5/15m"            # failures per account in a window; "off" disables
ip      = "50/15m"           # failures per client IP
lockout = "15m"              # how long an account stays locked at its limit
```

The counters live in the process, so each replica counts on its own; set
`Config.Throttle: auth.CacheThrottle(cache)` to share them through Redis.

### Built-in endpoints

```toml
[auth.endpoints]
login  = "/api/auth/login"   # POST {login, password, next?, scheme?} → Credential
logout = "/api/auth/logout"  # POST
me     = "/api/auth/me"      # GET {user, can}
```

Each is mounted only when its path is set, and they're tagged for the client SDK's
`nx.auth.login/logout/me`. `me` answers for anonymous visitors too (`"user": null`).
`user` is what an optional `Public(id *auth.Identity) any` method on your `Users`
returns — `{id, kind}` without one, so `Identity.User` is never sent by accident. `can` is
`auth.OpGates`; on this path it is false for any op the visitor couldn't call,
signed in or not.

## Gates

Gates work the same on REST, GraphQL and WebSocket:

```go
nexus.AsMutation(NewCreateOrder,
    auth.Required(),                // 401 without an identity
    auth.Requires("orders:create"), // 403 without the permission
)
```

- **Deny by default.** Every endpoint, page, view and live page requires a sign-in
  unless it opts out — `[auth] default = "public"` turns that around:

  ```go
  nexus.AsRest("GET", "/health", NewHealth, auth.Public())
  ```

- **Permissions and kinds on the identity.** `Identity.Perms` is matched with
  wildcards — `orders.*` grants `orders.view` and `orders.refunds.create`, `*` grants
  everything — and `Identity.Kind` says which kind of user it is ("staff",
  "customer"). Roles are your concept: `Users.Load` expands them into `Perms`.

- **At least one, and user kinds.** `auth.RequiresAny` passes with any one of its
  permissions; `auth.Kind` gates on the identity's kind. Both imply sign-in and
  combine with `Requires` — `Kind` covers what a path prefix can't, such as one
  `/graphql` mount serving staff and customers:

  ```go
  nexus.AsMutation((*Orders).Refund,
      auth.Kind("staff"),
      auth.RequiresAny("orders.refund", "orders.admin"),
  )
  ```

- **Decorator form.** The same gates attach as `//nexus:auth` modifiers — bare tokens,
  capability case-insensitive:

  ```go
  //nexus:mutation
  //nexus:auth Requires orders:create
  func NewCreateOrder(...) (...)

  //nexus:mutation
  //nexus:auth Kind staff
  //nexus:auth RequiresAny orders.refund orders.admin
  func (o *Orders) Refund(...) (...)

  //nexus:rest GET /health
  //nexus:auth Public
  func NewHealth(...) (...)
  ```

  See [`//nexus:` decorators](./decorators#auth-and-session-gates).

## Who is calling

```go
me := auth.Current(ctx)                // *Identity, nil when anonymous
user, ok := auth.User[MyUser](ctx)     // Identity.User, typed
uid, ok  := auth.ID[uint](ctx)         // Identity.ID parsed into T
```

## UI permissions that can't drift

`auth.Can(ctx, "users:delete")` and `auth.Gates(ctx, ...)` use the same rules as
`Requires`. `auth.OpGates(ctx, app)` goes further. It answers `map[opName]bool` for every
registered op, so the frontend asks by the op names it already calls
(`can.deleteUser`), and no permission string exists outside the registration:

```go
inertia.ShareProvide(func(app *nexus.App) inertia.SharedProvider {
    return func(ctx context.Context) (string, any) { return "can", auth.OpGates(ctx, app) }
})
```

`OpGates` evaluates `RequiresAny` and `Kind` gates too. The gate table is compiled
once and grouped by permission set. It costs about 4µs for
200 ops, so it is safe on every render.

## Passwords

`auth.Login` checks passwords with the `[auth.passwords]` hashers, and
`auth.SetPassword` hashes and stores new ones after checking the rules:

```toml
[auth.passwords]
hashers    = ["bcrypt", "argon2id", "pbkdf2"]   # the first hashes new passwords; all verify
min_length = 8
common     = true    # refuse common passwords
numeric    = true    # refuse all-digit passwords
similar    = true    # refuse passwords like the user's id or login
```

Hashes describe their own algorithm (`bcrypt$…`), so a password stored with another
listed hasher still verifies and is rehashed on its next sign-in (through
`SetPassword` on your `Users`). `auth.Hashers`, `auth.BCrypt()`/`Argon2id()`/`PBKDF2()`
and `auth.ValidatePassword` are there for code of your own, such as a seed script.

See also: [Web security](./security) for CSRF and headers, and
[Sessions](./sessions) for server-side sessions.
