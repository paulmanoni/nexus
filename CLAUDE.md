# nexus — framework guide for Claude Code

nexus is a Go backend framework: typed reflective handlers over REST + GraphQL +
WebSocket, fx-based dependency injection, an embedded **viteless** frontend (a
zero-Node "Vite for Go"), and a live introspection dashboard at `/__nexus`. This file
tells you how to use every feature. Verify APIs against the installed version; `nexus
docs <topic>` prints an inline quick-reference for any feature (`nexus docs --list`).

Import path: `github.com/paulmanoni/nexus`. Pure-Go build — no CGO, no build tags.
**No Node/npm is required** for the frontend, at dev, build, or run time; the runtime
is a single Go binary with the SPA embedded.

---

## 1. Frontend (viteless, embedded SPA)

nexus serves a Vue/React/TS SPA via the embedded **viteless** engine
(`github.com/paulmanoni/viteless`) — a zero-Node implementation of the Vite dev/build
model in Go (esbuild + a WASM QuickJS engine). `nexus build` produces `web/dist` and
`go build` embeds it via `//go:embed`. No npm, no `node_modules`, no Node by default.

**Dependencies & fidelity (auto-detected, highest available wins):**
1. **Real Vite installed** (`web/node_modules/.bin/vite`) → viteless delegates to it
   (100% compat; set `VITELESS_ENGINE=1` to force the viteless engine).
2. **Node on PATH** → viteless's own engine, evaluating `vite.config`/`viteless.config`
   and running real JS plugins via a Node sidecar (full fidelity); Vue/React/Tailwind
   are handled natively.
3. **No Node** → fully zero-Node: deps from the esm.sh CDN (cached), config evaluated
   in QuickJS, native Vue/React/Tailwind, JS-only plugins reported unsupported.

Dependency sourcing is likewise auto: if `web/node_modules` exists it's used
(offline, exact versions); otherwise deps come from esm.sh. `web/package.json` is
optional — when present its versions pin CDN fetches; run `npm install` only to opt
into node_modules / a real Vite.

### Layout
```
web/
  index.html            # entry HTML (references /src/main.ts)
  viteless.config.ts    # optional; imports defineConfig from 'viteless' (vite.config.ts also read)
  tsconfig.json         # paths "@/*"→src; includes viteless-env.d.ts
  viteless-env.d.ts     # ambient types so the editor resolves imports with nothing installed
  src/
    main.ts             # entry
    App.vue             # (or App.tsx for React)
  dist/                 # build OUTPUT, embedded in the binary
    index.html          # a committed stub ships so the first `go build` compiles
```
No `package.json`/`node_modules`/`package-lock.json` by default. The dir is `web/`;
override with `NEXUS_FRONTEND_DIR`.

### main.go wiring
```go
import "embed"

//go:embed all:web/dist
var webFS embed.FS

func main() {
    nexus.Boot(nexus.ServeFrontend(webFS, "web/dist") /*, modules… */)
}
```
`nexus.Boot(opts...)` loads `nexus.toml` automatically — runtime `Config`, every
`[extensions.*]` block, the `[env]` bridge, and the `nexus.Get` value store — then
runs the app. It's sugar for `nexus.Run(nexus.MustLoadConfig(),
append(nexus.MustLoadExtensions(), opts...)...)`; a missing `nexus.toml` is tolerated,
a malformed one panics. Override the path with `NEXUS_CONFIG` or use
`nexus.BootFrom(path, opts...)`. Reach for `nexus.Run(cfg, opts...)` directly when you
build `Config` in Go. (Extension packages still need their blank import — Go links only
imported code; `Boot` removes the load calls, not the imports.)
`ServeFrontend(fs, root, opts...)` is SPA-aware: extensionless paths fall back to
`index.html`, `/assets/*` gets immutable cache, and REST/GraphQL/WS routes win on
conflict. Mount under a sub-path with `nexus.FrontendAt("/admin")`. Boot fails fast if
`index.html` is missing — which is why the scaffold commits a `web/dist/index.html`
stub so the first `go build` (before any build) works.

### Build / serve commands
```
nexus dev                # go run + viteless HMR dev server — see below
nexus build              # viteless build → web/dist, then go build embeds it
```
`nexus build` skips the frontend step when `web/` has no `index.html`/`src` (a pure-Go
app). No npm step — viteless fetches/caches deps itself (or uses node_modules if present).

### `nexus dev` — the dev model (IMPORTANT)
`nexus dev` runs the **viteless HMR dev server** alongside `go run`:
- The **SPA is served on `http://localhost:5173/`** with HMR — open THAT for the
  frontend.
- The **Go app + dashboard stay on `:8080`** (or your `addr`).
- viteless **proxies** every unmatched request (`/__nexus`, `/graphql`, `/oauth`,
  `/ws`, your API) straight to the Go app — no managed `vite.config` proxy block to
  maintain. The Go app's real port is discovered from its startup log.

So in dev: **frontend → :5173, dashboard/API → :8080.** In production the embedded
`web/dist` is served at the app port via `ServeFrontend`.

### Scaffold a frontend
```
nexus new myapp --frontend vue      # fresh app with a web/ viteless project (no install needed)
nexus init --frontend vue           # add a frontend to an EXISTING project (writes web/, patches main.go)
```
After scaffolding: just `nexus dev` (zero install). Deps are fetched on first run.

---

## 2. App entry & config (`nexus.toml`)

`nexus.Boot(opts...)` loads `nexus.toml` automatically (runtime config +
`[extensions.*]` + the `[env]` bridge + the `nexus.Get` value store). Edit settings in
the file, not in code; absent keys fall back to framework defaults. The explicit form
(`nexus.MustLoadConfig()` + `nexus.MustLoadExtensions()` → `nexus.Run`) still works for
apps that build `Config` in Go.

**All runtime keys live under `[runtime]`** (or a `[runtime.<sub>]` table) — a key at
the top level is silently ignored. `[databases.*]` and `[extensions.*]` are top-level.
Any value (including custom sections) is readable via `nexus.Get[T]("section.key")` —
the dotted key mirrors the TOML table path.

```toml
[runtime]
environment    = "development"          # development | staging | production
introspection  = true                   # opens /__nexus (OFF by default → 404s)
introspection_networks = ["10.0.0.0/8"] # allowed even when introspection is off
trace_capacity = 1000                    # request-trace ring buffer (0 = off)
sdk            = true                    # one switch: generate+serve the typed client SDK

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

# Env bridge — TOP LEVEL. [env.*] tables become process environment
# variables AND are exposed to the frontend. The table path after `env.` is
# the variable name, so this sets env vars "client.id" and "client.secret":
[env.client]
id     = "myapp-web"
secret = "${CLIENT_SECRET}"        # ${ENV} expanded; keep real secrets in env
```
**`[env.*]` bridge:** every key under `[env]` is published (a) as a process env
var read by the Go app/extensions via `os.Getenv("client.id")`, and (b) to the
frontend build as `import.meta.env.client.id` (dot form — esbuild substitutes
the dotted member expression; the bracket form `import.meta.env["client.id"]` is
NOT substituted). Nested tables flatten with dots (`[env.a.b] c` → `a.b.c`).
SECURITY: frontend-exposed values land in the browser bundle — only put
client-public data there (an OAuth client id, a public URL), never a real
server secret.

`nexus docs nexustoml` documents every key. You can also pass `nexus.Config{...}`
inline to `nexus.Run` instead of the file.

**Introspection gate:** the entire `/__nexus` surface (dashboard + JSON APIs) is
**off by default and 404s** so production binaries are locked down. Set
`introspection = true` (or `Config.Introspection`) for dev; in production prefer an
admin CIDR allowlist (`introspection_networks = ["10.0.0.0/8"]`). `nexus dev` runs
with it open.

### HTTP router backend (pluggable; stdlib by default)

nexus is **router-agnostic** behind the `github.com/paulmanoni/nexus/httpx` seam.
Handlers and middleware see an `*httpx.Ctx` (a transport-neutral request handle);
the concrete router is an adapter chosen at boot. **The default is the stdlib
`net/http.ServeMux` (`httpx/stdrouter`) — zero third-party router deps, so the
default binary links no gin/sonic/etc.** gin and chi are opt-in:

```go
import (
    "github.com/paulmanoni/nexus"
    "github.com/paulmanoni/nexus/httpx/ginrouter" // or .../httpx/chirouter
)

nexus.Boot(nexus.WithRouter(ginrouter.New()))     // one line; or Config.Router
nexus.Run(cfg, nexus.WithRouter(chirouter.New()))
```

`nexus.WithRouter(...)` (equivalently `Config.Router`) is the only switch — no
nexus.toml key (an adapter must be imported to link anyway). Selecting gin/chi pulls
their dependency trees back in; the stdlib default does not. Route strings use the
canonical `:id` / `*rest` syntax on every backend (chi/std adapters translate).

Chain execution (the `c.Next()` / `c.Abort()` flow, recovery, error accumulation)
lives in `httpx.Ctx`, not the router — so every middleware runs identically on any
backend, and the router only matches paths + returns params. App-level middleware
(`Config.Middleware.Global`, etc.) wraps the whole mux so it runs even on 404/405
(CORS preflight relies on this); per-op middleware runs inside the matched route.

`App.Router() httpx.Router` exposes the live router (replaces the old
`App.Engine() *gin.Engine`). Low-level handlers that need raw HTTP take an
`*httpx.Ctx` parameter (replaces `*gin.Context`); `httpx.H` is the JSON map
shorthand (replaces `gin.H`).

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
    _ "github.com/paulmanoni/nexus/extension/cache/redis" // opt into Redis (omit → memory-only)
)

type DB struct{ *db.Manager }          // MUST embed *db.Manager
type SourceDB struct{ *db.Manager }
type CacheManager struct{ *cache.Manager }

func DatabaseOptions() []nexus.Option {
    return []nexus.Option{
        db.BindFromConfig[DB]("main"),        // reads [databases.main] from nexus.toml
        db.BindFromConfig[SourceDB]("source"),
    }
}

func CacheOption() nexus.Option {
    return cache.Bind[CacheManager]("session",
        func() *cache.Config { return &cache.Config{} },
        cache.WithDefault(),
        cache.WithDescription("Redis + in-memory fallback"))
}
```
The binders live in `db` / `extension/cache` (not the nexus root) so that importing
`nexus` does NOT pull GORM, the SQL drivers, Redis, or Prometheus into the build — an
app pays for those only when it calls a binder. This mirrors `pubsub.Broker`.
- `db.BindFromConfig[T]("name", opts...)` — reads `[databases.name]`; `T` embeds
  `*db.Manager`. The `[databases.*]` lookup is deferred to boot, so it works under
  `nexus.Boot` even though Boot loads the TOML after building option args (a bad/missing
  block still fails fast at boot).
- `db.Bind[T]("name", func() db.Config {…}, opts...)` — inline config (no TOML).
- `cache.Bind[T]("name", func() *cache.Config {…}, opts...)` — `T` embeds `*cache.Manager`.
- Options: `db.WithDefault/WithDetails/WithDescription`, `cache.WithDefault/WithDescription`.
- **The default cache is in-memory only and pulls NO heavy deps** (no go-redis, gocache,
  or Prometheus). Redis is opt-in database/sql-style: blank-import
  `_ ".../extension/cache/redis"` and a `production`-mode Manager keeps a Redis connection
  with transparent memory failover. Without that import the binary never links go-redis.
- Cache-backed metrics (multi-replica counters) are opt-in via
  `Config.Stores.Metrics = cache.NewMetricsStore(mgr)`; the default is an in-process
  memory store with no cache dependency.

Handlers/services then take `*DB`, `*CacheManager` as constructor params (fx injects).

**Manual API** (for queues or full control): `resource.NewDatabase/NewCache/NewQueue(name,
desc, details, healthy, opts...)` with `resource.AsDefault()/DependsOn(...)/WithDetails(fn)`;
register via `app.Register(r)`, or implement `NexusResources() []resource.Resource` on a
constructor param for auto-detection. Live usage edges: `app.OnResourceUse(target)`.

### Config values (`nexus.Get`)
`nexus.Get[T]("key", default...)` reads from, highest priority first: (1) an ENV
override (`db.port` → `DB_PORT`), (2) the `[extensions.config]` snapshot when wired
(hot-reloadable, remote-capable), (3) the **`nexus.toml` base layer** seeded by
`Boot`/`MustLoadConfig`. Layers resolve per-key, so a key absent from a higher layer
falls through. Read anywhere:
```go
addr := nexus.Get[string]("runtime.server.addr")     // straight from nexus.toml
port := nexus.Get[int]("db.port", 5432)              // 2nd arg = default
ttl  := nexus.Get[time.Duration]("cache.ttl", 5*time.Minute)
```
The dotted key mirrors the TOML table path — `[runtime.storage] url` →
`nexus.Get[string]("runtime.storage.url")` — **no extension needed** for plain
nexus.toml reads. Wire `[extensions.config]` (blank-import `_ ".../extension/config"` +
the TOML block) only when you need hot-reload, profiles, or a remote config server;
those values then override the nexus.toml base layer. Databases can pull secrets via
`key_prefix` instead of inline values.

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

The dashboard is **WebSocket-driven, not polled**: `/__nexus/live` pushes one
state snapshot (services, endpoints, resources, workers, stats, crons,
ratelimits, graphqlCache, middlewares, auth) on change + a 5s heartbeat, and
`/__nexus/events` streams traces — gathered only while a client is connected,
so endpoints pay nothing per request. Plugins add their live state to the
snapshot's `extra` map via `dashboard.RegisterSnapshotExtra(name, func() any)`
(auth does this for cached identities) instead of exposing a polled endpoint.

**Exempt an endpoint from the dashboard** with `nexus.HideFromDashboard()` —
a cross-transport per-op option (REST / GraphQL / WS) that drops the endpoint
from `/__nexus/endpoints`, the live snapshot, and the architecture graph while
the route keeps serving normally (dashboard-only, not a 404):
`nexus.AsRest("GET", "/internal/debug", NewDebug, nexus.HideFromDashboard())`.

---

## 10. Client SDK (browser → app)

A typed JS/TS SDK + Vue composables served from the binary (no npm package). It covers
**all three transports** — `nx.rest`, `nx.query`/`nx.mutate` (GraphQL), `nx.ws`, plus
`nx.crud` and `nx.auth.*`. Import in the frontend as `nexus-client` (resolved via tsconfig
`paths`). See `nexus docs client`.

**Simplest enable — one switch (`sdk = true`):** set `Config.SDK` (or `[runtime] sdk =
true` in nexus.toml) and nexus generates + serves the full typed SDK and, when a frontend
dir is present, dumps the SDK files + wires tsconfig so `import 'nexus-client'` resolves
with types — no `client.Config` ceremony. PocketBase-style. It activates only under `nexus
dev` OR when `introspection = true`, so a locked-down production binary never exposes the
API surface from this flag alone. For finer control (custom path, route middleware,
explicit OutDir, per-deployment gating) set `Config.Client` / `nexus.ClientUse(...)`
directly instead.

---

## 11. CLI cheatsheet

```
nexus new <dir>      Scaffold an app + nexus.toml. --frontend vue|react, --db, --cache,
                     --auth, --module <path>, --yes (no prompts).
nexus init [dir]     Add a frontend (web/) to an existing project. --frontend (req).
nexus dev [dir]      Live dev: viteless SPA+HMR on :5173, app/dashboard on :8080.
nexus build          viteless build → web/dist, then go build embeds it. No npm.
                     ONE binary (frontend + Go). -o <path>.
nexus client [--out dir]   Write the embedded JS/TS client SDK to disk.
nexus generate dockerfile   Multi-stage Dockerfile.
nexus docs [topic]   Inline reference. --web opens the README.
nexus pki ...        Generate mTLS certs for the peer mesh.
```
`nexus build` produces ONE binary (frontend + Go). There is no deployment-split CLI.

---

## 12. Conventions & gotchas

- **Pure-Go build** — no `-tags`, no CGO. The frontend needs **no Node/npm** at dev,
  build, or run time (viteless is embedded). Node/Vite are used only if present, for
  higher fidelity.
- **Dashboard 404s unless `introspection = true`** (or `nexus dev`). It's locked down
  by default for production.
- **In dev the SPA is on `:5173`** (viteless), not the Go app port. `web/dist` is the
  production artifact, built by `nexus build` and embedded.
- **Frontend deps**: none to install by default — viteless fetches from esm.sh (cached)
  or uses `web/node_modules` if you ran `npm install`. `web/dist/*` (except the committed
  `index.html` stub) and any `web/node_modules` are gitignored.
- **Handler constructors are `NewXxx`**; the `New` prefix is stripped for op names.
- Don't reference `nexus.DeployAs` / `nexus.IfDeployment` — not implemented.
- `nexus docs <topic>` is the authoritative per-feature reference inside the installed
  binary; prefer it when unsure of an exact signature.
