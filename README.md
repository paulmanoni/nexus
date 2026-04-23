# nexus

A Go framework over [Gin](https://github.com/gin-gonic/gin) that lets you write plain handler functions, wires them into REST + GraphQL + WebSocket transports, and ships a live Vue dashboard that renders your service topology, per-endpoint traffic, rate limits, errors, and cron jobs as they happen.

![Architecture dashboard](docs/dashboard.png)

```go
func main() {
    nexus.Run(
        nexus.Config{Addr: ":8080", EnableDashboard: true},
        nexus.ProvideResources(NewMainDB, NewCacheManager),
        advertsModule,
    )
}

var advertsModule = nexus.Module("adverts",
    nexus.Provide(NewAdvertsService),
    nexus.AsQuery(NewGetAllAdverts),
    nexus.AsMutation(NewCreateAdvert,
        nexus.Middleware("auth", "Bearer token", AuthMiddleware),
        nexus.Use(ratelimit.NewMiddleware(store, "adverts.createAdvert",
            ratelimit.Limit{RPM: 30, Burst: 5})),
    ),
)
```

No fx import. No schema assembly. No middleware plumbing. The handler is plain Go, the dashboard is at `/__nexus/`.

## Highlights

- **Reflective controllers** — write `func(svc, deps..., args) (*T, error)`; nexus's `AsRest` / `AsQuery` / `AsMutation` introspect the signature and wire the transport. No `graph.NewResolver[T](...).With...` boilerplate.
- **One middleware API for three transports** — `middleware.Middleware` bundles a `gin.HandlerFunc` + `graph.FieldMiddleware` from a single definition; `nexus.Use(mw)` attaches it to any kind of endpoint.
- **Live dashboard** — Architecture, Endpoints, Crons, Rate limits, Traces tabs. Traffic animations pulse on real requests. Error dialog shows recent errors with IP + timestamp (scales to 1000 events via virtualized scrolling).
- **Per-op observability, free** — every handler gets a request + error counter and streams a `request.op` event, all without any user code.
- **Layered rate limiting** — global (engine-root gin middleware) + per-op (graph field middleware), hot-swappable from the dashboard at runtime.
- **Cross-transport cron jobs** — schedule bare handlers; control (pause/resume/trigger-now) lives on the dashboard.
- **fx under the hood, not in your imports** — `nexus.Run/Module/Provide/Invoke` wrap fx so you get DI + lifecycle without importing `go.uber.org/fx`.

## Install

```bash
go get github.com/paulmanoni/nexus
```

Requires Go 1.25+.

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
| `nexus.Module(name, opts...)` | Named group of options. Ordered group in fx logs. |
| `nexus.Provide(fns...)` | Constructor(s) into the dep graph. |
| `nexus.Supply(vals...)` | Ready-made values into the dep graph. |
| `nexus.Invoke(fn)` | Side-effect at start-up; receives deps via function params. |
| `nexus.ProvideResources(fns...)` | Like Provide, but auto-registers resources via `NexusResourceProvider`. |
| `nexus.AsRest(method, path, fn, opts...)` | REST endpoint from a reflective handler. |
| `nexus.AsQuery(fn, opts...)` / `AsMutation(fn, opts...)` | GraphQL op, auto-mounted by the framework. |
| `nexus.Use(middleware.Middleware)` | Cross-transport middleware — works on REST + GraphQL. |

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

### Cron jobs

```go
app.Cron("refresh-cache", "*/5 * * * *").
    Describe("Refresh advert cache").
    Handler(func(ctx context.Context) error {
        return refreshCache(ctx)
    })
```

Dashboard Crons tab: schedule, last run, last result, pause/resume, trigger-now.

### Cache

`cache.Manager` is go-cache in-memory by default; switches to Redis when env is configured. Always present on `App` — nexus uses it for metrics persistence automatically.

```go
mgr := app.Cache()
_ = mgr.Set(ctx, "k", value, 5*time.Minute)
```

## Dashboard

Mounted at `/__nexus/` when `EnableDashboard: true`. Five tabs:

| Tab | What it shows |
|---|---|
| **Architecture** | Service topology with external "Clients" node, dashed system boundary, per-op edges with resource + middleware chips. Packets fly from Clients to the specific op row on live traffic (green for success, red ✕ for rejections). |
| **Endpoints** | REST path and GraphQL op-name list; per-endpoint tester (curl + Playground for GraphQL), arg validators rendered as chips. |
| **Crons** | Schedule table, pause/resume, trigger-now. |
| **Rate limits** | Declared vs effective limit per endpoint, inline edit (RPM/burst/perIP) with save/reset. |
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
| `GET  /__nexus/endpoints` | Services + endpoints |
| `GET  /__nexus/resources` | Resource snapshots (health probed live) |
| `GET  /__nexus/middlewares` | `{ middlewares: [...], global: [ordered names] }` |
| `GET  /__nexus/stats` | Per-endpoint counters (RecentErrors stripped) |
| `GET  /__nexus/stats/:service/:op/errors` | Full error ring for one endpoint |
| `GET  /__nexus/ratelimits` | Store snapshot |
| `POST /__nexus/ratelimits/:service/:op` | Override a limit |
| `DELETE /__nexus/ratelimits/:service/:op` | Reset to declared |
| `GET  /__nexus/crons`, `POST /.../:name/{trigger,pause,resume}` | Cron control |
| `GET  /__nexus/events` | WebSocket: trace + `request.op` events |

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

## Examples

| Path | Shows |
|---|---|
| `examples/petstore` | Minimal REST + WebSocket + tracing. |
| `examples/fxapp` | Multi-domain app wired via `nexus.Module` (fx hidden). |
| `examples/graphapp` | GraphQL via reflective AsQuery/AsMutation, typed DB wrappers, rate limits, validators, metrics. |
| `examples/wstest` | WebSocket echo playground. |

Run any example:
```bash
go run ./examples/graphapp
```

## Package layout

```
nexus/                top-level App, Run, Module, Provide, Use, Cron, options
├── graph/            absorbed go-graph resolver builder + validators
├── registry/         services, endpoints, resources, middleware metadata
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
├── db/               opinionated GORM helpers
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
