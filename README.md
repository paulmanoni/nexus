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
| `nexus init [dir] --frontend=vue\|react` | Scaffold the frontend pipeline (`islands.src/`, embed) into an existing project. |
| `nexus dev [dir]` | `go run` + auto-rebuild frontend on save; opens the dashboard. |
| `nexus build [package]` | Bundle frontend sources under `islands.src/`, then `go build` the main package. |
| `nexus docs [topic]` | Inline reference (`handlers`, `frontend`, `auth`, …). |
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

## Dashboard

Mounted at `/__nexus/` when `Dashboard.Enabled` is true. Shows:

- **Architecture**: services, resources, endpoints, dependency edges. Live updates as registry mutates.
- **Traces**: request waterfalls across services; cross-transport stitched.
- **Crons**: scheduled jobs + last/next-run.
- **Rate limits**: per-key bucket state.

![Trace waterfall](docs/traces.png)

## Listeners, scopes, and TLS

In production you usually want the dashboard surface (`/__nexus/*`) reachable from operators but **not** from the public internet. Nexus does this with named listeners + scopes — one process binds multiple ports, each gated to a subset of routes.

### Scopes

```go
nexus.Config{
    Server: nexus.ServerConfig{
        Listeners: map[string]nexus.Listener{
            "public": {Addr: ":8080",          Scope: nexus.ScopePublic},
            "admin":  {Addr: "127.0.0.1:9090", Scope: nexus.ScopeAdmin},
        },
    },
}
```

- **`ScopePublic`** — user routes (REST / GraphQL / WebSocket) + `/__nexus/health` + `/__nexus/ready`. Dashboard hidden.
- **`ScopeInternal`** — same as public; intended for sidecar / peer traffic.
- **`ScopeAdmin`** — everything: dashboard + user routes (so the in-page testers' relative fetches still work).

Routes outside the listener's scope return `404` — byte-equivalent to "not mounted", so scanners can't fingerprint the dashboard.

### Introspection gate

Belt-and-braces on top of scopes — defaults to **closed** so a misconfigured listener doesn't leak the dashboard:

```go
nexus.Config{
    Introspection:         false,                  // closed in prod (default)
    IntrospectionNetworks: []string{"10.0.0.0/8"}, // VPN bypass
}
```

When closed, every `/__nexus/*` request 404s unless the caller's TCP peer matches an allowlisted CIDR. The gate uses `RemoteIP()`, not `ClientIP()` — `X-Forwarded-For` is spoofable, wrong default for a security gate. Behind an LB, prefer the admin-listener pattern over the CIDR allowlist.

GraphQL field introspection (the `__schema` query) is gated by the same `Introspection` flag — closing the dashboard also closes in-band GraphQL introspection.

### TLS on a listener

Set `Listener.TLS` to terminate HTTPS on that port — the raw TCP listener gets wrapped at bind time:

```go
adminTLS, err := nexus.ServerTLSConfig(
    "admin.crt",     // PEM cert
    "admin.key",     // PEM private key
    "admin-ca.crt",  // optional CA bundle; pass "" to skip mTLS
)
if err != nil { log.Fatal(err) }

nexus.Config{
    Server: nexus.ServerConfig{
        Listeners: map[string]nexus.Listener{
            "public": {Addr: ":8080",         Scope: nexus.ScopePublic},
            "admin":  {Addr: "10.0.0.5:9443", Scope: nexus.ScopeAdmin, TLS: adminTLS},
        },
    },
}
```

When the optional CA file is supplied, the listener requires clients to present a certificate signed by that CA — mTLS, enforced at the TLS layer before any HTTP request is dispatched. Pass `""` for server-only TLS.

`ServerTLSConfig` sets `MinVersion = TLS 1.2` by default. For custom cipher suites or SNI via `GetCertificate`, build a `*tls.Config` directly and assign it to the `TLS` field — anything `crypto/tls` supports works.

> For **public-internet HTTPS with Let's Encrypt auto-issuance**, prefer `extension/tls.Plugin` — it owns its own `:443`/`:80` pair and handles ACME challenges. `Listener.TLS` is the right tool for admin/internal ports fronted by your own cert material (internal CA, mTLS-protected dashboard, etc.).

### Typical production recipe

```go
adminTLS, _ := nexus.ServerTLSConfig("admin.crt", "admin.key", "admin-ca.crt")

nexus.Run(
    nexus.Config{
        Introspection:         false,
        IntrospectionNetworks: []string{"10.0.0.0/8"},
        Server: nexus.ServerConfig{
            Listeners: map[string]nexus.Listener{
                "public": {Addr: ":8080",         Scope: nexus.ScopePublic},
                "admin":  {Addr: "10.0.0.5:9443", Scope: nexus.ScopeAdmin, TLS: adminTLS},
            },
        },
    },
    tls.Plugin(tls.Config{ // public :443 + :80, LE-issued cert for the user-facing port
        Domains: []string{"app.example.com"},
        Email:   "ops@example.com",
    }),
    // ... app modules
)
```

Result: world-facing `:443` serves user traffic over LE-issued HTTPS. Dashboard is reachable only on the private `10.0.0.5:9443` with an mTLS client cert signed by `admin-ca.crt`. `/__nexus/*` is `404` on every other surface even if the network somehow leaks.

## Examples

The framework ships a few self-contained examples under `examples/`:

| | |
|---|---|
| `petstore` | REST + GraphQL CRUD; the canonical "small app" |
| `petstore-spa` | Same with a frontend bundle served from `embed.FS` |
| `pubsub` | Redis + RabbitMQ adapters |
| `wsecho`, `wstest` | WebSocket patterns |
| `graphapp` | GraphQL-first app with cross-resolver auth |
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
├── manifest/              app self-description JSON (served at /__nexus/manifest)
├── registry/              endpoint + service + resource graph
└── examples/              self-contained sample apps
```

## License

MIT.
