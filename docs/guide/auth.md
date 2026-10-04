# Auth

`extension/auth` handles the whole auth path:

1. extract a credential from the request
2. resolve it to an identity, with caching
3. enforce per-op gates
4. report 401s and 403s to the dashboard

There are two ways to set it up. **Accounts and sign-in** (new in 2.1) is the one to
start with: you write one `Users` type, and nexus issues and checks sessions, tokens
and API keys itself. **Resolve a token** plugs in a resolver you write, for apps that
verify credentials someone else issues.

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
    return &auth.Identity{ID: strconv.FormatInt(row.ID, 10), Kind: row.Kind, Perms: row.Perms, Extra: &row}
}

nexus.Boot(auth.Module(auth.Config{Users: auth.UseUsers(NewUsers)}), …)
```

A type that doesn't implement `Users` fails boot, naming the missing method.
`FindLogin`'s identity needs the user's `Kind` when you use areas: signing in under an
area, and the `next` a sign-in returns to, are checked against it. Two
optional methods add to it: `SetPassword(ctx, id, encoded string) error` stores
passwords (`auth.SetPassword`, and a rehash when a stored hash is outdated), and
`CheckLogin(ctx, id *auth.Identity) error` refuses a sign-in, such as a disabled
account.

Sign-in is two calls in any handler:

```go
func (s *AccountService) SignIn(ctx context.Context, in SignInForm) (*auth.Credential, error) {
    id, err := auth.Login(ctx, auth.Password{Username: in.Email, Password: in.Password})
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
returns — `{id, kind}` without one, so `Extra` is never sent by accident. `can` is
`auth.OpGates`; on this path it is false for any op the visitor couldn't call,
signed in or not.

## Resolve a token

```go
import "github.com/paulmanoni/nexus/v2/extension/auth"

resolve := func(ctx context.Context, token string) (*auth.Identity, error) {
    u, err := tokens.Validate(ctx, token)
    if err != nil {
        return nil, err
    }
    return &auth.Identity{ID: u.ID, Roles: u.Roles, Extra: u}, nil
}

nexus.Boot(
    auth.Single(resolve, auth.CacheFor(15*time.Minute)), // one bearer scheme
    ordersModule,
)
```

For several schemes, tried in order:

```go
auth.Module(auth.Config{
    Authentication: auth.Authentication{
        Schemes: []auth.Scheme{
            {Resolve: resolve}, // Bearer() by default
            {Name: "apikey", Extract: auth.APIKey("X-API-Key"), Resolve: resolveKey},
        },
        Cache: auth.CacheFor(15 * time.Minute),
    },
})
```

The extractors are `auth.Bearer()`, `auth.Cookie(name)`, `auth.APIKey(header)` and
`auth.Chain(...)`.

## Gates

Gates work the same on REST, GraphQL and WebSocket:

```go
nexus.AsMutation(NewCreateOrder,
    auth.Required(),                // 401 without an identity
    auth.Requires("orders:create"), // 403 without the permission
)
```

- **Deny by default.** Every endpoint requires an identity unless it opts out:

  ```go
  auth.Authorization{Default: auth.Authenticated()}
  nexus.AsRest("GET", "/health", NewHealth, nexus.Public())
  ```

- **Permissions and kinds on the identity.** `Identity.Perms` is matched with
  wildcards — `orders.*` grants `orders.view` and `orders.refunds.create`, `*` grants
  everything — and `Identity.Kind` says which kind of user it is ("staff",
  "customer"). Roles and Scopes still match exactly, as before; for wildcards there
  too, set `auth.Authorization{Authority: auth.Wildcard()}`.

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
user, ok := auth.User[MyUser](ctx)     // the Identity's Extra payload, typed
uid, ok  := auth.ID[uint](ctx)         // Identity.ID parsed into T
```

`auth.IdentityFrom` and `auth.Subject` are the older spellings of `Current` and
`ID`, and still work.

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

## One backend for everything

A static resolve function can't see DI dependencies. Declare a DI-constructed backend
instead, and implement whichever capabilities you need:

```go
auth.Module(auth.Config{
    Backend: auth.UseBackend(func(db *DB) *AuthBackend { return NewAuthBackend(db) }),
    Endpoints: auth.Endpoints{
        Login:  "/api/auth/login",   // Backend.Login + Backend.Issue
        Logout: "/api/auth/logout",  // Manager.Invalidate + Backend.RevokeToken
        Token:  "/oauth/token",      // Backend.TokenHandler
    },
})
```

| Method | Powers |
|---|---|
| `Resolve(ctx, token) (*Identity, error)` | Token resolution for schemes without their own |
| `Login(ctx, Credentials) (*Identity, error)` | `Manager.Login` and the Login endpoint |
| `Authorize(id, required) bool` | Permission checks |
| `Issue(ctx, *Identity) (any, error)` | The login response (for example, a token pair) |
| `RevokeToken(ctx, token) error` | Logout and revoke |
| `TokenHandler() httpx.HandlerFunc` | A raw grant endpoint |

For a full OAuth2 server (password, client credentials and refresh grants), use
`oauth2.Backend(oauth2.Config{...})` from `extension/oauth2` as the backend.

## Passwords and login

Django-style, with every piece swappable:

```go
store := auth.NewMemoryUserStore()                // or your own UserStore
store.CreateUser("alice", "s3cret-pw", "ADMIN")

id, err := auth.Authenticate(ctx,
    auth.Password{Username: "alice", Password: "s3cret-pw"},
    auth.NewModelBackend(store))
// A wrong password or an unknown user → auth.ErrInvalidCredentials

err = auth.ValidatePassword(ctx, pw, id, auth.DefaultValidators()...)
```

- **Hashers:** `auth.BCrypt()` (the default), `auth.Argon2id()` and `auth.PBKDF2()`.
  Hashes describe their own algorithm, so a set of hashers verifies any of them and
  rehashes stale hashes on login.
- **Validators:** `MinLength`, `NotNumericOnly`, `NotCommon` and `NotSimilarToUser`.
- **Backends:** tried in order. `auth.ModelBackend` checks a `UserStore` (`ByUsername`,
  `ByID`, `SetPassword`). Timing is equalized, so responses don't reveal which users
  exist.

## Logout and invalidation

Take `*auth.Manager` and call `Invalidate(token)` or `InvalidateByIdentity(id)`. The
dashboard's Auth tab can do the same per row.

See also: [Web security](./security) for CSRF and headers, and
[Sessions](./sessions) for server-side sessions.
