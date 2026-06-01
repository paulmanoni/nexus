# nexus — framework guide for Claude Code

nexus is a Go backend framework: typed reflective handlers over REST + GraphQL +
WebSocket, fx-based dependency injection, a **Vite**-built embedded frontend, and a
live introspection dashboard at `/__nexus`. This file tells you how to use every
feature. Verify APIs against the installed version; `nexus docs <topic>` prints an
inline quick-reference for any feature (`nexus docs --list` for the topic list).

Import path: `github.com/paulmanoni/nexus`. Pure-Go build — no CGO, no build tags.
Node/npm are needed only at build & dev time for the frontend; the runtime is a
single Go binary with the SPA embedded.

---

## 1. Frontend (Vite, embedded SPA)

nexus serves a Vue/React SPA built with **Vite**. The frontend is a standard,
npm-managed Vite project under `web/`; `nexus build` runs `vite build` and `go build`
embeds the output (`web/dist`) into the binary via `//go:embed`. Node/npm are
build- and dev-time dependencies only — **the runtime is a single Go binary** with no
Node and no `node_modules`. Because it's ordinary Vite, any Vite plugin, Tailwind,
PostCSS, or component library works.

### Layout
```
web/
  index.html            # Vite entry HTML (references /src/main.ts)
  vite.config.ts        # base '/'; server.proxy is managed by `nexus dev`
  tsconfig.json
  package.json          # npm-managed deps + scripts (dev/build/preview)
  package-lock.json
  src/
    main.ts             # entry
    App.vue             # (or App.tsx for React)
  node_modules/         # npm-installed (gitignored)
  dist/                 # vite build OUTPUT, embedded in the binary
    index.html          # a committed stub ships so the first `go build` compiles
```
The dir is `web/` by default; override with `NEXUS_FRONTEND_DIR`.

### main.go wiring
```go
import "embed"

//go:embed all:web/dist
var webFS embed.FS

func main() {
    cfg := nexus.MustLoadConfig()
    opts := nexus.MustLoadExtensions()
    opts = append(opts, nexus.ServeFrontend(webFS, "web/dist"), /* modules… */)
    nexus.Run(cfg, opts...)
}
```
`ServeFrontend(fs, root, opts...)` is SPA-aware: extensionless paths fall back to
`index.html`, `/assets/*` gets immutable cache, and REST/GraphQL/WS routes win on
conflict. Mount under a sub-path with `nexus.FrontendAt("/admin")`. Boot fails fast
if `index.html` is missing in the FS — which is why the scaffold commits a
`web/dist/index.html` stub so the first `go build` (before any `vite build`) works.

### How frontend packages are managed (npm + Vite)

It's a normal Vite project, so dependency management is ordinary npm:
```
cd web
npm install                 # install deps into web/node_modules (one-time / on clone)
npm install <pkg>           # add a runtime dep
npm install -D <pkg>        # add a dev dep (a Vite plugin, Tailwind, etc.)
```
`package.json` is the source of truth; `package-lock.json` pins the tree. Commit both;
`web/node_modules` and `web/dist/*` (except the committed `index.html` stub) are
gitignored.

### Build / serve commands
```
nexus dev                # go run + Vite dev server (HMR) — see below
nexus build              # npm install (if needed) + vite build → web/dist,
                         #   then go build embeds it via //go:embed
```
`nexus build` skips the frontend step entirely when there's no `web/package.json`
(a pure-Go app). It runs `npm ci` when a lockfile is present, else `npm install`,
only when `web/node_modules` is missing.

### `nexus dev` — the dev model (IMPORTANT)
`nexus dev` runs **`npm run dev` (the Vite dev server)** alongside `go run`:
- The **SPA is served by Vite on `http://localhost:5173/`** with HMR — open THAT for
  the frontend.
- The **Go app + dashboard stay on `:8080`** (or your `addr`).
- nexus injects a managed proxy block into `web/vite.config.ts` (between
  `// @nexus:proxy-start` / `// @nexus:proxy-end` markers) so `/__nexus`, `/graphql`,
  `/oauth`, and `/ws` reach the Go app from the Vite origin. Don't hand-edit between
  those markers — `nexus dev` rewrites them.
- Override the dev-server command with `--frontend-cmd` (default `npm run dev`).

So in dev: **frontend → :5173, dashboard/API → :8080.** Don't expect the SPA on the
Go app port during dev. In production the embedded `web/dist` is served at the app
port via `ServeFrontend`.

### Scaffold a frontend
```
nexus new myapp --frontend vue      # fresh app with a web/ Vite project
nexus init --frontend vue           # add a Vite frontend to an EXISTING project
                                    #   (writes web/, patches main.go)
```
After scaffolding: `cd web && npm install`, then `nexus dev`.

---

## 2. App entry & config (`nexus.toml`)

`main.go` loads runtime config from `nexus.toml` via `nexus.MustLoadConfig()` and
`[extensions.*]` blocks via `nexus.MustLoadExtensions()`. Edit settings in the file,
not in code; absent keys fall back to framework defaults.

**All runtime keys live under `[runtime]`** (or a `[runtime.<sub>]` table) — a key at
the top level is silently ignored. `[databases.*]` and `[extensions.*]` are top-level.

```toml
[runtime]
environment    = "development"          # development | staging | production
introspection  = true                   # opens /__nexus (OFF by default → 404s)
introspection_networks = ["10.0.0.0/8"] # allowed even when introspection is off
trace_capacity = 1000                    # request-trace ring buffer (0 = off)

[runtime.server]
addr = ":8080"
route_prefix = ""                        # prepended to every REST/GraphQL/WS route

[runtime.server.listeners.admin]         # optional multi-scope listeners
addr  = "127.0.0.1:7000"
scope = "admin"                          # public | internal | admin

[runtime.dashboard]
enabled = true
name    = "My App"

[runtime.graphql]
path = "/graphql"

[runtime.middleware.cors]
allow_origins = ["*"]

[runtime.middleware.ratelimit]
rpm = 600
burst = 50

# Databases — TOP LEVEL (not under [runtime]); wired in code via
# nexus.DatabaseFromConfig[T]("name"). Inline values OR a config-server key_prefix.
[databases.main]
driver   = "postgres"
host     = "localhost"
port     = "5432"
user     = "postgres"
password = "${DB_PASSWORD}"              # ${ENV} expanded at load
name     = "myapp"
sslmode  = "disable"
default  = true

# Config server (optional) — decoded by MustLoadExtensions; values via nexus.Get.
[extensions.config]
endpoint = "http://localhost:8078"
identity = "myapp"
profile  = "default"
```
`nexus docs nexustoml` documents every key. You can also pass `nexus.Config{...}`
inline to `nexus.Run` instead of the file.

**Introspection gate:** the entire `/__nexus` surface (dashboard + JSON APIs) is
**off by default and 404s** so production binaries are locked down. Set
`introspection = true` (or `Config.Introspection`) for dev; in production prefer an
admin CIDR allowlist (`introspection_networks = ["10.0.0.0/8"]`). `nexus dev` runs
with it open.

---

## 3. Modules & dependency injection

```go
var Module = nexus.Module("billing",     // stamps "billing" on every endpoint inside
    nexus.Path("/billing"),               // REST + GraphQL prefix in one
    nexus.Provide(NewBillingService),     // constructor(s) into the fx graph
    nexus.AsRest("POST", "/charge", NewCharge),
    nexus.AsQuery(NewListInvoices),
)
```
The dashboard's Architecture graph **groups by module**. Option helpers:
- `nexus.Provide(fns...)` — constructors into the DI graph.
- `nexus.ProvideService(fn)` — Provide + draw service→service/resource edges from the
  constructor's params automatically.
- `nexus.ProvideResources(fns...)` — Provide + auto-register `NexusResourceProvider`s.
- `nexus.Supply(vals...)` — ready-made values. `nexus.Invoke(fn)` — startup side effect.
- `nexus.Path("/x")` — module URL prefix (REST + GraphQL). `nexus.RoutePrefix("/x")` —
  REST-only prefix.

---

## 4. Services

A service is a typed wrapper around `*nexus.Service` so fx routes by type and the
dashboard groups handlers under it:
```go
type BillingService struct{ *nexus.Service }

func NewBillingService(app *nexus.App) *BillingService {
    return &BillingService{app.Service("billing").Describe("Billing")}
}
```
Attach resources: `svc.Using("main", "cache")` / `svc.UsingDefaults()` /
`svc.Attach(r)`. Override GraphQL mount: `svc.AtGraphQL("/billing/graphql")`.

---

## 5. Reflective handlers

Every transport uses the same shape:
```go
func NewOp(svc *XService, deps..., p nexus.Params[ArgsStruct]) (*Response, error)
```
- First `*Service`-wrapper dep grounds the op under that service (omit in
  single-service apps, or pin with `nexus.OnService[*XService]()`).
- Last param `nexus.Params[T]` exposes `.Context` and `.Args`.
- Return `(T, error)` — `T` is the GraphQL type / REST JSON body.
- `NewListPets` → op name `ListPets` (the `New` prefix is stripped).
- Struct tags drive schema + validation: `graphql:"title,required" validate:"required,len=3|120"`, `path:"id"` for REST path params.

### REST
```go
type GetArgs struct { ID string `path:"id"` }
nexus.AsRest("GET", "/users/:id", NewGet)
```

### GraphQL (auto-mounted on `/graphql`)
```go
nexus.AsQuery(NewSearchUsers)
nexus.AsMutation(NewCreateAdvert, auth.Required(), auth.Requires("ROLE_CREATE"))
```
Field name = constructor name minus `New`, first letter lowercased
(`NewSearchUsers` → `searchUsers`). Fields are partitioned by service; service-less
handlers mount on a default partition.

**Related fields without N+1 — `LoadField`.** Add a batched (dataloader) field to a
Go type so nested GraphQL resolvers don't fire one query per parent:
```go
nexus.LoadField[models.AcademicProgramme, int64, *models.AcademicLevel](
    "level",                                                   // GraphQL field name
    func(p models.AcademicProgramme) int64 { return *p.LevelID }, // parent → key
    func(ctx context.Context, ids []int64, db *resources.DB) (map[int64]*models.AcademicLevel, error) {
        var rows []models.AcademicLevel
        db.GetDB().Where("id IN (?)", ids).Find(&rows)
        out := make(map[int64]*models.AcademicLevel, len(rows))
        for i := range rows { out[*rows[i].ID] = &rows[i] }   // map EACH key → its row
        return out, nil
    },
)
```
`LoadField[Parent, Key, Child]("field", keyFn, fetch)` — the framework collects every
parent's key across the query and calls `fetch` ONCE per batch. `fetch` is one of:
(a) `func(ctx, []Key) (map[Key]Child, error)` — no deps; (b) a constructor returning
`dataloader.Fetch[Key, Child]` (fx-injected); (c) inline with trailing fx-injected deps
(`func(ctx, []Key, db *DB, …)`). Parent's SDL name is its Go type name. (Watch the loop:
key each result by its own id — don't overwrite as the example's inner loop did.)

### WebSocket
```go
func NewChatSend(svc *ChatService, sess *nexus.WSSession, p nexus.Params[ChatPayload]) error {
    sess.EmitToRoom("chat.message", p.Args, "lobby"); return nil
}
nexus.AsWS("/events", "chat.send", NewChatSend, auth.Required())
```
Multiple `AsWS` on one path share a connection, dispatched by the envelope `type`.
Wire format: `{ "type": "...", "data": {...}, "timestamp": ... }`. `*WSSession`:
`Send / Emit / EmitToUser / EmitToRoom / EmitToClient`, `JoinRoom / LeaveRoom`.
Built-in `ping/authenticate/subscribe/unsubscribe` are handled by the hub.

---

## 6. Resources (databases / caches / queues)

**Typed helpers (idiomatic).** Define a wrapper that embeds the framework manager,
then register it as an `Option`. The framework owns the lifecycle (Start on boot,
Stop on shutdown), provides `*YourType` into the DI graph, and registers it as a
dashboard resource (shown red if down):
```go
import (
    "github.com/paulmanoni/nexus"
    "github.com/paulmanoni/nexus/db"
    "github.com/paulmanoni/nexus/extension/cache"
)

type DB struct{ *db.Manager }          // MUST embed *db.Manager
type SourceDB struct{ *db.Manager }
type CacheManager struct{ *cache.Manager }

func DatabaseOptions() []nexus.Option {
    return []nexus.Option{
        nexus.DatabaseFromConfig[DB]("main"),        // reads [databases.main] from nexus.toml
        nexus.DatabaseFromConfig[SourceDB]("source"),
    }
}

func CacheOption() nexus.Option {
    return nexus.Cache[CacheManager]("session",
        func() *cache.Config { return &cache.Config{} },
        nexus.WithCacheDefault(),
        nexus.WithCacheDescription("Redis + in-memory fallback"))
}
```
- `nexus.DatabaseFromConfig[T]("name", opts...)` — reads `[databases.name]` (needs
  `MustLoadConfig` to have run); `T` embeds `*db.Manager`.
- `nexus.Database[T]("name", func() db.Config {…}, opts...)` — inline config (no TOML).
- `nexus.Cache[T]("name", func() *cache.Config {…}, opts...)` — `T` embeds `*cache.Manager`.
- Options: `WithDatabaseDefault/Details/Description`, `WithCacheDefault/Description`.

Handlers/services then take `*DB`, `*CacheManager` as constructor params (fx injects).

**Manual API** (for queues or full control): `resource.NewDatabase/NewCache/NewQueue(name,
desc, details, healthy, opts...)` with `resource.AsDefault()/DependsOn(...)/WithDetails(fn)`;
register via `app.Register(r)`, or implement `NexusResources() []resource.Resource` on a
constructor param for auto-detection. Live usage edges: `app.OnResourceUse(target)`.

### Config server (`nexus.Get`)
With `[extensions.config]` wired (blank-import `_ ".../extension/config"` + the TOML
block), read values anywhere:
```go
addr := nexus.Get[string]("server.addr")
port := nexus.Get[int]("db.port", 5432)              // 2nd arg = default
ttl  := nexus.Get[time.Duration]("cache.ttl", 5*time.Minute)
```
Databases can pull secrets from it via `key_prefix` instead of inline values.

---

## 7. Workers & crons

```go
nexus.AsWorker("cache-invalidation",
    func(ctx context.Context, db *DB, cache *CacheManager) error {
        for { select { case <-ctx.Done(): return nil; case n := <-listener.Notify: handle(n) } }
    })   // first param MUST be context.Context; the rest are fx-injected

app.Cron("refresh", "@every 30s").
    Describe("warm the cache").
    Service("billing").                 // groups the cron→service edge
    Handler(func(ctx context.Context) error { return nil })
```
Worker/cron resource + service deps are auto-detected for the graph.

---

## 8. Auth & OAuth2

```go
import "github.com/paulmanoni/nexus/extension/auth"

auth.Module(auth.Config{
    Resolve: func(ctx context.Context, tok string) (*auth.Identity, error) {
        u, err := validate(ctx, tok); if err != nil { return nil, err }
        return &auth.Identity{ID: u.ID, Roles: u.Roles, Extra: u}, nil
    },
    Cache: auth.CacheFor(15 * time.Minute),
})
```
Per-op gates (cross-transport): `auth.Required()` (401 if missing),
`auth.Requires("ROLE_X")` (403). Extractors: `auth.Bearer()`, `auth.Cookie(name)`,
`auth.APIKey(header)`, `auth.Chain(...)`. Typed user in a handler:
`u, ok := auth.User[MyUser](p.Context)`. Logout: take `*auth.Manager`, call
`Invalidate(token)` / `InvalidateByIdentity(id)`. A full OAuth2 server is
`oauth2.Module(oauth2.Config{...})` (`extension/oauth2`) — password grant →
JWT access/refresh at `/oauth/token`.

---

## 9. Dashboard (`/__nexus`)

Live introspection UI (needs introspection open — see §2). Tabs: **Architecture**
(graph grouped by module — drill into a module to see its endpoints/services/
resources/workers/crons; collapsed at scale; edges bundle with counts; ELK layout,
minimap, dark mode; live traffic pulses), Endpoints (per-op tester), Crons, Rate
limits, Auth, Traces. Gate it behind your own middleware via
`Config.Middleware.Dashboard`.

---

## 10. Client SDK (browser → app)

A typed JS/TS SDK + Vue composables served from the binary (no npm package). Enable
with `Config.Client.Enabled = true` or `nexus.ClientUse(client.Config{Enabled:true})`.
Import in the frontend as `nexus-client` (resolved via tsconfig `paths`). See
`nexus docs client`.

---

## 11. CLI cheatsheet

```
nexus new <dir>      Scaffold an app + nexus.toml. --frontend vue|react, --db, --cache,
                     --auth, --module <path>, --yes (no prompts).
nexus init [dir]     Add a Vite frontend (web/) to an existing project. --frontend (req).
nexus dev [dir]      Live dev: Vite SPA+HMR on :5173, app/dashboard on :8080.
                     --frontend-cmd <c> overrides the dev-server command (default npm run dev).
nexus build          npm install (if needed) + vite build → web/dist, then go build
                     embeds it. ONE binary (frontend + Go). -o <path>.
nexus client [--out dir]   Write the embedded JS/TS client SDK to disk.
nexus generate dockerfile   Multi-stage Dockerfile.
nexus docs [topic]   Inline reference. --web opens the README.
nexus pki ...        Generate mTLS certs for the peer mesh.
```
`nexus build` produces ONE binary (frontend + Go). There is no deployment-split CLI.

---

## 12. Conventions & gotchas

- **Pure-Go build** — no `-tags`, no CGO. Building/`nexus dev` for a project with a
  `web/` frontend needs Node + npm on PATH (build/dev only; not at runtime).
- **Dashboard 404s unless `introspection = true`** (or `nexus dev`). It's locked down
  by default for production.
- **In dev the SPA is on `:5173`** (Vite), not the Go app port. `web/dist` is the
  production artifact, built by `nexus build` and embedded.
- **Frontend deps**: ordinary `npm install` in `web/`. Commit `package.json` +
  `package-lock.json`; `web/node_modules` and `web/dist/*` (except the committed
  `index.html` stub) are gitignored.
- **Handler constructors are `NewXxx`**; the `New` prefix is stripped for op names.
- Don't reference `nexus.DeployAs` / `nexus.IfDeployment` — not implemented.
- `nexus docs <topic>` is the authoritative per-feature reference inside the installed
  binary; prefer it when unsure of an exact signature.
