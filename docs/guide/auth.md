# Auth

`extension/auth` handles the whole auth path:

1. extract a credential from the request
2. resolve it to an identity, with caching
3. enforce per-op gates
4. report 401s and 403s to the dashboard

## Resolve a token

```go
import "github.com/paulmanoni/nexus/extension/auth"

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

- **Wildcard permissions.** `auth.Authorization{Authority: auth.Wildcard()}` lets
  `orders:*` grant `orders:read`.

## Who is calling

```go
user, ok := auth.User[MyUser](ctx)     // the Identity's Extra payload, typed
uid, ok  := auth.Subject[uint](ctx)    // Identity.ID parsed into T
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

The gate table is compiled once and grouped by permission set. It costs about 4µs for
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
