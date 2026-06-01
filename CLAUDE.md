# nexus — framework guide for Claude Code

nexus is a Go backend framework: typed reflective handlers over REST + GraphQL +
WebSocket, fx-based dependency injection, a **zero-Node** embedded frontend, and a
live introspection dashboard at `/__nexus`. This file tells you how to use every
feature. Verify APIs against the installed version; `nexus docs <topic>` prints an
inline quick-reference for any feature (`nexus docs --list` for the topic list).

Import path: `github.com/paulmanoni/nexus`. Build with the **`vue` tag + CGO** when
the app has `.vue` files (the SFC compiler is QuickJS/CGo):
`CGO_ENABLED=1 go run -tags vue .`.

---

## 1. Frontend (zero-Node, embedded SPA)

nexus serves a Vue/React SPA with **no Node.js, no `package.json` scripts, no vite,
and no `node_modules` at build or runtime**. Dependencies come from esm.sh into a
local cache (`~/.nexus/cache`), pinned in `nexus.lock`, and bundled by an
esbuild-in-Go pipeline.

### Layout
```
islands.src/            # SOURCE you edit (Vite-convention "src")
  main.ts               # entry
  App.vue
  node_modules/         # EDITOR-ONLY types (gitignored) — never a build input
islands/                # BUILD OUTPUT, embedded in the binary
  index.html            # SPA shell (checked in; references /main.js)
nexus.lock              # resolved frontend deps + integrity (authoritative)
package.json            # human/IDE/Renovate-facing dep list (no scripts)
tsconfig.json           # IDE-only (esbuild ignores it)
nexus-shims.d.ts        # IDE ambient module stubs (fallback before `nexus types`)
```
Folder names are overridable via `NEXUS_ISLANDS_SRC` / `NEXUS_ISLANDS_OUT`.

### main.go wiring
```go
import "embed"

//go:embed all:islands
var islandsFS embed.FS

func main() {
    cfg := nexus.MustLoadConfig()
    opts := nexus.MustLoadExtensions()
    opts = append(opts, nexus.ServeFrontend(islandsFS, "islands"), /* modules… */)
    nexus.Run(cfg, opts...)
}
```
`ServeFrontend(fs, root, opts...)` is SPA-aware: extensionless paths fall back to
`index.html`, `/assets/*` gets immutable cache, and REST/GraphQL/WS routes win on
conflict. Mount under a sub-path with `nexus.FrontendAt("/admin")`.

### Workflow (the commands you'll use)
```
nexus add vue            # fetch a dep from esm.sh → ~/.nexus/cache + nexus.lock
                         #   (also: nexus add react react-dom, nexus add pinia, …)
nexus types              # editor IntelliSense WITHOUT npm: mirror each dep's real
                         #   .d.ts from esm.sh into islands.src/node_modules
                         #   (gitignored, editor-only). nexus dev refreshes it.
nexus dev                # live dev server + HMR (see below)
nexus build              # production bundle → islands/, embedded via //go:embed
```
Add a dependency, then import it in `islands.src/*` — no install step beyond
`nexus add`. `nexus remove <pkg>` drops it.

### `nexus dev` — the dev model (IMPORTANT)
`nexus dev` runs an **unbundled native-ESM dev server** (the "viteless" path):
- The **SPA is served live on `http://localhost:5173/`** with HMR — open THAT for
  the frontend. It serves `islands.src` modules directly (one module per URL, one
  Vue instance → real state-preserving HMR) and proxies API calls to the Go app.
- The **Go app + dashboard stay on `:8080`** (or your `addr`).
- `islands/` is NOT rebuilt during dev — it's a production artifact (`nexus build`).
  The dev server serves the shell from `islands/index.html`, rewriting its
  production entry (`/main.js`) to the source entry (`/main.ts`) on the fly.

So in dev: **frontend → :5173, dashboard/API → :8080.** Don't expect the SPA on the
Go app port during dev.

### `.vue` / SFC compilation
`.vue` single-file components are compiled by an in-process QuickJS-backed
`@vue/compiler-sfc`, which needs **CGO + the `vue` build tag**:
```
CGO_ENABLED=1 go run -tags vue .
CGO_ENABLED=1 go install -tags vue github.com/paulmanoni/nexus/cmd/nexus@latest
```
Without the tag, `nexus dev` errors when it finds `.vue` sources.

### Editor IntelliSense
`nexus-shims.d.ts` (ambient `declare module` stubs) silences TS errors with no
network. For REAL types (autocomplete, signatures) run **`nexus types`** — it writes
a types-only `islands.src/node_modules` the editor resolves against; the build stays
zero-Node and ignores it. There is ONE editor `node_modules`, under `islands.src/`
— not at the project root.

### Scaffold a frontend
```
nexus new myapp --frontend vue      # fresh app with islands.src + islands
nexus init --frontend vue           # add frontend to an EXISTING project
                                    #   (writes islands.src/, patches main.go)
```

---

## 2. App entry & config (`nexus.toml`)

`main.go` loads runtime config from `nexus.toml` via `nexus.MustLoadConfig()` and
`[extensions.*]` blocks via `nexus.MustLoadExtensions()`. Edit settings in the file,
not in code; absent keys fall back to framework defaults.

```toml
# nexus.toml
environment    = "development"          # development | staging | production
introspection  = true                   # opens /__nexus (OFF by default → 404s)
trace_capacity = 1000                    # request-trace ring buffer (0 = off)

[server]
addr = ":8080"

[dashboard]
enabled = true
name    = "My App"

[graphql]
path = "/graphql"
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

```go
import "github.com/paulmanoni/nexus/resource"

func (m *DB) NexusResources() []resource.Resource {     // implement on your wrapper
    return []resource.Resource{
        resource.NewDatabase("main", "GORM — postgres",
            map[string]any{"engine": "postgres"}, m.IsConnected, resource.AsDefault()),
    }
}
```
`NewDatabase` / `NewCache` / `NewQueue(name, desc, details, healthy, opts...)`;
options `resource.AsDefault()`, `resource.DependsOn(...)`, `resource.WithDetails(fn)`.
Register with `app.Register(r)` or `nexus.ProvideResources`. A constructor param that
implements `NexusResources() []resource.Resource` is auto-detected → graph edges.
Live runtime usage: `app.OnResourceUse(target)` records `target.UsingCtx(ctx, name)`.

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
nexus init [dir]     Add frontend scaffolding to an existing project. --frontend (req).
nexus add <pkg>...   Fetch a frontend dep from esm.sh → ~/.nexus/cache + nexus.lock.
nexus types          Editor IntelliSense from nexus.lock (no npm).
nexus dev [dir]      Live dev: SPA+HMR on :5173, app/dashboard on :8080.
nexus build          Build a single binary (frontend bundled + embedded). -o <path>.
nexus generate dockerfile   Multi-stage Dockerfile.
nexus docs [topic]   Inline reference. --web opens the README.
nexus pki ...        Generate mTLS certs for the peer mesh.
```
`nexus build` produces ONE binary (frontend + Go). There is no deployment-split CLI.

---

## 12. Conventions & gotchas

- **`.vue` apps need `-tags vue` + `CGO_ENABLED=1`** for the SFC compiler.
- **Dashboard 404s unless `introspection = true`** (or `nexus dev`). It's locked down
  by default for production.
- **In dev the SPA is on `:5173`**, not the Go app port. `islands/` is only built by
  `nexus build`.
- **Frontend deps**: `nexus add` (not npm). The one editor `node_modules` lives under
  `islands.src/` and is gitignored — never commit it, never treat it as a build input.
- **Handler constructors are `NewXxx`**; the `New` prefix is stripped for op names.
- Don't reference `nexus.DeployAs` / `nexus.IfDeployment` — not implemented.
- `nexus docs <topic>` is the authoritative per-feature reference inside the installed
  binary; prefer it when unsure of an exact signature.
