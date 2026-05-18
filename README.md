<p align="center">
  <img src="docs/logo.svg" alt="nexus" width="120" height="120">
</p>

<h1 align="center">nexus</h1>

<p align="center">
  A Go framework over <a href="https://github.com/gin-gonic/gin">Gin</a> that lets you write plain handlers,
  wires them into REST + GraphQL + WebSocket from one signature, and ships a live dashboard at <code>/__nexus/</code>.
</p>

```go
func main() {
    nexus.Run(
        nexus.Config{
            Server:    nexus.ServerConfig{Addr: ":8080"},
            Dashboard: nexus.DashboardConfig{Enabled: true, Name: "Adverts"},
        },
        nexus.ProvideResources(NewMainDB),
        adverts.Module,
    )
}

var Module = nexus.Module("adverts",
    nexus.Provide(NewService),
    nexus.AsQuery(NewListAdverts),
    nexus.AsMutation(NewCreateAdvert,
        auth.Required(),
        auth.Requires("ROLE_CREATE_ADVERT"),
    ),
)
```

![Architecture dashboard](docs/dashboard.png)

---

## Why nexus

- **One handler, three transports.** `AsRest` / `AsQuery` / `AsMutation` / `AsWS` all read the same reflective signature — `func(svc, deps..., p nexus.Params[Args]) (T, error)` — and wire the right transport.
- **Live architecture view.** `nexus.Module` groups endpoints; constructor introspection draws service → service / service → resource edges automatically. Real traffic pulses on the edges.
- **Built-in auth, rate limits, metrics, traces.** Cross-transport bundles via `nexus.Use`. Per-op observability is free — every handler gets counters + traces with no user code.
- **Manifest-driven deployment.** Write the app as a monolith, declare a few split units in `nexus.deploy.yaml`, ship N independent binaries from the same source. `go build -overlay` swaps cross-module `*Service` bodies for HTTP stubs at compile time.
- **Node-free frontend.** `nexus add vue` pulls from esm.sh into `~/.nexus/cache`; `nexus build` bundles via esbuild. No `node_modules`, no `npm install`. See [frontend/deps](frontend/deps/README.md).
- **fx under the hood, not in your imports.** `nexus.Run/Module/Provide/Invoke` wrap fx so you get DI + lifecycle without the import.

## Install

```bash
go install github.com/paulmanoni/nexus/cmd/nexus@latest
```

Go 1.25+. For `.vue` source compile support, opt into the QuickJS-backed Vue SFC compiler:

```bash
CGO_ENABLED=1 go install -tags vue github.com/paulmanoni/nexus/cmd/nexus@latest
```

## CLI

```bash
nexus new my-app && cd my-app
nexus dev                 # go run + auto-open the dashboard
```

| Command | What it does |
|---|---|
| `nexus new <dir>` | Scaffold a runnable app (prompts for frontend / db / cache / auth). |
| `nexus init [dir]` | Add `nexus.deploy.yaml` to an existing project. |
| `nexus dev [dir]` | `go run` + auto-rebuild frontend on save; opens the dashboard. `--split` boots one subprocess per deployment unit. |
| `nexus build [--deployment <name>]` | Build a binary. Bundles frontend sources under `islands.src/` first, then `go build` (with overlay shadows when `--deployment` is set). |
| `nexus docs [topic]` | Inline reference (`handlers`, `deploy`, `frontend`, `auth`, …). |
| `nexus add <spec>` | Fetch a frontend dep from esm.sh into `~/.nexus/cache`, write `nexus.lock`. |
| `nexus install` | Sync the cache to `nexus.lock` (fresh clones, CI). |
| `nexus remove <spec>` | Drop entry from `nexus.lock`. |
| `nexus update [spec]` | Re-resolve specs, bump `nexus.lock`. |
| `nexus vendor` | Copy cached blobs to `./vendor/nexus/` for air-gapped builds. |
| `nexus gc` | Reclaim cache space from unreferenced blobs. |

## Quick start

```go
// main.go
package main

import "github.com/paulmanoni/nexus"

func main() {
    nexus.Run(
        nexus.Config{
            Server:    nexus.ServerConfig{Addr: ":8080"},
            Dashboard: nexus.DashboardConfig{Enabled: true, Name: "Demo"},
        },
        helloModule,
    )
}

// hello.go
type HelloService struct{}

func NewHelloService() *HelloService { return &HelloService{} }

type SayHelloArgs struct{ Name string }

func (s *HelloService) SayHello(_ context.Context, p nexus.Params[SayHelloArgs]) (string, error) {
    return "Hello, " + p.Input.Name, nil
}

var helloModule = nexus.Module("hello",
    nexus.Provide(NewHelloService),
    nexus.AsQuery((*HelloService).SayHello),
)
```

`nexus dev` runs it, opens the dashboard at `http://localhost:8080/__nexus`, exposes the handler over GraphQL (`{ sayHello(name:"world") }`) and REST (`POST /hello/sayHello`).

## Reflective handlers

One signature, three transports:

```go
// REST: POST /widgets
nexus.AsRest("POST", "/widgets", NewCreateWidget)

// GraphQL: mutation createWidget(input: WidgetInput!): Widget!
nexus.AsMutation(NewCreateWidget)

// GraphQL query: query listWidgets(filter: WidgetFilter): [Widget!]!
nexus.AsQuery(NewListWidgets)

// WebSocket event: emit { type:"chat.send", payload:{...} }
nexus.AsWS("/events", "chat.send", NewChatSend)
```

Each constructor returns a function with the canonical shape:

```go
func NewCreateWidget(svc *WidgetService) func(context.Context, nexus.Params[CreateArgs]) (Widget, error)
```

The framework introspects: `*WidgetService` becomes an fx dependency; `Params[CreateArgs]` declares the input schema; the return type becomes the GraphQL field type. Args + return get auto-mapped to GraphQL input + object types — no schema duplication.

## CRUD generator

```go
type Advert struct {
    ID    uuid.UUID `nexus:"key"`
    Title string
    Body  string
}

var Module = nexus.Module("adverts",
    nexus.ProvideCRUD[Advert]("adverts"),
)
```

Mounts:

```
GET    /adverts          → list
GET    /adverts/:id      → get
POST   /adverts          → create
PUT    /adverts/:id      → update
DELETE /adverts/:id      → delete
```

Plus the corresponding GraphQL queries + mutations. Stores ship for `postgres`, `mysql`, `sqlite`, in-memory; pick via `ProvideStore[Advert](store.Postgres{...})`.

## Cross-transport middleware

```go
nexus.AsMutation(NewCreateAdvert,
    auth.Required(),
    auth.Requires("ROLE_CREATE_ADVERT"),
    ratelimit.PerUser(100, time.Minute),
)
```

Works identically on REST, GraphQL, and WS. Each middleware reads from `nexus.Context` rather than `*gin.Context` so it's transport-agnostic.

## Frontend

Two paths to a bundle the framework can serve:

**1. `nexus add` + `nexus build` (recommended).** Node-free; pulls deps from esm.sh, bundles via esbuild. See [frontend/deps](frontend/deps/README.md).

```bash
nexus add vue @vue-flow/core
nexus build                       # islands.src/*.{ts,tsx,jsx,vue} → islands/
```

**2. Bring your own bundle.** Mount any pre-built SPA from an `embed.FS`:

```go
//go:embed all:web/dist
var webFS embed.FS

nexus.Run(nexus.Config{...},
    nexus.ServeFrontend(webFS, "web/dist"),
    helloModule,
)
```

Unknown paths fall through to `index.html` (SPA-aware). REST/GraphQL/WS/dashboard routes win on conflict.

## Deployment

Write one app, ship N binaries:

```yaml
# nexus.deploy.yaml
deployments:
  api-svc:
    owns: [adverts, users, auth]
  worker-svc:
    owns: [billing.workers, notifications.workers]
```

```bash
nexus build --deployment api-svc      → ./bin/api-svc
nexus build --deployment worker-svc   → ./bin/worker-svc
```

Both binaries share the source tree. `go build -overlay` substitutes HTTP-stub `*Service` bodies for modules a binary doesn't own — your code reads `users *users.Service` and the framework wires it to the right transport per deployment.

## Dashboard

Mounted at `/__nexus/` when `Dashboard.Enabled` is true. Shows:

- **Architecture**: services, resources, endpoints, dependency edges. Live updates as registry mutates.
- **Traces**: request waterfalls across services; cross-transport stitched.
- **Crons**: scheduled jobs + last/next-run.
- **Rate limits**: per-key bucket state.

![Trace waterfall](docs/traces.png)

## Examples

The framework ships a few self-contained examples under `examples/`:

| | |
|---|---|
| `petstore` | REST + GraphQL CRUD; the canonical "small app" |
| `petstore-spa` | Same with a frontend bundle served from `embed.FS` |
| `pubsub` | Redis + RabbitMQ adapters |
| `wsecho`, `wstest` | WebSocket patterns |
| `graphapp` | GraphQL-first app with cross-resolver auth |
| `microsplit` | Manifest-driven monolith → multi-service split |
| `fxapp` | Direct fx wiring without the nexus.Run wrapper |

## Layout

```
.                          framework root + public API surface
├── cmd/nexus/             CLI binary
├── frontend/deps/         node-free dep manager + bundler (see its README)
├── extension/             optional integrations (auth, oauth2, ratelimit,
│                          metrics, cron, dashboard, frontend, …)
├── graph/                 GraphQL builder + resolver introspection
├── transport/             gin/REST + WS adapters
├── manifest/              nexus.deploy.yaml parser + overlay generator
├── registry/              endpoint + service + resource graph
└── examples/              self-contained sample apps
```

## License

MIT.
