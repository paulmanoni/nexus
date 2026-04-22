# nexus

A thin Go framework over [Gin](https://github.com/gin-gonic/gin) that registers every endpoint — REST, GraphQL, WebSocket — into a central registry, traces each request through an in-memory event bus, and exposes the lot at `/__nexus` for a Vue dashboard that renders live service topology, endpoint catalog, and request traces.

nexus does **not** replace your GraphQL layer. Hand it a `*graphql.Schema` (typically assembled with [go-graph](https://github.com/paulmanoni/go-graph)) and it mounts, introspects, and surfaces every field.

## What you get

- **Unified endpoint registry** — REST, GraphQL, and WebSocket handlers registered through one API land on a single dashboard.
- **Live architecture view** — services, resources (DBs, caches, queues), and the edges between them, drawn with [Vue Flow](https://vueflow.dev/).
- **Request traces** — bounded ring buffer with pub/sub over a WebSocket. Slow UIs drop events rather than block the request path.
- **Resource health** — register databases/caches/queues once; their status bubbles up to the dashboard and is referenced by services via `.Using("name")`.
- **Multi-instance dispatch** — the `multi` package routes N named instances of any type (e.g. multiple `*gorm.DB`) behind a single `.Using(name)` call, with optional hooks so nexus can auto-draw service→resource edges as lookups happen.
- **GraphQL introspection** — via `graphfx`, resolvers built with go-graph expose return type, per-arg validators and defaults, middleware chains, and deprecation info to the registry without extra declarations.
- **fx integration** — `fxmod` and `graphfx` slot into `go.uber.org/fx` graphs; lifecycle and shutdown are handled for you.

## Install

```bash
go get nexus
```

Requires Go 1.25+.

## Quick start

```go
package main

import (
    "net/http"

    "github.com/gin-gonic/gin"
    "nexus"
)

func main() {
    app := nexus.New(
        nexus.WithTracing(1000),
        nexus.WithDashboard(),
        nexus.WithDashboardName("Petstore"),
    )

    pets := app.Service("pets").Describe("Pet inventory")

    pets.REST("GET", "/pets").
        Describe("List all pets").
        Handler(func(c *gin.Context) {
            c.JSON(http.StatusOK, gin.H{"pets": []string{"Rex", "Whiskers"}})
        })

    _ = app.Run(":8080")
}
```

Open <http://localhost:8080/__nexus/> for the dashboard.

## Core concepts

### App

Built via `nexus.New(opts...)`. Options:

| Option | Purpose |
| --- | --- |
| `WithEngine(*gin.Engine)` | Supply a pre-configured Gin engine (otherwise a bare engine with Recovery is created). |
| `WithTracing(capacity int)` | Enable per-request trace events in a ring buffer of `capacity` events. |
| `WithDashboard()` | Mount `/__nexus/*` (endpoints, resources, events, embedded UI). |
| `WithDashboardName(string)` | Brand shown in the dashboard header and tab title. |

### Service

A named group of endpoints — one node in the architecture graph.

```go
pets := app.Service("pets").Describe("Pet inventory")

pets.REST("GET", "/pets").Describe("List").Handler(handler)
pets.WebSocket("/pets/stream").OnMessage(echoHandler).Mount()
pets.MountGraphQL("/graphql", schema)
```

### Resource

Register any dependency whose health the dashboard should surface:

```go
mainDB := resource.NewDatabase("main-db", "Primary Postgres",
    map[string]any{"engine": "postgres"},
    dbm.IsConnected,
    resource.AsDefault(),
)
app.Register(mainDB)

app.Service("pets").Using("main-db")          // explicit
app.Service("owners").Using("")               // default DB
app.Service("graph").UsingDefaults()          // default of every kind
```

Kinds: `KindDatabase`, `KindCache`, `KindQueue`, `KindOther`.

### Tracing

`WithTracing(n)` installs a pub/sub ring buffer (`trace.Bus`) that the dashboard streams from over `/__nexus/events`. Handlers record child spans with:

```go
start := time.Now()
// ... do work ...
trace.Record(c, "db.pets.list", start, nil)
```

### multi — named instances

```go
dbs := multi.New[*gorm.DB]().
    Register("main", mainDB, multi.AsDefault()).
    Register("questions", qbDB).
    Register("uaa", uaaDB)

dbs.Using("main").Find(&rows)
dbs.UsingCtx(ctx, "questions").Find(&rows)   // fires hooks; nexus draws edges
```

Install the auto-attach hook so resource edges appear as lookups fire inside a request:

```go
app.OnResourceUse(dbs)
```

## fx integration

```go
fx.New(
    fx.Supply(fxmod.Config{
        Addr:            ":8080",
        DashboardName:   "Fx Petstore",
        TraceCapacity:   1000,
        EnableDashboard: true,
    }),
    fxmod.Module,
    petsModule,
    ownersModule,
).Run()
```

See `examples/fxapp` for a multi-domain setup.

## GraphQL with graphfx + go-graph

```go
fx.New(
    fxmod.Module,
    graphfx.Module,
    fx.Provide(
        graphfx.AsQuery(NewGetAllAdverts),
        graphfx.AsMutation(NewCreateAdvert),
    ),
    graphfx.ServeAt("adverts", "/graphql",
        graphfx.Describe("Job adverts catalog"),
        graphfx.UseDefaults(),
    ),
)
```

`graphfx` introspects every mounted resolver and enriches the registry with return type, per-arg `Required`/default/validator metadata, named middleware chains, and deprecation reasons — all of which the dashboard renders automatically.

See `examples/graphapp` for a full GraphQL service wired to two named databases and a cache.

## Dashboard

Mounted at `/__nexus` when `WithDashboard()` is set:

| Route | Description |
| --- | --- |
| `GET /__nexus/` | Embedded Vue dashboard (Architecture, Endpoints, Traces tabs) |
| `GET /__nexus/config` | Dashboard config (name, etc.) |
| `GET /__nexus/endpoints` | Services + endpoints from the registry |
| `GET /__nexus/resources` | Registered resources and health |
| `GET /__nexus/middlewares` | Declared middleware names |
| `GET /__nexus/events` | WebSocket stream of trace events (with `?since=N` for backlog) |

### Developing the UI

```bash
cd dashboard/ui
npm install
npm run dev       # Vite dev server
npm run build     # writes dist/ which is embedded into Go binaries
```

## Examples

| Path | What it shows |
| --- | --- |
| `examples/petstore` | Minimal REST + WebSocket app with tracing and dashboard. |
| `examples/fxapp` | fx-driven multi-domain app with resources. |
| `examples/graphapp` | Full GraphQL service via `graphfx` + go-graph + multiple named DBs. |
| `examples/wstest` | WebSocket echo playground. |

Run any example with e.g. `go run ./examples/graphapp`.

## Package layout

```
nexus/              top-level App, Service, options
├── registry/       metadata store — services, endpoints, resources, middleware
├── resource/       database / cache / queue abstractions
├── trace/          ring-buffer event bus + middleware
├── transport/
│   ├── rest/       REST builder
│   ├── gql/        GraphQL adapter (registers fields into the registry)
│   └── ws/         WebSocket builder
├── dashboard/      /__nexus HTTP surface + embedded Vue UI
├── middleware/     shared middleware descriptors
├── multi/          N named instances behind .Using(name)
├── fxmod/          go.uber.org/fx integration for *nexus.App
├── graphfx/        go.uber.org/fx integration for go-graph schemas
├── db/             opinionated GORM helpers
└── examples/       runnable demos
```

## License

[MIT](LICENSE)