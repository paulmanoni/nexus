<p align="center">
  <img src="docs/logo.png" alt="nexus — control plane for Go applications" width="320">
</p>

# nexus

A Go framework over [Gin](https://github.com/gin-gonic/gin) that lets you write plain handler functions, wires them into REST + GraphQL + WebSocket transports, and ships a live Vue dashboard that renders your service topology, per-endpoint traffic, rate limits, errors, and cron jobs as they happen.

![Architecture dashboard](docs/dashboard.png)

```go
func main() {
    nexus.Run(
        nexus.Config{Addr: ":8080", EnableDashboard: true},
        nexus.ProvideResources(NewMainDB, NewCacheManager),
        auth.Module(auth.Config{Resolve: resolveBearer}),
        advertsModule,
        nexus.AsWorker("cache-invalidation", NewCacheInvalidationWorker),
    )
}

var advertsModule = nexus.Module("adverts",
    nexus.ProvideService(NewAdvertsService),
    nexus.AsQuery(NewGetAllAdverts),
    nexus.AsMutation(NewCreateAdvert,
        auth.Required(),
        auth.Requires("ROLE_CREATE_ADVERT"),
        nexus.Use(ratelimit.NewMiddleware(store, "adverts.createAdvert",
            ratelimit.Limit{RPM: 30, Burst: 5})),
    ),
)
```

No fx import. No schema assembly. No middleware plumbing. The handler is plain Go, the dashboard is at `/__nexus/`.

## Highlights

- **Reflective controllers** — write `func(svc, deps..., args) (*T, error)`; nexus's `AsRest` / `AsQuery` / `AsMutation` introspect the signature and wire the transport. No `graph.NewResolver[T](...).With...` boilerplate.
- **Module-first architecture view** — `nexus.Module("name", opts...)` groups endpoints as one card on the dashboard; services appear as typed *dependency nodes* that both handlers and service constructors can point at. `nexus.ProvideService` inspects the constructor's params and draws service → resource / service → service edges automatically.
- **Built-in auth** — `auth.Module` ships a pluggable authentication surface (bearer / cookie / api-key extraction, cached identity resolution, per-op `auth.Required` / `auth.Requires(perms)` bundles, admin invalidation + live `auth.reject` trace events on the dashboard's Auth tab).
- **Background workers** — `nexus.AsWorker` wraps long-lived listeners (DB LISTEN/NOTIFY, queue consumers, sweepers) with framework-owned lifecycle, ctx cancellation, panic recovery, and a card on the architecture view that shows their dep graph.
- **One middleware API for three transports** — `middleware.Middleware` bundles a `gin.HandlerFunc` + `graph.FieldMiddleware` from a single definition; `nexus.Use(mw)` attaches it to any kind of endpoint.
- **Live dashboard** — Architecture, Endpoints, Crons, Rate limits, Auth, Traces tabs. Traffic animations pulse on real requests — inbound lanes, per-op resource edges, and service-level edges all light up in sync. Error dialog shows recent errors with IP + timestamp (scales to 1000 events via virtualized scrolling).
- **Per-op observability, free** — every handler gets a request + error counter and streams a `request.op` event, all without any user code.
- **Layered rate limiting** — global (engine-root gin middleware) + per-op (graph field middleware), hot-swappable from the dashboard at runtime.
- **Cross-transport cron jobs** — schedule bare handlers; control (pause/resume/trigger-now) lives on the dashboard.
- **fx under the hood, not in your imports** — `nexus.Run/Module/Provide/Invoke` wrap fx so you get DI + lifecycle without importing `go.uber.org/fx`.

## Install

```bash
go get github.com/paulmanoni/nexus
go install github.com/paulmanoni/nexus/cmd/nexus@latest   # optional CLI
```

Requires Go 1.25+.

## CLI

The `nexus` binary is a thin convenience wrapper — everything it does is
reachable through plain `go` commands, but having one entry-point for the
common-case loop keeps muscle memory short.

```bash
nexus new my-app          # scaffold main.go + module.go + go.mod + nexus.deploy.yaml
cd my-app
go mod tidy
nexus dev                 # go run . + auto-open http://localhost:8080/__nexus/
```

| Command | What it does |
|---|---|
| `nexus new <dir>` | Scaffolds a minimal app (reflective `AsRest` + dashboard + a heavily-commented `nexus.deploy.yaml`). `--module <path>` overrides the go.mod path. |
| `nexus init [dir]` | Adds `nexus.deploy.yaml` to an existing project. Scans for `nexus.DeployAs(...)` tags and pre-populates a deployments + peers block. `--force` overwrites; refuses by default. |
| `nexus dev [dir]` | Runs `go run <dir>` (default `.`), probes `:8080`, opens the dashboard as soon as it responds. With `--split` boots one subprocess per deployment unit (per-unit binary built via the overlay path), streams trace events with cross-service spans, prints a per-unit ready dot. `--addr host:port`, `--no-open`, `--base-port`. |
| `nexus build` | Builds one deployment binary using `go build -overlay`. `--deployment <name>` (required) names the unit from `nexus.deploy.yaml`. The framework generates per-deployment shadow source (HTTP-stub `Service` for non-owned modules) and a deploy-init file that bakes the manifest's port + peer table into the binary — main.go contains zero deployment code. |
| `nexus version` | Prints the CLI version. |

## Quick start

```go
package main

import (
    "context"

    "github.com/paulmanoni/nexus"
)

// Service wrapper — distinct Go type per logical service so fx can
// route by type (no named tags).
type AdvertsService struct{ *nexus.Service }

func NewAdvertsService(app *nexus.App) *AdvertsService {
    return &AdvertsService{app.Service("adverts").Describe("Job adverts catalog")}
}

// Typed DB handle — same pattern. Fx resolves by type, compile-time
// routing, no string lookups.
type MainDB struct{ *DB }

func NewMainDB() *MainDB { /* Open, migrate, return wrapper */ }

// Every dep the handler declares shows up on the dashboard:
//   - *AdvertsService  →  grounds the op under the "adverts" service
//   - *MainDB          →  draws an edge from adverts → main resource
//   - nexus.Params[T]  →  resolve context + typed args bundle
func NewListAdverts(svc *AdvertsService, db *MainDB, p nexus.Params[struct{}]) (*Response, error) {
    return fetch(p.Context, db)
}

func main() {
    nexus.Run(
        nexus.Config{
            Addr:            ":8080",
            DashboardName:   "Adverts",
            TraceCapacity:   1000,
            EnableDashboard: true,
        },
        nexus.ProvideResources(NewMainDB),
        nexus.Module("adverts",
            nexus.Provide(NewAdvertsService),
            nexus.AsQuery(NewListAdverts),
        ),
    )
}
```

Open <http://localhost:8080/__nexus/>. Fire a request → packet animation on the Architecture tab.

## Core concepts

### App and Config

`nexus.Run(cfg, opts...)` builds and runs the app. Block until SIGINT/SIGTERM, then gracefully shuts down. `Config` covers environment-level knobs; options are the building blocks of your graph.

```go
nexus.Run(nexus.Config{
    Addr:            ":8080",
    DashboardName:   "Adverts",
    TraceCapacity:   1000,
    EnableDashboard: true,

    // GraphQL toggles — one switch, all services
    DisablePlayground: false,
    GraphQLDebug:      false,

    // App-wide rate limit (applies to every HTTP path)
    GlobalRateLimit: ratelimit.Limit{RPM: 600, Burst: 50},
})
```

Option builders:

| Option | Produces |
|---|---|
| `nexus.Module(name, opts...)` | Named group of options. Stamps module name onto every endpoint for the architecture view. |
| `nexus.Provide(fns...)` | Constructor(s) into the dep graph. |
| `nexus.ProvideService(fn)` | Provide + introspect: detects resource / service deps from the constructor's params and records them for the Architecture view. |
| `nexus.ProvideResources(fns...)` | Like Provide, but auto-registers resources via `NexusResourceProvider`. |
| `nexus.Supply(vals...)` | Ready-made values into the dep graph. |
| `nexus.Invoke(fn)` | Side-effect at start-up; receives deps via function params. |
| `nexus.AsRest(method, path, fn, opts...)` | REST endpoint from a reflective handler. |
| `nexus.AsQuery(fn, opts...)` / `AsMutation(fn, opts...)` | GraphQL op, auto-mounted by the framework. |
| `nexus.AsWS(path, type, fn, opts...)` | WebSocket endpoint scoped to one envelope message type; multiple AsWS on the same path share one hub. |
| `nexus.AsWorker(name, fn)` | Long-lived background task; framework manages lifecycle + records status. |
| `nexus.Use(middleware.Middleware)` | Cross-transport middleware — works on REST + GraphQL. |
| `auth.Module(auth.Config{Resolve: ...})` | Built-in auth surface: extraction + cached resolution + per-op enforcement bundles. |

### Reflective handlers

Write handlers as plain Go functions. Signature convention:

```go
func NewOp(svc *XService, deps..., p nexus.Params[ArgsStruct]) (*Response, error)
```

- **First `*Service`-wrapper dep** grounds the op under that service. Auto-routing picks the single-service default when omitted.
- **`context.Context`** anywhere in the deps list is special-cased (filled from `p.Context`).
- **Last param** (if it's a struct, or `nexus.Params[T]`) carries user-supplied args.
- **Return** must be `(T, error)` — T becomes the GraphQL return type, flow-through for REST.

```go
type CreateArgs struct {
    Title        string `graphql:"title,required" validate:"required,len=3|120"`
    EmployerName string `graphql:"employerName,required" validate:"required,len=2|200"`
}

func NewCreateAdvert(svc *AdvertsService, db *MainDB,
                     p nexus.Params[CreateArgs]) (*AdvertResponse, error) {
    return create(p.Context, db, p.Args.Title, p.Args.EmployerName)
}
```

Tags drive the schema + validators:
- `graphql:"name,required"` — field name + NonNull marker
- `validate:"required,len=3|120"` — `graph.Required()` + `graph.StringLength(3, 120)`, introspected by the dashboard as chips

### Service + typed resource wrappers

Resources (DBs, caches, queues) are typed wrappers that own their dashboard metadata. No resourcesModule, no string matching.

```go
type MainDB struct{ *DB }

func (m *MainDB) NexusResources() []resource.Resource {
    return []resource.Resource{
        resource.NewDatabase("main", "GORM — sqlite",
            map[string]any{"engine": "sqlite", "schema": "main"},
            m.IsConnected, resource.AsDefault()),
    }
}
```

Any handler that takes `*MainDB` as a dep auto-draws the `service → main` edge on the Architecture graph — no `Using(...)` call required.

### Cross-transport middleware

```go
// Build once — bundle carries Gin + Graph realizations.
authMw := middleware.Middleware{
    Name:        "auth",
    Description: "Bearer token validation",
    Kind:        middleware.KindBuiltin,
    Gin:         authGinHandler,
    Graph:       authResolverMiddleware,
}

// Apply anywhere.
nexus.AsRest("POST", "/secure", NewSecureHandler, nexus.Use(authMw))
nexus.AsMutation(NewMutate,                    nexus.Use(authMw))

// Global — every HTTP path (REST + GraphQL + WS upgrade + dashboard)
nexus.Config{
    GlobalMiddleware: []middleware.Middleware{requestID, logger, cors},
}
```

Built-ins that ship:
- `ratelimit.NewMiddleware(store, key, limit)` — token-bucket with per-IP option
- `metrics` — auto-attached to every op, no user code

### Rate limits

Layered by default:

| Layer | How | Where |
|---|---|---|
| **Global** | `Config.GlobalRateLimit` | gin middleware on engine root |
| **Per-op** | `nexus.Use(ratelimit.NewMiddleware(...))` | per-handler |
| **Runtime override** | Rate limits tab in dashboard | hot-swappable without redeploy |

Store swap for multi-replica:
```go
nexus.Config{
    RateLimitStore: ratelimit.NewRedisStore(redisClient), // not yet shipped
    // or just
    Cache: cache.NewManager(cfg, logger), // app auto-uses this for the store
}
```

### Metrics + error dialog

Every op automatically gets:
- Request counter (atomic, ~70 ns)
- Error counter
- Ring of recent error events (IP, timestamp, message) capped at 1000
- `request.op` trace event emitted on every handler exit

The Architecture tab shows `⚡N` (request count) and `⚠N` (errors) chips per op. Click the error chip → paginated dialog with filter over IP/message + virtualized scrolling that stays snappy at thousands of events.

### Auth (built-in)

`auth.Module` owns the plumbing — token extraction, identity caching, per-op enforcement, context propagation — and leaves *resolution* (token → Identity) as the single plug you wire:

```go
import "github.com/paulmanoni/nexus/auth"

nexus.Run(nexus.Config{...},
    auth.Module(auth.Config{
        // Required: turn a raw token into an Identity.
        Resolve: func(ctx context.Context, tok string) (*auth.Identity, error) {
            u, err := myAPI.ValidateToken(ctx, tok)
            if err != nil { return nil, err }
            return &auth.Identity{
                ID:    u.ID,
                Roles: u.Roles,
                Extra: u,   // user-defined payload, typed-accessible later
            }, nil
        },
        Cache: auth.CacheFor(15 * time.Minute),

        // Optional: match your existing error envelope
        OnUnauthenticated: func(c *gin.Context, err error) {
            c.AbortWithStatusJSON(401, pkg.Response[any]{Success: false, Message: "UnAuthorized"})
        },
    }),
    advertsModule,
)
```

Per-op gating (cross-transport):

```go
nexus.AsMutation(NewCreateAdvert,
    auth.Required(),                         // 401 if no identity
    auth.Requires("ROLE_CREATE_ADVERT"),     // 403 if missing permission
)
```

Resolver access:

```go
func NewListAdverts(db *MainDB, p nexus.Params[struct{}]) (*Response, error) {
    user, ok := auth.User[MyUser](p.Context)
    if !ok {
        // Required() would have caught this earlier, but a direct
        // check is idiomatic for handlers using auth.Optional().
    }
    return fetch(p.Context, db, user.ID)
}
```

Token extraction strategies ship ready-made — `auth.Bearer()`, `auth.Cookie(name)`, `auth.APIKey(header)`, `auth.Chain(...)` — plus the typed `auth.User[T]` generic accessor, `auth.AnyOf`/`auth.AllOf` permission helpers, and a `*auth.Manager` handle (fx-injected) for logout flows:

```go
func NewLogoutHandler(am *auth.Manager) func(context.Context, nexus.Params[TokenArgs]) (OK, error) {
    return func(ctx context.Context, p nexus.Params[TokenArgs]) (OK, error) {
        am.Invalidate(p.Args.Token)         // single-session logout
        // or am.InvalidateByIdentity(userID) → sweeps every cached session for that user
        return OK{}, nil
    }
}
```

Dashboard's Auth tab renders the cached identity table, recent 401/403 rejections (live via `auth.reject` trace events), and per-row "invalidate" buttons — all driven off `GET /__nexus/auth` + `POST /__nexus/auth/invalidate`.

### Workers

`nexus.AsWorker` wraps long-lived background tasks (DB `LISTEN`/`NOTIFY` loops, queue consumers, sweepers) with framework-owned lifecycle:

```go
nexus.AsWorker("cache-invalidation",
    func(ctx context.Context, db *OatsDB, cache *CacheManager, logger *zap.Logger) error {
        // Wait for dependencies to come up
        for !db.IsConnected() {
            select {
            case <-ctx.Done(): return ctx.Err()
            case <-time.After(time.Second):
            }
        }

        listener := pq.NewListener(db.ConnectionString(), 10*time.Second, time.Minute, nil)
        defer listener.Close()
        if err := listener.Listen("cache_invalidation"); err != nil { return err }

        for {
            select {
            case <-ctx.Done():
                return nil                  // clean stop on fx.Stop
            case n := <-listener.Notify:
                handleInvalidation(ctx, cache, n)
            }
        }
    })
```

The framework starts the function on its own goroutine at `fx.Start`, cancels `ctx` at `fx.Stop`, recovers panics, and records `Status` / `LastError` on the registry. The worker appears as a dedicated card on the Architecture view with its dep graph (resources + services it took as params) drawn as outgoing edges — same visual language as services.

Signature requirements:
- First param MUST be `context.Context`.
- Remaining params are fx-injected deps (resources, services, loggers, whatever's in the graph).
- Optional `error` return sets `LastError` on the registry; `context.Canceled` / `nil` is a clean stop.

### Cron jobs

```go
app.Cron("refresh-cache", "*/5 * * * *").
    Describe("Refresh advert cache").
    Handler(func(ctx context.Context) error {
        return refreshCache(ctx)
    })
```

Dashboard Crons tab: schedule, last run, last result, pause/resume, trigger-now.

### WebSocket (AsWS)

`AsWS(path, messageType, fn)` registers a reflective WebSocket handler scoped
to one inbound envelope type. Multiple `AsWS` calls for the same path share
one connection pool — the framework dispatches by the envelope's `type` field.

```go
type ChatPayload struct {
    Text string `json:"text"`
}

func NewChatSend(svc *ChatService, sess *nexus.WSSession,
                 p nexus.Params[ChatPayload]) error {
    sess.EmitToRoom("chat.message",
        map[string]string{"text": p.Args.Text, "user": sess.UserID()},
        "lobby")
    return nil
}

var chat = nexus.Module("chat",
    nexus.Provide(NewChatService),
    nexus.AsWS("/events", "chat.send",   NewChatSend,   auth.Required()),
    nexus.AsWS("/events", "chat.typing", NewChatTyping),
)
```

**Wire protocol** — every message is wrapped in the envelope:

```json
{ "type": "chat.send", "data": { "text": "hello" }, "timestamp": 1700000000 }
```

The built-in types `ping`, `authenticate`, `subscribe`, `unsubscribe` are
handled by the framework hub and never reach user handlers. Custom types
dispatch to the matching `AsWS` registration; unknown types are dropped
silently.

**Session API** (`*nexus.WSSession`) mirrors the fan-out semantics of the
oats_applicant hub pattern:

| Call | Scope |
|---|---|
| `sess.Send(type, data)` | Unicast back to the originating connection. |
| `sess.Emit(type, data)` | Broadcast to every connection on this endpoint. |
| `sess.EmitToUser(type, data, userID...)` | Every connection authed as one of the listed users. |
| `sess.EmitToRoom(type, data, room)` | Every connection subscribed to the room. |
| `sess.EmitToClient(type, data, clientID...)` | Specific ClientIDs. |
| `sess.JoinRoom(room)` / `sess.LeaveRoom(room)` | Server-side room membership (client can also use the built-in `subscribe`/`unsubscribe` protocol messages). |
| `sess.ClientID()` / `sess.UserID()` / `sess.Metadata()` / `sess.Context()` | Connection-scoped accessors. |
| `sess.SendRaw(bytes)` | Escape hatch for non-envelope payloads. |

**Identity** at upgrade time: the framework picks `?userId=` from the URL
or an auth-middleware-set `user` value in `gin.Context` (anything satisfying
`interface{ GetID() string }`). Handlers read it via `sess.UserID()`;
`auth.Required()` and friends work as middleware on the upgrade route.

**Middleware rule** — `nexus.Use(mw)`, `auth.Required()`, etc. on the *first*
`AsWS` call for a path install on the HTTP upgrade route. Middleware on
later calls for the same path is ignored with a warning log (every dispatch
shares one upgrade route).

Handler errors come back as an `error` envelope on the same connection and
keep the socket open:

```json
{ "type": "error", "data": { "type": "chat.send", "message": "too long" }, "timestamp": ... }
```

For the full hub API (custom upgrader, typed event envelopes, connection
worker-pool sizing), the imperative builder is still available:
`(*Service).WebSocket(path).WithHub(hub).Mount()`.

### Cache

`cache.Manager` is go-cache in-memory by default; switches to Redis when env is configured. Always present on `App` — nexus uses it for metrics persistence automatically.

```go
mgr := app.Cache()
_ = mgr.Set(ctx, "k", value, 5*time.Minute)
```

### Deployment-ready modules

Write the application as a monolith; ship as N independent services with no source changes. The framework swaps cross-module `*Service` struct bodies between the local impl and an HTTP-stub shadow at compile time, driven by a single declarative file:

```yaml
# nexus.deploy.yaml
deployments:
  monolith:                       # owns every module by default
    port: 8080
  users-svc:
    owns: [users]
    port: 8081
  checkout-svc:
    owns: [checkout]
    port: 8080

peers:
  users-svc:
    timeout: 2s
    auth:
      type: bearer
      token: ${USERS_SVC_TOKEN}
  checkout-svc:
    timeout: 2s
```

#### Module declaration — unchanged across deployments

```go
var Module = nexus.Module("users",
    nexus.DeployAs("users-svc"),
    nexus.Provide(NewService),
    nexus.AsRest("GET", "/users/:id", NewGet),
    nexus.AsRest("GET", "/users",     NewList),
    nexus.AsQuery(NewSearch),
)
```

`DeployAs("users-svc")` names the deployment unit; the manifest decides whether this module's source is compiled locally or replaced with an HTTP stub for a given binary.

#### Consumer — same Go in every binary

```go
type Service struct {
    *nexus.Service
    users *users.Service          // local in monolith / users-svc, HTTP stub in checkout-svc
}

func NewService(app *nexus.App, u *users.Service) *Service {
    return &Service{Service: app.Service("checkout"), users: u}
}

func NewSubmit(svc *Service, p nexus.Params[SubmitArgs]) (*Receipt, error) {
    u, err := svc.users.Get(p.Context, users.GetArgs{ID: p.Args.UserID})
    if err != nil { return nil, fmt.Errorf("lookup user: %w", err) }
    return &Receipt{...}, nil
}
```

Cross-module calls read like normal struct-method calls. No `Client` interface, no per-deployment branching, no env-var lookups in user code. The framework auto-Provides `*users.Service` via an `init()`-time registry so consumers don't need a manual `Provide` line either.

#### Build per deployment

```bash
nexus build --deployment monolith       # ./bin/monolith   — every module local
nexus build --deployment users-svc      # ./bin/users-svc  — checkout shadowed
nexus build --deployment checkout-svc   # ./bin/checkout-svc — users shadowed
```

Each command:
1. Reads `nexus.deploy.yaml`, scans modules for `DeployAs` tags, computes which to shadow.
2. For every non-owned module, parses each `.go` file in the package, preserves exported types verbatim, and synthesizes a `zz_shadow_gen.go` containing an HTTP-stub `Service` whose plain methods route through `nexus.PeerCaller`. Multi-file modules (`types.go + service.go + handlers.go + module.go`) are handled the same way as single-file ones.
3. Generates a `zz_deploy_gen.go` whose `init()` calls `nexus.SetDeploymentDefaults(...)` with the manifest's port + peer table baked in (URL, timeout, auth closure for bearer tokens, retries, min-version skew floor).
4. Writes everything under `.nexus/build/<deployment>/` (gitignored), then runs `go build -overlay=overlay.json` so the compiler picks up shadow + deploy-init files without touching the source tree.

#### Run all units locally with one command

```bash
$ nexus dev --split

  nexus dev (split mode)
  ──────────
  checkout-svc  port 8080  →  http://localhost:8080
  users-svc     port 8081  →  http://localhost:8081
  ● starting · ctrl-c to stop all

  building checkout-svc
  building users-svc
checkout-svc nexus: listening on [::]:8080
  ● checkout-svc ready · http://localhost:8080
users-svc    nexus: listening on [::]:8081
  ● users-svc    ready · http://localhost:8081

  ● all ready · 2 units · ctrl-c to stop

checkout-svc → POST /checkout                              [9c9090]
checkout-svc   ↳ remote GET /users/:id ok 5ms → http://localhost:8081  [9c9090]
users-svc    → GET /users/u1                               [9c9090]
users-svc    ← GET /users/u1 200 1ms                       [9c9090]
checkout-svc ← POST /checkout 200 7ms                      [9c9090]
```

Each unit is built via the overlay path (real production binary, not `go run`), gets a per-unit ready dot when its port responds, and the trace bus on each subprocess streams into the dev terminal — request lines plus colored cross-service spans threaded by 6-char trace ID so you can read concurrent requests at a glance. `Ctrl-C` kills every subprocess cleanly.

#### Friendly framework errors

Every framework error is a `*nexus.UserError` with `op` / `hint` / `notes` / `cause` fields, formatted multi-line with a hint section:

```
nexus error [remote call]: GET /users/:id: peer responded but the JSON didn't fit the client's return type
  url:  http://localhost:8081/users/
  body: [{"id":"u1","Name":"Alice"},{"id":"u2","Name":"Bob"}]
  cause: json: cannot unmarshal array into Go value of type users.User
  hint: verify the peer's handler return type matches the client's expected shape; if the URL above looks wrong, check the args struct for empty path params
```

Status-specific hints fire for 401/403/404/405/408/429/5xx. 3xx redirects from the peer surface as fail-fast errors (the remote client deliberately doesn't auto-follow so wrong-endpoint cases land loud, not as a confusing decode error). Path-expansion catches empty parameters before any HTTP request. Boot-time topology validation runs before fx spins up. All error fields propagate to the dashboard waterfall as span attrs (`error.op`, `error.hint`, etc.) so the UI can render them as separate elements.

#### Two ways to start

| Starting from | Command |
|---|---|
| Empty directory | `nexus new myapp` — full scaffold with `nexus.deploy.yaml` already in place |
| Existing nexus codebase | `nexus init` — drops a manifest, pre-populates from discovered `DeployAs` tags |

The manifest is heavily commented as a learning artifact: anyone reading it can see exactly how to add a new deployment, wire a peer, configure auth, etc.

#### Design notes

- **Path 3 architecture.** The transport switch lives at compile time, not runtime. There's no `if dep == "users-svc"` branch in user code; the type's body is genuinely different per binary, enforced by Go's compiler. A method that exists on the local impl but not on the remote stub fails the split build immediately — no silent runtime degradation.
- **`-overlay` not source rewriting.** Original source on disk never changes. Per-deployment shadows live in `.nexus/build/<deployment>/` and feed `go build -overlay=overlay.json`. Same Go toolchain, same debugger story, no custom build pipeline to maintain.
- **No more `nexus gen clients`.** The previous codegen path that produced `UsersClient` interfaces and `usersClientLocal/Remote` impls is gone. The new flow has fewer concepts (just `*Service`) and removes the regenerate-after-each-handler-change friction.
- **Scope vs Service Weaver.** Same goal — write monolith, deploy as N — without Weaver's `Implements[T]` mixin or rewrite tax. Untagged modules stay exactly as they are today; the deployment story is opt-in per module via `DeployAs`.

## Dashboard

Mounted at `/__nexus/` when `EnableDashboard: true`. Six tabs:

| Tab | What it shows |
|---|---|
| **Architecture** | Module containers + endpoints, service-dep nodes, worker cards, resource nodes, external "Clients" node, dashed system boundary. Per-op and service-level edges both pulse on live traffic (green for success, red ✕ for rejections). |
| **Endpoints** | REST path and GraphQL op-name list; per-endpoint tester (curl + Playground for GraphQL), arg validators rendered as chips. |
| **Crons** | Schedule table, pause/resume, trigger-now. |
| **Rate limits** | Declared vs effective limit per endpoint, inline edit (RPM/burst/perIP) with save/reset. |
| **Auth** | Cached identities (redacted tokens), live 401/403 stream, per-identity invalidation. Renders a "not configured" prompt when `auth.Module` isn't wired. |
| **Traces** | WebSocket stream of request events, filterable. |

Tab selection persists via `?tab=` in the URL — shareable, bookmarkable, survives refresh.

![Traces tab](docs/traces.png)

### Gating the dashboard

Opt the whole `/__nexus/*` surface (JSON APIs, WebSocket events, embedded UI) into your own auth / permission chain by passing middleware bundles:

```go
nexus.Run(
    nexus.Config{
        EnableDashboard: true,
        DashboardMiddleware: []middleware.Middleware{
            {Name: "auth",  Kind: middleware.KindBuiltin, Gin: bearerAuthGin},
            {Name: "admin", Kind: middleware.KindCustom,  Gin: requireAdminGin},
        },
    },
    // ...
)
```

Middleware runs on the `/__nexus` route group before any dashboard handler, so one chain covers every dashboard request. Bundles with a nil `Gin` realization are ignored (the dashboard is HTTP-only). `nexus.WithDashboardMiddleware(...)` is the equivalent `AppOption` for callers using `nexus.New`.

HTTP surface:

| Route | Returns |
|---|---|
| `GET  /__nexus/` | Embedded Vue UI |
| `GET  /__nexus/endpoints` | Services + endpoints (services carry `ResourceDeps` / `ServiceDeps` from `ProvideService`) |
| `GET  /__nexus/resources` | Resource snapshots (health probed live) |
| `GET  /__nexus/workers` | `AsWorker` registrations + live Status / LastError / deps |
| `GET  /__nexus/middlewares` | `{ middlewares: [...], global: [ordered names] }` |
| `GET  /__nexus/stats` | Per-endpoint counters (RecentErrors stripped) |
| `GET  /__nexus/stats/:service/:op/errors` | Full error ring for one endpoint |
| `GET  /__nexus/ratelimits` | Store snapshot |
| `POST /__nexus/ratelimits/:service/:op` | Override a limit |
| `DELETE /__nexus/ratelimits/:service/:op` | Reset to declared |
| `GET  /__nexus/crons`, `POST /.../:name/{trigger,pause,resume}` | Cron control |
| `GET  /__nexus/auth` | `{ identities, cachingEnabled }` — cached auth state |
| `POST /__nexus/auth/invalidate` | Body `{id?|token?}` → drops cache entries (`{dropped: N}`) |
| `GET  /__nexus/events` | WebSocket: trace + `request.op` + `auth.reject` events |

### Developing the UI

```bash
cd dashboard/ui
npm install
npm run dev       # Vite dev server
npm run build     # dist/ gets embedded into Go binaries via //go:embed
```

## Benchmarks

Microbenchmarks of the per-request hot paths on an Apple M1 Pro:

| Path | ns/op | allocs |
|---|---:|---:|
| `metrics.Record` (success, single key) | 73 | 0 |
| `metrics.Record` (parallel, 10 cores) | 238 | 0 |
| `ratelimit.Allow` (single key) | 134 | 1 |
| `callHandler` (reflective invoke) | 477 | 5 |
| `bindGqlArgs` (map → struct) | 250 | 4 |
| direct function call (baseline) | 0.3 | 0 |

A request going through `AsQuery` with args, metrics, and one rate limit therefore pays on the order of 73 + 134 + 477 + 250 ≈ **1 μs** of nexus-side work. The surrounding cost (Gin routing, graphql-go query parsing, JSON encoding, your handler, any DB/cache roundtrip) is measured by your own load test, not by this README.

Run the microbenchmarks:
```bash
go test ./metrics ./ratelimit ./ -bench=. -benchmem -run 'x^'
```

For end-to-end numbers on your own workload, load-test a real endpoint:
```bash
vegeta attack -rate=10000 -duration=30s -targets=targets.txt | vegeta report
```

### Monolith vs split — what does deployment shape cost?

Benchmark of the `examples/microsplit` `/checkout` endpoint, which exercises
the full cross-module call path (checkout calls users). 32 concurrent
clients, 20k requests per run, on a 10-core M1 Pro. Same source, two
different binaries via `nexus build --deployment`.

| | Monolith | Split | Ratio |
|---|---:|---:|---:|
| Throughput | 56,618 req/s | 16,380 req/s | 3.5× |
| p50 latency | 450 µs | 1.66 ms | 3.7× |
| p95 latency | 1.29 ms | 3.65 ms | 2.8× |
| p99 latency | 1.75 ms | 7.25 ms | 4.2× |

The monolith routes the cross-module call in-process through `LocalInvoker`
(an `httptest.Recorder` against the same gin engine — auth, rate-limit,
metrics, and traces all fire identically). Split mode replaces that with
a real HTTP roundtrip via `PeerCaller`, plus traceparent propagation and
two extra JSON encode/decodes.

**The 3.5× factor is mostly TCP loopback, not framework overhead.** Per
call, monolith ≈ 17 µs of nexus-side work; split ≈ 60 µs total, of which
~43 µs is the kernel + HTTP path and ~17 µs is the framework. The extra
~43 µs is the actual cost of going to the network.

#### When the handler does real work

The trivial `users.Get` returns from an in-memory map. With a 5ms
`time.Sleep` standing in for a real DB/cache/external call, the
deployment-mode delta vanishes:

| | Monolith (5ms handler) | Split (5ms handler) |
|---|---:|---:|
| Throughput | 5,286 req/s | 5,376 req/s |
| p50 latency | 5.93 ms | 5.51 ms |
| p95 latency | 6.97 ms | 7.19 ms |
| p99 latency | 8.55 ms | 14.7 ms |

**Identical at p50/p95.** When the handler dominates total latency, the
split's framework + HTTP overhead becomes a rounding error. p99 still
widens because of HTTP connection-pool contention under heavy concurrency
— tunable via `Peer.Timeout` and the underlying `http.Client`.

#### Practical rule

| Handler does | Split overhead is |
|---|---|
| < 1ms (in-memory) | 3-4× hit on throughput — cross-service hop dominates |
| 1-5ms (cache, fast DB) | 1.5-2× hit — noticeable but acceptable |
| ≥ 5ms (real DB, external API) | invisible — handler absorbs it |

Most real handlers do ≥ 10ms of work (a single Postgres query is
3-15ms; an external HTTP call is 50-300ms). So the split-vs-monolith
choice is almost never about per-request performance — it's about
operational concerns (independent scaling, separate on-call, team
boundaries, blast radius). Ship as monolith on day one without
paying a future tax: when you split later, the per-request cost
increase is a rounding error against your actual handler work.

#### Reproduce

```bash
cd examples/microsplit
nexus build --deployment monolith
nexus build --deployment users-svc
nexus build --deployment checkout-svc

# Monolith
PORT=9000 ./bin/monolith &
hey -n 20000 -c 32 -m POST -T application/json \
    -d '{"userId":"u1","orderId":"o7"}' \
    http://localhost:9000/checkout

# Split
NEXUS_DEPLOYMENT=users-svc    PORT=9001 ./bin/users-svc &
NEXUS_DEPLOYMENT=checkout-svc PORT=9000 \
    USERS_SVC_URL=http://localhost:9001 ./bin/checkout-svc &
hey -n 20000 -c 32 -m POST -T application/json \
    -d '{"userId":"u1","orderId":"o7"}' \
    http://localhost:9000/checkout
```

## Examples

| Path | Shows |
|---|---|
| `examples/petstore` | Minimal REST + WebSocket + tracing. |
| `examples/fxapp` | Multi-domain app wired via `nexus.Module` (fx hidden). |
| `examples/graphapp` | GraphQL via reflective AsQuery/AsMutation, typed DB wrappers, rate limits, validators, metrics. |
| `examples/wstest` | WebSocket echo playground (imperative `(*Service).WebSocket(...)` path). |
| `examples/wsecho` | Typed WebSocket via `AsWS` — two message types on one path, envelope protocol, session fan-out. |
| `examples/microsplit` | Two-module demo (`users` + `checkout`) showing manifest-driven deployments. `users` is split across `types.go + service.go + handlers.go + module.go` to exercise the multi-file shadow path. `nexus build --deployment X` produces three different binaries from the same source. |

Run any example:
```bash
go run ./examples/graphapp
```

## Package layout

```
nexus/                top-level App, Run, Module, Provide, ProvideService, AsWorker, Use, Cron, options
├── auth/             built-in authentication surface (extractors, identity cache, per-op bundles, dashboard routes)
├── graph/            absorbed go-graph resolver builder + validators
├── registry/         services, endpoints, resources, workers, middleware metadata
├── resource/         Database/Cache/Queue abstractions + health probing
├── trace/            ring-buffer bus + per-request middleware + op events
├── transport/
│   ├── rest/         REST builder
│   ├── gql/          GraphQL HTTP adapter (Playground, auth hook)
│   └── ws/           WebSocket builder + Hub
├── middleware/       Info + cross-transport Middleware bundle
├── metrics/          per-endpoint counters, error ring, cache-backed store
├── ratelimit/        token-bucket store, Gin + Graph middleware factories
├── cron/             scheduler, dashboard HTTP, event emission
├── cache/            Redis + in-memory hybrid (nexus uses it as a default)
├── multi/            N named instances behind .Using(name) (legacy pattern)
├── db/               opinionated GORM helpers (Manager.DB(ctx), ConnectionString, GetCtx)
├── dashboard/        /__nexus HTTP surface + embedded Vue UI
└── examples/         runnable demos
```

## Testing

```bash
go build ./...
go vet ./...
go test ./...
go test ./... -bench=. -benchmem -run 'x^'
```

## License

[MIT](LICENSE)
