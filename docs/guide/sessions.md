# Sessions

`extension/session` provides Django-style server-side sessions. A cookie carries an
opaque ID, the data lives in a store, and handlers use a lazy handle. It works for
anonymous visitors and signed-in users, on REST, Inertia and GraphQL.

```go
import "github.com/paulmanoni/nexus/extension/session"

nexus.Boot(session.Module(session.Config{}))   // in-memory store, 14-day TTL
```

```go
s := session.Get(ctx)
s.Set("cart", skus)   // the first write creates the ID and sets the cookie
s.Cycle()             // rotate the ID on login (session fixation defense)
s.Destroy()           // logout: delete the stored data and expire the cookie
```

The session is lazy:

- The store isn't read until the session is touched.
- Nothing is saved unless the session was modified. `s.Touch()` forces a save.
- The cookie is set on the first write, so write the session before the response body.

Values must round-trip through JSON.

## Stores

| Store | Survives |
|---|---|
| `session.NewMemoryStore()` (default) | `nexus dev` rebuilds, but not production restarts |
| `session.CacheStore(cache)` | Restarts and multiple replicas, when the cache is Redis |
| Your own `session.Store` | Whatever your database gives you |

The cookie is always HttpOnly, and SameSite defaults to Lax. Set `Secure: true` behind
TLS.
