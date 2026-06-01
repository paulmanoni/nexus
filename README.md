<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/banner.svg">
    <source media="(prefers-color-scheme: light)" srcset="docs/banner-light.svg">
    <img src="docs/banner.svg" alt="nexus" width="100%">
  </picture>
</p>

<p align="center">
  A Go framework over <a href="https://github.com/gin-gonic/gin">Gin</a> that lets you write plain handlers,
  wires them into REST + GraphQL + WebSocket from one signature, and ships a live dashboard at <code>/__nexus/</code>.
</p>

<p align="center">
  <a href="https://github.com/paulmanoni/nexus/releases/latest"><img alt="Release" src="https://img.shields.io/github/v/release/paulmanoni/nexus?sort=semver&color=10b981&labelColor=064e3b"></a>
  <a href="https://pkg.go.dev/github.com/paulmanoni/nexus"><img alt="Go Reference" src="https://pkg.go.dev/badge/github.com/paulmanoni/nexus.svg"></a>
  <a href="go.mod"><img alt="Go version" src="https://img.shields.io/github/go-mod/go-version/paulmanoni/nexus?color=10b981&labelColor=064e3b"></a>
  <a href="https://goreportcard.com/report/github.com/paulmanoni/nexus"><img alt="Go Report Card" src="https://goreportcard.com/badge/github.com/paulmanoni/nexus"></a>
  <a href="#license"><img alt="License: MIT" src="https://img.shields.io/badge/license-MIT-10b981?labelColor=064e3b"></a>
</p>

<p align="center">
  <a href="#why-nexus">Why</a> &#183;
  <a href="#install">Install</a> &#183;
  <a href="#quick-start">Quick start</a> &#183;
  <a href="#reflective-handlers">Handlers</a> &#183;
  <a href="#dashboard">Dashboard</a> &#183;
  <a href="#peer-mesh">Peer mesh</a> &#183;
  <a href="#configuration">Config</a>
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
- **Typed peer mesh.** `peer.AsCall` exposes a handler to other apps; `peer.Call[T]` calls one. HTTP/2 + JSON over mTLS, persistent multiplexed connections, schema drift detection, trace stitching across binaries. See [Peer mesh](#peer-mesh) below.
- **Configuration server.** Spring-Cloud-Config-style distribution: `config.Server` hosts plaintext TOML (local folder or git); `config.Client` fetches signed snapshots with a sealed cache; `nexus.Get[T]("key", default)` reads typed values from anywhere. WS push for sub-second hot reload. `${VAR}` placeholders inside `nexus.toml` resolve from the process env (and optionally a `.env` file via `nexus.LoadDotenvIfPresent()`). See [Configuration](#configuration) below.
- **Guided tours for any frontend.** `extension/tour` mounts a Shadow-DOM overlay on every HTML response — record click-by-click walkthroughs with auto-screenshots (multi-scene capture for dropdowns + modals), edit step text inline on the preview, play back as numbered-badge highlights, export to **PDF** or **Word** for handoff docs. Works on React, Vue, Angular, vanilla — host CSS can't leak in. See [Tours](#tours) below.
- **Vite frontend, embedded.** `nexus new --frontend vue|react` scaffolds a standard Vite project under `web/`. `nexus dev` runs the Vite dev server (HMR) and proxies the API; `nexus build` runs `vite build` and `go build` embeds `web/dist` via `//go:embed`. Any Vite plugin, Tailwind, or component library works. Node/npm are build- and dev-time only — the runtime is a single Go binary. See [Frontend](#frontend).
- **fx under the hood, not in your imports.** `nexus.Run/Module/Provide/Invoke` wrap fx so you get DI + lifecycle without the import.

## Install

```bash
go install github.com/paulmanoni/nexus/cmd/nexus@latest
```

Go 1.25+. The CLI is pure Go — no cgo, no C toolchain, no build tags — so it
cross-compiles to a single static binary. The frontend uses Vite, so building or
running `nexus dev` on a project that has a `web/` frontend also needs **Node.js +
npm** on PATH (build- and dev-time only; the compiled app embeds `web/dist` and needs
neither at runtime).

## CLI

```bash
nexus new my-app && cd my-app
nexus dev                 # go run + live dashboard (add --open to launch a browser)
```

| Command | What it does |
|---|---|
| `nexus new <dir>` | Scaffold a runnable app (prompts for frontend / db / cache / auth). |
| `nexus init [dir] --frontend=vue\|react` | Add a Vite frontend (`web/`, embed) to an existing project. |
| `nexus dev [dir]` | `go run` + the Vite dev server (HMR on :5173); serves the dashboard and proxies `/__nexus`, `/graphql`, `/oauth`, `/ws` to the app. |
| `nexus build [package]` | `npm install` (if needed) + `vite build` → `web/dist`, then `go build` embeds it via `//go:embed`. |
| `nexus docs [topic]` | Inline reference (`handlers`, `frontend`, `auth`, …). |
| `nexus client [--out dir]` | Write the embedded JS/TS client SDK to disk. |
| `nexus generate <kind>` | Code-generation helpers (e.g. `dockerfile`). |

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

`nexus dev` runs it and serves the dashboard at `http://localhost:8080/__nexus` (add --open to launch a browser), exposing the handler over GraphQL (`{ sayHello(name:"world") }`) and REST (`POST /hello/sayHello`).

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

The frontend is a standard **Vite** project under `web/` — npm-managed, so any Vite plugin, Tailwind, PostCSS, or component library works. `nexus build` runs `vite build` and `go build` embeds the output into the binary; the runtime needs no Node.

```bash
nexus new my-app --frontend vue   # scaffold web/ (or --frontend react)
cd my-app/web && npm install       # install frontend deps (one-time / on clone)
cd .. && nexus dev                 # go run + Vite dev server (HMR) on :5173
nexus build                        # vite build → web/dist, embedded via //go:embed
```

`main.go` embeds the build output and serves it:

```go
//go:embed all:web/dist
var webFS embed.FS

nexus.Run(nexus.Config{...},
    nexus.ServeFrontend(webFS, "web/dist"),
    helloModule,
)
```

Unknown paths fall through to `index.html` (SPA-aware); `/assets/*` gets immutable cache; REST/GraphQL/WS/dashboard routes win on conflict. Boot fails fast if `index.html` is missing, so the scaffold commits a `web/dist/index.html` stub — the first `go build` compiles before any `vite build`. The frontend dir is `web/` by default (override with `NEXUS_FRONTEND_DIR`); to mount an existing pre-built SPA, just point `ServeFrontend` at its embedded dist.

### `nexus dev` — Vite dev server + API proxy

`nexus dev` runs `npm run dev` (the Vite dev server, with HMR) on `http://localhost:5173/` alongside `go run` on `:8080`. It injects a managed proxy block into `web/vite.config.ts` (between `// @nexus:proxy-start` / `// @nexus:proxy-end` markers) so `/__nexus`, `/graphql`, `/oauth`, and `/ws` reach the Go app from the Vite origin. Open **:5173** for the SPA during dev; the embedded `web/dist` is served at the app port only in production. Override the dev-server command with `--frontend-cmd`.

### Dev-reload watcher

Under `nexus dev` (`NEXUS_DEV=1`), the browser auto-reloads on source changes. The watcher ignores hidden files, sourcemaps, and **runtime data artifacts** out of the box — SQLite databases (`.db` / `.sqlite` / `.sqlite3`), their `-wal` / `-shm` / `-journal` sidecars, and `.log` files — so a request that writes the database can't trigger a reload loop.

Add app-specific ignore globs via `Config.DevReload.Exclude` (or `[runtime.devreload]` in `nexus.toml`). Each changed file is matched against its base name, its path relative to the watch root, and as a directory-subtree prefix:

```toml
[runtime.devreload]
exclude = ["uploads", "*.tmp", "cache/*.json"]
```

Invalid patterns are logged once at boot and skipped.

### Custom banner

`nexus dev` prints a NEXUS wordmark on startup. Drop a `banner.txt` in the directory you run `nexus dev` from to replace it with your own — the file's contents are rendered verbatim above the `target` / `starting` rows, so a project can ship its own ASCII wordmark or message:

```
$ cat banner.txt
  ~ acme admin ~
  staging
```

An empty or missing `banner.txt` falls back to the built-in NEXUS banner.

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

## Peer mesh

Typed RPC between nexus apps over HTTP/2 + JSON. One persistent multiplexed connection per peer pair, mTLS by default, trace stitching across binaries, schema-drift detection — explicit, type-safe, no codegen.

```go
// callee
peer.Module(peer.Config{Identity: "orders-svc", Listen: ":7000", ...})
// + nexus.AsCall("createOrder", NewCreateOrder)

// caller
peer.Module(peer.Config{Identity: "checkout-svc", Peers: map[string]peer.PeerSpec{...}})
order, err := peer.Call[*Order](ctx, peers, "orders-svc", "createOrder", args)
```

Full reference: [**extension/peer/README.md**](extension/peer/README.md) covers the safety nets (schema drift, trace stitching, health prober, concurrency cap), SRV discovery for replicas, three auth modes (mTLS / HMAC / none), and the `nexus pki` PKI toolchain.

## Configuration

Two on-disk config surfaces, distinct purposes:

| Surface | Auto-loaded | Purpose |
|---|---|---|
| **`nexus.toml`** | yes (`./nexus.toml`) | Deploy manifest — topology, declared inputs, plugin blocks. What the platform sees + provisions. |
| **`extension/config`** | opt-in | Runtime key/value store; `nexus.Get[T]("key", default)` from any handler. |

`nexus.toml` supports `${VAR}` / `${VAR:default}` placeholders (strict mode) and an opt-in `.env` loader via `nexus.LoadDotenvIfPresent()` for dev workflows.

Full reference: [**extension/config/README.md**](extension/config/README.md) covers both surfaces — `config.Server` / `config.Client` / `config.Local`, the sealed-cache + signed-snapshot security model, env-var expansion rules, and the dotenv contract.

## Tours

Guided walkthroughs for any HTTP-served frontend. Record click-by-click sequences with auto-screenshots, edit inline, replay as numbered-badge highlights, export to PDF / Word for handoff docs. Closed Shadow DOM mount — works on React, Vue, Angular, vanilla without CSS bleed in either direction.

```go
nexus.Run(nexus.Config{...},
    tour.Module(tour.WithGORM(db.DB()), tour.AutoInject(true)),
)
```

After restart, every HTML page carries a floating **● Tour** pill; authoring dashboard is at `/__nexus/tour`.

Full reference: [**extension/tour/README.md**](extension/tour/README.md) covers recording semantics, multi-scene capture for dropdowns/modals, the preview-page editor, Word/PDF export with TOC, and the keyboard shortcuts.

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
├── extension/             optional integrations (auth, oauth2, ratelimit,
│                          metrics, cron, dashboard, frontend, peer,
│                          config, tls, …)
├── graph/                 GraphQL builder + resolver introspection
├── transport/             gin/REST + WS adapters
├── manifest/              app self-description JSON (served at /__nexus/manifest)
├── registry/              endpoint + service + resource graph
└── examples/              self-contained sample apps
```

## License

MIT.
