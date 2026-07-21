<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/banner.svg">
    <source media="(prefers-color-scheme: light)" srcset="docs/banner-light.svg">
    <img src="docs/banner.svg" alt="nexus" width="100%">
  </picture>
</p>

<p align="center">
  Write a plain Go function. Get REST + GraphQL + WebSocket from it — plus a live dashboard.
</p>

<p align="center">
  <a href="https://github.com/paulmanoni/nexus/releases/latest"><img alt="Release" src="https://img.shields.io/github/v/release/paulmanoni/nexus?sort=semver&color=10b981&labelColor=064e3b"></a>
  <a href="https://pkg.go.dev/github.com/paulmanoni/nexus"><img alt="Go Reference" src="https://pkg.go.dev/badge/github.com/paulmanoni/nexus.svg"></a>
  <a href="go.mod"><img alt="Go version" src="https://img.shields.io/github/go-mod/go-version/paulmanoni/nexus?color=10b981&labelColor=064e3b"></a>
  <a href="https://deps.dev/go/github.com%2Fpaulmanoni%2Fnexus"><img alt="Dependencies" src="https://img.shields.io/badge/deps.dev-insights-10b981?labelColor=064e3b"></a>
  <a href="#license"><img alt="License: MIT" src="https://img.shields.io/badge/license-MIT-10b981?labelColor=064e3b"></a>
</p>

<p align="center">
  <a href="#install">Install</a> &#183;
  <a href="#60-second-start">60-second start</a> &#183;
  <a href="#write-your-first-handler">First handler</a> &#183;
  <a href="#the-dashboard">Dashboard</a> &#183;
  <a href="#going-further">Going further</a>
</p>

---

**nexus** is a Go framework. You write one plain function; nexus exposes it over
**REST, GraphQL, and WebSocket** from the same signature, wires up dependencies for
you, and ships a live **dashboard** at `/__nexus/` that draws your whole app and shows
traffic in real time. No code generation, no schema files, and **no Node.js** for the
frontend. The HTTP router is pluggable behind a small seam — the default backend is the
standard library (`net/http`, zero third-party router deps); [chi](https://github.com/go-chi/chi)
and [Gin](https://github.com/gin-gonic/gin) are opt-in via `nexus.WithRouter(...)` (the Gin
adapter is a separate module, so gin never enters a default build's dependency graph).

## Install

```bash
go install github.com/paulmanoni/nexus/cmd/nexus@latest
```

Needs Go 1.26+. That's it — pure Go, no C toolchain, no npm.

## 60-second start

```bash
nexus new my-app      # scaffold a runnable app (answer a few prompts, or add --yes)
cd my-app
nexus dev             # run it with live reload + dashboard
```

Open **http://localhost:8080/__nexus** to see the dashboard. Your API is live too — try
the GraphQL IDE at **http://localhost:8080/graphql**.

## Write your first handler

Create `main.go`:

```go
package main

import (
    "context"

    "github.com/paulmanoni/nexus"
)

// 1. A service — just a struct with a constructor.
type Greeter struct{}

func NewGreeter() *Greeter { return &Greeter{} }

// 2. A handler — a normal method. p.Args holds your typed input.
type HelloArgs struct{ Name string }

func (g *Greeter) SayHello(_ context.Context, p nexus.Params[HelloArgs]) (string, error) {
    return "Hello, " + p.Args.Name + "!", nil
}

// 3. Wire it up.
func main() {
    nexus.Run(
        nexus.Config{
            Server:        nexus.ServerConfig{Addr: ":8080"},
            Dashboard:     nexus.DashboardConfig{Enabled: true, Name: "Demo"},
            Introspection: true, // open /__nexus (nexus dev sets this for you)
        },
        nexus.Module("hello",
            nexus.Provide(NewGreeter),
            nexus.AsQuery((*Greeter).SayHello),                  // GraphQL
            nexus.AsRest("POST", "/hello", (*Greeter).SayHello), // REST
        ),
    )
}
```

Run it with `go run .` (or `nexus dev`), then call it any way you like:

```bash
# GraphQL
curl localhost:8080/graphql -d '{"query":"{ sayHello(name:\"world\") }"}'

# REST
curl localhost:8080/hello -d '{"name":"world"}'
```

Same function, three transports. Add `nexus.AsMutation(...)` for a GraphQL mutation or
`nexus.AsWS(...)` for a WebSocket event — all read the same signature.

## Skip the wiring: `//@` decorators

Rather than maintain a `nexus.Module(...)` list, you can annotate handlers with `//@`
doc comments and let nexus generate the wiring. Same functions — registered from the
comment above them:

```go
package users

import "github.com/paulmanoni/nexus"

//@provide
func NewService(app *nexus.App) *Service { return &Service{app.Service("users")} }

type GetArgs struct {
    ID string `uri:"id"` // REST path params bind via the `uri` tag
}

//@rest GET /users/:id
func NewGetUser(s *Service, p nexus.Params[GetArgs]) (*User, error) { ... }

//@query
//@auth Requires("ADMIN")
func NewSearchUsers(s *Service, p nexus.Params[SearchArgs]) ([]User, error) { ... }
```

Your `main` keeps **no handler list at all**:

```go
func main() { nexus.Boot() } // or nexus.Run(cfg, …) — no nexus.Module(...) needed
```

Then iterate or build:

```bash
nexus dev                       # injects the wiring on the fly — nothing written to disk
nexus generate handlers ./...   # OR: commit nexus_handlers_gen.go for plain go build/install
```

`nexus generate handlers` scans your packages, writes the `nexus.AsRest/AsQuery/Provide`
calls into a generated file per package, and wires every annotated package into the build
— so `main` stays `nexus.Boot()`. Each package becomes a dashboard **module** automatically.

Annotations: `//@provide`, `//@rest M P`, `//@query`, `//@mutation`, `//@subscription`,
`//@ws P T`, `//@worker N`, plus `//@auth Required|Requires("X")` and `//@use <mw>`. It's
purely additive — coexists with explicit `nexus.Module(...)`, opt out per handler any time.
Extensions ship their own too, e.g. `//@inertia.Page "GET" "/dashboard" "Dashboard"`.

## The dashboard

Visit `/__nexus/` (enabled above) and you get a live map of your app, built automatically
from what you registered — no setup:

![Architecture dashboard](docs/dashboard-signal.png)

- **See everything** — every module, endpoint, resource, worker, and cron, with the
  dependency edges between them. Real traffic pulses on the wires.
- **Click any node** — the Inspector shows its endpoints, auth rules, recent traces, crons,
  and rate limits, with a built-in tester to fire requests.
- **It's live** — updates stream over a WebSocket, so it's always current and costs your
  API nothing per request.

> The dashboard is **off by default in production** (every `/__nexus/*` path 404s) — turn it
> on with `Dashboard.Enabled` + `Introspection: true`. `nexus dev` turns it on for you.

## A little more

Add cross-transport middleware right where you register a handler — it works the same on
REST, GraphQL, and WS:

```go
nexus.AsMutation(NewCreateOrder,
    auth.Required(),                 // 401 if not logged in
    auth.Requires("ROLE_ADMIN"),     // 403 if missing the role
    ratelimit.PerUser(100, time.Minute),
)
```

Generate a full CRUD API (REST + GraphQL) from a struct:

```go
type Todo struct {
    ID    uuid.UUID `nexus:"key"`
    Title string
    Done  bool
}

nexus.Module("todos", nexus.ProvideCRUD[Todo]("todos"))
// → GET/POST /todos, GET/PUT/DELETE /todos/:id, plus GraphQL queries + mutations
```

Add a frontend with **no Node.js required** — nexus ships an embedded "Vite for Go":

```bash
nexus new my-app --frontend vue   # or react
nexus dev                         # SPA with hot-reload on :5173, API on :8080
```

## Client SDK

Flip one switch and nexus generates **and** serves a typed JS/TS client for all three
transports — no npm package, no codegen step to run:

```toml
[runtime]
sdk = true
```

Then, in your frontend:

```ts
import { NexusClient } from 'nexus-client'

const nx = new NexusClient()

await nx.rest('GET', '/pets/:id', { id })   // REST
await nx.query('searchPets', { q: 'cat' })  // GraphQL
await nx.auth.login({ username, password }) // auth flow
nx.ws('/events').on('chat.message', render) // WebSocket
```

Vue composables (`useQuery`, `useMutation`, `useAuth`, …) and React hooks ship alongside.
`sdk = true` activates only under `nexus dev` or when `introspection = true`, so a
locked-down production binary never exposes the API surface from this flag alone.

**Secure by default.** Tokens live in memory (cleared on reload) so an XSS can't lift a
long-lived credential — opt into `localStorage` explicitly when you want persistence.
Cookie-based auth gets automatic CSRF double-submit, and the SDK routes
(`/__nexus/client/*`) sit behind the same introspection gate as the dashboard.

Tune the auth wiring from `auth.Config` — where the login token lives and the CSRF pair
(values shown are the defaults):

```go
auth.Module(auth.Config{
    // ... schemes ...
    LoginTokenField: "data.token",  // dotted path to the token in a login response
    CSRFCookie:      "csrftoken",   // Django/Laravel convention
    CSRFHeader:      "X-CSRFToken",
})
```

For production, vendor the SDK at build time instead of serving it at runtime:
`nexus client --out ./web/sdk`.

## Built in: security, storage, passwords

Three things every real app needs — each a config switch or one typed `Bind`, no
third-party SDKs pulled into your build.

**Security headers + CSRF.** The safe response headers (`X-Frame-Options`,
`X-Content-Type-Options`, `Referrer-Policy`) are **on by default** for every app. CSRF is
one line — off by default because a token-authenticated API isn't CSRF-vulnerable; turn it
on when you serve cookie/session HTML forms (a template engine, or Inertia):

```toml
[runtime.middleware.security]
csp  = "default-src 'self'"   # opt-in Content-Security-Policy
hsts_max_age = 31536000       # opt-in HSTS (once you serve https)
csrf = true                   # opt-in double-submit CSRF
```

**File storage — local ↔ S3, no code change.** Talk to one `Disk` interface; the backend
is chosen by config. The S3 driver speaks to any S3-compatible store (AWS, MinIO, R2,
Spaces) directly over HTTPS with built-in SigV4 signing — **no AWS SDK is linked**.

```go
type Uploads struct{ *storage.Manager }

nexus.Run(cfg, storage.Bind[Uploads]("uploads", func() storage.Config {
    return storage.Config{Driver: "local", Root: "./var/uploads"} // or Driver:"s3", Bucket, Region, …
}))

// in a handler (Uploads embeds the disk):
u.Put(ctx, "avatars/1.png", r, storage.WithContentType("image/png"))
url, _ := u.SignedURL(ctx, "avatars/1.png", 15*time.Minute)   // presigned GET
```

**Password auth — Django-style, fully swappable.** Pluggable password hashers (bcrypt
default; argon2id / pbkdf2 also verify, with transparent rehash-on-login), password
validators, and credential-login backends tried in order:

```go
store := auth.NewMemoryUserStore()               // or your own GORM / API-backed UserStore
store.CreateUser("alice", "s3cret-pw", "ADMIN")

id, err := auth.Authenticate(ctx,
    auth.Password{Username: "alice", Password: "s3cret-pw"}, auth.NewModelBackend(store))
// wrong password OR unknown user → auth.ErrInvalidCredentials (no user enumeration)

auth.ValidatePassword(ctx, pw, id, auth.DefaultValidators()...) // MinLength, NotCommon, …
```

## Going further

When you're ready for more, each area has its own focused guide:

| Topic | Where |
|---|---|
| Every feature, inline | `nexus docs` (e.g. `nexus docs handlers`, `nexus docs dashboard`) |
| Auth: passwords, login backends, OAuth2 | `nexus docs auth`, `extension/auth`, `extension/oauth2` |
| Security headers + CSRF | `nexus docs security` |
| File & object storage (local + S3) | `nexus docs storage` |
| Production: ports, scopes, TLS, mTLS | hide the dashboard from the internet with named listeners + the introspection gate |
| Typed RPC between apps (peer mesh) | [extension/peer/README.md](extension/peer/README.md) |
| Config server + `nexus.toml` | [extension/config/README.md](extension/config/README.md) |
| Runnable sample apps | the [`examples/`](examples) folder (`petstore`, `petstore-spa`, `pubsub`, `wsecho`, `graphapp`, `bigtopo`) |

A couple of handy extras:

- **`nexus.HideFromDashboard()`** — drop an internal endpoint from the dashboard while it keeps serving.

## License

MIT.
