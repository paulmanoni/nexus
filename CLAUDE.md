# nexus — framework guide for Claude Code

nexus is a Go backend framework: typed reflective handlers over REST + GraphQL +
WebSocket, dependency injection (a built-in zero-dep container; fx optional), a **Vite**
frontend embedded in the binary, and a live introspection dashboard at `/__nexus`. This
file tells you how to use every feature. Verify APIs against the installed version; `nexus
docs <topic>` prints an inline quick-reference for any feature (`nexus docs --list`).

Import path: `github.com/paulmanoni/nexus`. Pure-Go build — no CGO, no build tags.
A frontend is an ordinary npm-managed Vite project: **Node.js 20+ and npm are dev- and
build-time requirements**; the runtime is still a single Go binary with `web/dist`
embedded, and runs without Node (an Inertia SSR server is the one opt-in exception).

---

## 1. Frontend (Vite, embedded SPA / Inertia)

nexus drives the project's **own Vite** (`web/node_modules/.bin/vite`) and embeds its
output: `nexus build` runs `vite build` into `web/dist`, and `go build` embeds it via
`//go:embed`. Any framework, Vite plugin or npm library works. **viteless** (the former
zero-Node engine) is retired: a `web/` with `viteless.config.*`/`viteless-env.d.ts` and no
`package.json` gets a migration hint (a warning in `nexus dev`, an error in `nexus build`);
`nexus init --frontend vue --force` adds the Vite files and keeps the sources.

### Layout
```
web/
  package.json          # vite ^6.4.3, @vitejs/plugin-vue ^5.2.4 (react: plugin-react ^5.2 + React 19),
                        # typescript ~6.0.3, vue-tsc ^3.3.11 (vue-tsc 3.3 crashes on TypeScript 7)
  package-lock.json     # commit it — nexus dev/build run `npm ci` when it exists
  vite.config.ts        # plugins: [vue(), nexus()] — nexus from './sdk/nexus-vite-plugin.js'; NO proxy
  tsconfig.json         # strict, include ["src"], "@/*"→src, types vite/client, no baseUrl
  index.html            # entry HTML (/src/main.ts) — also the Inertia page shell
  sdk/                  # COMMIT: nexus-vite-plugin.{js,d.ts} + the typed SDK nexus dev writes
  src/
    main.ts             # entry (main.tsx for React)
    App.vue             # (App.tsx; Inertia: Pages/*.vue; --ssr adds ssr.ts)
  dist/                 # build OUTPUT, embedded in the binary
    index.html          # a committed stub ships so the first `go build` compiles
```
`node_modules/` and `dist/*` (except the stub) are gitignored; `web/sdk` must not be — a
fresh checkout's `vite.config.ts` imports the plugin from it. **Which dir** (`nexus dev`):
`--frontend` (cwd-relative) > `NEXUS_FRONTEND_DIR` (project-relative) > the dir main.go's
`ServeFrontend`/`frontend.Plugin` call names (AST scan, resolved against the package dir)
> `web/` when it has a `package.json`; `nexus build` takes `NEXUS_FRONTEND_DIR`, else
`web`. **No `package.json` → no Vite**: a hand-written or prebuilt `dist` is served as-is
(e.g. `examples/petstore-spa`).

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
`index.html`, and REST/GraphQL/WS routes win on conflict. Mount under a sub-path with
`nexus.FrontendAt("/admin")`. **Caching comes from the build:** a file is `immutable`
only when the Vite manifest lists it as output *and* its name carries a content hash;
everything else (and the shell) is revalidated with an ETag, so repeat requests are
304s. In production, boot fails fast when the bundle has neither `index.html` nor a
Vite manifest — a manifest alone boots a module-only (Inertia) build, whose unknown
routes then 404; in development an unbuilt bundle serves a placeholder instead.

**The Vite handshake (`nexus-vite-plugin` + `internal/vitehot`).** With `nexus()` in
`vite.config`, `vite dev` writes `<outDir>/.vite/nexus-hot.json` (the dev server's
bound origin, base, entries, pid) and removes it on exit. `ServeFrontend` and the
Inertia engine read it per request — never served, followed only under `nexus dev` or
`environment = "development"` and only while its dev server is live (running pid, or an
origin that answers; a file left by a killed Vite reads as absent) — so pages on the Go
origin load modules from wherever Vite really bound, and `public/` files are proxied to
Vite for loopback clients. **The browser always opens the app's origin**; Vite serves
modules only, so there is no proxy block and no second URL, and `npm run dev` + `go run
.` is a complete dev setup. `vite build` gets `build.manifest: true` forced (the app
reads `dist/.vite/manifest.json`); `nexus({ input: 'src/main.ts' })` declares a
module-only entry. **Inertia pages render into `index.html`** (`App.FrontendDocument`):
the engine sets `data-page` on the mount element and keeps the rest of the document, so
title/meta/stylesheets live in `index.html`, not in `inertia.Config.Head`; only a
module-only build (`input`, no `index.html`) gets a synthesised document. `nexus({ pages
})` (default `src/Pages`) warns in dev and fails `vite build` for a registered page with
no component. Under `nexus dev` the reload shim reloads when a new server process is
serving, never for files Vite hot-updates. `NEXUS_VITE_DEV` survives only as a fallback
origin when no hot file is readable (`nexus dev` no longer sets it). Design:
`docs/design/frontend-seam.md`.

**`[env]` → the bundle.** `nexus dev`/`nexus build` pass nexus.toml's `[env]` table to
Vite as `NEXUS_FRONTEND_ENV` (JSON of dotted keys); the plugin defines
`import.meta.env.<dotted.key>` in dev and build. A Vite you start yourself gets none.

### Build / serve commands
```
nexus dev                # build-then-swap Go loop + the project's own Vite — see below
nexus build              # npm ci (if needed) → vite build [→ SSR build] → web/dist, then go build
```
`nexus build` treats `<dir>/package.json` as "there is a frontend" (none → pure-Go
build). Steps: install deps only when `node_modules/.bin/vite` is missing (`npm ci` with a
lockfile, else `npm install`; a clear error without npm on PATH) → write
`web/sdk/nexus-vite-plugin.{js,d.ts}` → `vite build` → if `src/ssr.ts` exists, `vite build
--ssr src/ssr.ts --outDir dist/ssr` (without emptying `dist`) → require
`dist/.vite/manifest.json` or `dist/index.html` → `go build`.

### `nexus dev` — the dev model (IMPORTANT)
`nexus dev` supervises the project's **own Vite** beside the Go app:
- First run installs dependencies (`npm ci`/`npm install`) when `node_modules/.bin/vite`
  is missing, writes `web/sdk/nexus-vite-plugin.*`, then spawns `vite` in the frontend dir
  (its own process group, no forced `--host`) with `NEXUS_FRONTEND_ENV`.
- The dev origin comes from the **hot file**, never from Vite's stdout. **Open the URL
  `nexus dev` prints — the Go app's origin** (`http://localhost:8080` or your `addr`); the
  dashboard is on the same origin. Vite's own `Local:`/`Network:` banner is hidden (it
  would send you to the wrong port); its other output is prefixed `[web]` (`--verbose`
  shows everything). `--tui` runs Vite too.
- On exit Vite gets SIGTERM, then SIGKILL after 2s, and `nexus dev` waits, so the plugin
  removes its hot file. There is no Inertia "mode" any more (no `go list -deps` scan, no
  `[runtime.inertia] enabled`): SPA and Inertia apps share one dev topology.
  `--frontend-cmd` is deprecated and ignored.
In production the embedded `web/dist` is served at the app port via `ServeFrontend`.

**Go restarts are build-then-swap.** On a save the next binary compiles while the
current one keeps serving; only a green build takes the old process down, so the app
is unavailable for the swap (~20ms) rather than the whole compile — the outage no
longer grows with build time. Three consequences worth knowing:
- **A broken save doesn't take the app down.** The compile error prints and the last
  good build keeps serving until the code compiles again.
- **A save that doesn't change the binary skips the restart** (`● binary unchanged`).
  Go's build output is content-addressed, so `_test.go` edits, comment-only changes,
  and edits in packages the app doesn't import cost nothing and preserve app state.
- The freshly built binary is **pre-executed once** (aborted inside the Go runtime,
  before any package init or `main`) so the OS pays its first-exec cost — ~450ms of
  code-signature validation on macOS — while the old process is still answering.

`--go-run` restores the legacy loop (`go run`, app killed before every rebuild) if
the new one ever misbehaves.

**The link is the rebuild.** Compilation is cached per package; the one step no
cache makes incremental is the link, and it dominates (~all of a warm rebuild).
Two defaults follow from that, both dev-only:
- **DWARF is stripped** (`-ldflags=-w`, the old opt-in `--fast`). Pass `--debug`
  to keep it when you need delve or a complete panic trace.
- **The frontend bundle is stubbed out of the dev binary.** Under `NEXUS_DEV`
  `ServeFrontend` reads `web/dist` from disk (and pages load their modules from Vite
  anyway), so the embedded copy is dead weight that gets relinked on every save. `nexus
  dev` maps it to empty files via the same `go build -overlay` it already uses
  for handler codegen — no build tags, no source changes. Scoped strictly to the
  tree a `ServeFrontend` call names, so assets your app genuinely reads at
  runtime (fonts, templates, seed data) are untouched. `--no-embed-stub` opts out.

Measured on a ~114MB app: 4.27s → 2.99s per rebuild. Note this is latency-until-
live, not downtime — build-then-swap keeps the app serving throughout.

**What triggers a rebuild.** Go sources, `go.mod`/`go.sum`, `nexus.toml`, and any file
under an `//go:embed` root. Not: `_test.go` files (never compiled into the binary),
`testdata/`, hidden files, or nested modules with their own `go.mod` — unless the root
module `replace`s into one, which makes it a real build input.

**`.nexusignore`.** Drop one next to `nexus.toml` to keep project-specific paths out of
the dev loop entirely (generated trees, fixtures, a sibling service's source). Read at
startup, gitignore-style, and honored by both the Go watcher and `--dist`:
```
# comments and blank lines are ignored
tmp/                    # trailing slash: directories only
generated               # no slash: matches at any depth
internal/mock/*.go      # a slash anchors the pattern to the project root
assets/**/snapshots     # ** spans any number of directories
!internal/mock/keep.go  # ! re-includes; later rules win
```
An ignored directory is pruned, so nothing inside it is watched (a `!` re-include under
a pruned directory can't resurrect it — same rule git applies).

**Keeping in-memory state across a rebuild (`nexus.PreserveDev`).** The rebuild replaces
the process, so maps die with the old binary. Hand the state to the dev loop instead:
```go
func NewStore() *Store {
    s := &Store{notes: map[int]Note{}}
    nexus.PreserveDev("notes", s)          // no-op outside nexus dev
    return s
}
func (s *Store) SnapshotDev() ([]byte, error) { return json.Marshal(s.notes) }
func (s *Store) RestoreDev(b []byte) error    { return json.Unmarshal(b, &s.notes) }
```
Restore happens inside `PreserveDev`, so lazy DI construction is fine; the snapshot is
written on the graceful shutdown `nexus dev` triggers before the swap. Without methods to
write, use `nexus.PreserveDevJSON(name, get, set)`. `auth.MemoryUserStore` implements
`DevState` already (register it as `nexus.PreserveDev("auth.users", store)`); users the
new process seeds itself win over the snapshot. Dev-only (gated on the state file
`nexus dev` passes), per-session (state survives rebuilds, not Ctrl-C), graceful exits
only, and best-effort — a failed snapshot/restore is reported and skipped, never fatal.
Caches are deliberately not preserved. `nexus docs devstate`.

For state that already has its own on-disk format (an embedded key/value store,
a SQLite handle), `nexus.DevStateDir()` returns that session directory — point
the store at a real path instead of `:memory:` and it survives the rebuild;
outside `nexus dev` it returns `""`, so the production path is untouched.
`extension/oauth2` does this for its default token store, so an OAuth2 login
survives a rebuild instead of forcing a re-login on every save (access tokens are
opaque values held in the store, not self-contained JWTs, so persisting the store
is sufficient). Setting `Config.TokenStore` opts out.

**Keeping `web/dist` fresh in dev (`--dist`).** The dev server serves the frontend
from memory and never writes `web/dist`, so the embedded production bundle stays
frozen at the last `nexus build` — a `go build` taken mid-session ships stale assets.
`nexus dev --dist` re-runs `vite build` (plus the SSR build when `src/ssr.ts` exists)
into `web/dist` (debounced) alongside the dev server, so the embed always matches the
live frontend; the plugin puts the live dev
server's hot file back after `emptyOutDir`. Opt-in: each change is a full production
build. No rebuild loop — `dist/` is excluded from the watch and the Go-source watcher
ignores `web/dist` writes.

### Dev server logs (columnar, configurable — Django/Spring-style)
`nexus dev` reshapes the app's structured (zap-JSON) log lines into a columnar,
colorized **Dev Server Logs** view — `time · LEVEL · source(file:line) · message
key=value` (info=cyan, warn=amber, error=red). Non-JSON output (gin, the
`nexus: listening on …` banner, panics) passes through untouched, and the
prettifier auto-disables when stdout isn't a tty (so `nexus dev > log` keeps raw
JSON for grep/jq); color honors `NO_COLOR`.

The formatter is pluggable, configured declaratively like Django's `LOGGING`
formatters or Spring's `logging.pattern.console`:
```toml
[runtime.logging]
format   = "pretty"   # pretty (default) | logfmt | pattern | raw/json (passthrough)
pattern  = "%time  %-5level  %caller  %msg  %fields"   # used when format = "pattern"
requests = true       # dev-only per-request console log (see below); false to silence
```
Pattern tokens (Spring/logback-flavored): `%time`/`%d`, `%level`/`%p` (`%-5level`
pads+uppercases), `%caller`/`%logger`, `%msg`/`%m`, `%fields`/`%X`, `%%`. Override
per-run with `--log-format` / `--log-pattern`; `--raw-logs` forces passthrough. Add
a new named format by writing one `logFormatter` and registering it in
`resolveLogFormatter` (`cmd/nexus/dev_logpretty.go`).

**Per-request console logs (dev only).** By default request activity goes to the
dashboard trace stream (`/__nexus`), not the console — so navigating pages leaves the
terminal quiet. Under `nexus dev` (`NEXUS_DEV=1`) nexus also logs one line per HTTP
request to stdout (`GET /users  status=200  dur=12ms`), rendered through the view above;
5xx→error, 4xx→warn. Dashboard traffic (`/__nexus*`) is skipped. Production binaries
never log requests to the console (gated on `NEXUS_DEV`); silence it in dev with
`[runtime.logging] requests = false`.

### Scaffold a frontend
```
nexus new myapp --frontend vue      # or react — a Vite project under web/ (package.json, vite.config.ts, sdk/)
nexus new myapp --inertia [--ssr]   # Inertia (Vue) pages + pages.go; --ssr adds src/ssr.ts + the SSR build
nexus init --frontend vue           # add web/ to an EXISTING project (patches main.go); --force keeps sources
```
After scaffolding: `go mod tidy && nexus dev` — it installs the deps on first run and
prints the app's URL. `--tooling` is deprecated (ignored). Scaffolds write
`environment = "development"` to nexus.toml; **deployments set
`NEXUS_ENVIRONMENT=production`**, which overrides it. SSR: `ssr.ts` uses
`@inertiajs/vue3/server`; `nexus build` writes `web/dist/ssr/ssr.js` with its deps bundled
(`ssr.noExternal`), so `node web/dist/ssr/ssr.js` (:13714) runs beside the binary without
`node_modules` (it is embedded with `all:web/dist`, but `ServeFrontend` never serves
`dist/ssr`); main.go passes `inertia.Config{SSR: ssrhttp.New("")}`; under `nexus dev`
pages render client-side.

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
environment    = "development"          # development | staging | production; NEXUS_ENVIRONMENT overrides
introspection  = true                   # opens /__nexus (OFF by default → 404s)
introspection_networks = ["10.0.0.0/8"] # allowed even when introspection is off
trace_capacity = 1000                    # request-trace ring buffer (0 = off)
sdk            = true                    # one switch: generate+serve the typed client SDK

[runtime.server]
addr = ":8080"
route_prefix = ""                        # prepended to every REST/GraphQL/WS route
# strip_trailing_slash = true            # "/users/" routes as "/users" (internal
                                          # rewrite, no redirect; off by default)
# idle_timeout   = "120s"                 # keep-alive cap (default 120s; "-1s" = Go's)
# read_timeout   = "0s"                   # OFF by default (would cut large uploads)
# write_timeout  = "0s"                   # OFF by default (would cut SSE/downloads)
# max_body_bytes = 33554432               # OFF by default — set it; every JSON
                                          # handler is otherwise an unbounded
                                          # memory sink. Over-limit → 413.
# shutdown_timeout = "10s"               # graceful-drain window on SIGINT/SIGTERM.
                                          # Default 10s in prod, 250ms under nexus dev.
                                          # In-flight request contexts are cancelled when
                                          # the window closes, so handlers that select on
                                          # ctx let shutdown finish early.

[runtime.server.listeners.admin]         # optional multi-scope listeners
addr  = "127.0.0.1:7000"
scope = "admin"                          # public | internal | admin

[runtime.websocket]
# WebSocket upgrades bypass CORS and carry cookies, so nexus defaults to
# SAME-ORIGIN for every upgrader: AsWS endpoints, GraphQL subscriptions, and the
# /__nexus streams. List a cross-origin frontend here. Loopback is always
# allowed under `nexus dev`. "*" disables the check.
allowed_origins = ["https://app.example.com", "*.example.com"]

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

# Built-in web security (extension/security shares this engine). Security
# response headers are ON by default even without this block; keys here only
# tune them or enable the opt-ins. CSRF is OFF by default (a token-auth API
# isn't CSRF-vulnerable) — enable it for cookie/session HTML forms.
[runtime.middleware.security]
# headers        = false                 # turn the default headers off
# frame_options  = "SAMEORIGIN"          # "-" omits X-Frame-Options
# referrer_policy = "no-referrer"
csp            = "default-src 'self'"     # opt-in Content-Security-Policy
hsts_max_age   = 31536000                 # opt-in HSTS (seconds; needs https)
csrf           = true                     # opt-in double-submit CSRF

# Databases — TOP LEVEL (not under [runtime]); wired in code via
# db.BindFromConfig[T]("name") (T embeds *db.Manager). Inline values OR a config-server key_prefix.
[databases.main]
driver   = "postgres"
host     = "localhost"
port     = "5432"
user     = "postgres"
password = "${DB_PASSWORD}"              # ${ENV} expanded at load
name     = "myapp"
sslmode  = "disable"
default  = true
# log    = "warn"                        # SQL logging: omit = auto (on in dev,
                                          # silent in prod). Force with silent/
                                          # false/off | error | warn/true/on | info/all

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
frontend as `import.meta.env.client.id` whenever `nexus dev`/`nexus build` start Vite
(passed as `NEXUS_FRONTEND_ENV`; nexus-vite-plugin defines each dotted key in dev and
build — use the member form; the bracket form `import.meta.env["client.id"]` is NOT
substituted). Nested tables flatten with dots (`[env.a.b] c` → `a.b.c`).
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

**`ginrouter` is a SEPARATE module** so gin stays out of the main module's
dependency graph entirely — `go get github.com/paulmanoni/nexus/httpx/ginrouter`
to use it (the import path is unchanged; it just versions independently). `stdrouter`
and `chirouter` ship inside the main module (chi has no transitive deps).

Chain execution (the `c.Next()` / `c.Abort()` flow, recovery, error accumulation)
lives in `httpx.Ctx`, not the router — so every middleware runs identically on any
backend, and the router only matches paths + returns params. App-level middleware
(`Config.Middleware.Global`, etc.) wraps the whole mux so it runs even on 404/405
(CORS preflight relies on this); per-op middleware runs inside the matched route.

`App.Router() httpx.Router` exposes the live router (replaces the old
`App.Engine() *gin.Engine`). Low-level handlers that need raw HTTP take an
`*httpx.Ctx` parameter (replaces `*gin.Context`); `httpx.H` is the JSON map
shorthand (replaces `gin.H`).

### DI container backend (pluggable; built-in by default)

nexus has its **own dependency-injection container** — `github.com/paulmanoni/nexus/di`,
a small zero-third-party-dependency engine — wired behind the same kind of seam as
the router. **It is the default, so the default binary links no `go.uber.org/fx`,
`dig`, `multierr`, or `atomic`.** `go.uber.org/fx` is opt-in:

```go
import (
    "github.com/paulmanoni/nexus"
    "github.com/paulmanoni/nexus/di/fxcontainer"
)

nexus.Boot(nexus.WithContainer(fxcontainer.New()))     // one line
```

`nexus.WithContainer(...)` is the only switch (no nexus.toml key — an adapter must be
imported to link). Selecting fx pulls fx/dig back into the build; the builtin default
does not. Both backends are observably identical (verified by a parity suite); the
builtin is ~60× faster to build a graph at startup, with far fewer allocations — a
one-time boot cost either way.

**User/extension code never names the container.** `nexus.Provide / Supply / Invoke /
Module / Options / Error` are unchanged and now record a backend-neutral `di.Option`
(via the hidden `nexusOption() di.Option` method). The container is an implementation
detail behind those helpers — like fx was before.

- **Lifecycle:** take a `nexus.Lifecycle` parameter (alias of `di.Lifecycle`) in a
  constructor/invoke and `lc.Append(nexus.Hook{OnStart, OnStop})`. Resources
  (`db.Bind`, `cache.Bind`), workers, crons, and the HTTP listeners all register their
  start/stop this way. The fx adapter bridges `fx.Lifecycle` onto it transparently.
- **`nexus.Error(err)`** surfaces an option-build error at boot (replaces the old
  `nexus.Raw(fx.Error(err))` pattern). `nexus.Raw(di.Option)` remains the low-level
  escape hatch.
- **Value groups / optional deps:** internal wiring (e.g. the GraphQL field group,
  the optional default auth gate) uses `di.Annotate(fn, di.ParamTags/di.ResultTags(...))`
  with `group:"…"` / `optional:"true"` tags rather than `fx.In`/`fx.Out` marker structs,
  so both backends translate them identically. Constructors are lazy (run only when a
  result is demanded) and singletons; invokes run eagerly in registration order — same
  semantics as fx.

**`fxcontainer` is a SEPARATE module** so fx stays out of the main module's dependency
graph — `go get github.com/paulmanoni/nexus/di/fxcontainer` to use it. The builtin
container ships inside the main module.

---

## 3. Modules & dependency injection

```go
var Module = nexus.Module("billing",     // stamps "billing" on every endpoint inside
    nexus.Path("/billing"),               // REST + GraphQL prefix in one
    nexus.Provide(NewBillingService),     // constructor(s) into the DI graph
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

A service is a typed wrapper around `*nexus.Service` so the DI container routes by
type and the dashboard groups handlers under it:
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
- Struct tags drive schema + validation: `graphql:"title,required" validate:"required,len=3|120"`, `path:"id"` for REST path params (legacy `uri:"id"` still works) (also `query:"x"`, `header:"X"`, `form:"x"`, `json:"x"`).
- `nexus.Describe("…")` sets an op's description (dashboard + GraphQL SDL) — a cross-transport per-op option (REST / GraphQL / WS), like `HideFromDashboard()` / `WithIcon()`. It supersedes the transport-specific `Desc` (GraphQL) and `Description` (REST), which are deprecated but still work.

**Service methods register directly — no wrapper.** A method (or free function)
with the plain shape `func(ctx context.Context, args T) (R, error)` is a valid
handler as-is: pass the method expression and the receiver becomes a DI-injected
dep, the trailing struct binds like `Params[T].Args`, and the op name derives from
the method name (`CreateUser` → `createUser`; a bound `svc.CreateUser` names the
same). Kills the one-line `NewXxx` delegation wrappers:
```go
func (s *UserService) CreateUser(ctx context.Context, in CreateArgs) (*User, error) { ... }

nexus.AsMutation((*UserService).CreateUser, auth.Requires("add_user"))
nexus.AsRest("POST", "/users", (*UserService).CreateUser)
```
Reach for `Params[T]` only when the handler needs more than ctx+args. Use pointer
receivers: a zero-arg value-receiver method expression would read the receiver
struct itself as the args container.

**Scalar-arg methods (`nexus.Arg`).** A method taking bare scalars registers
without an args-struct wrapper — the option names the wire argument(s):
```go
nexus.AsQuery((*Svc).GetUser, nexus.Arg("id"), nexus.Op("userShow"))
nexus.AsRest("GET", "/users/:id", (*Svc).GetUser, nexus.Arg("id"))
nexus.AsMutation((*Svc).Move, nexus.Arg("id", "employerId"))   // multi-arg
```
Names map POSITIONALLY onto the handler's LAST len(names) params, in order
(Go reflection can't see param names — check the order when types coincide).
The args struct is synthesized at registration (tagged json/query/uri/graphql),
so schema, SDK, and every binder see what a wrapper would have declared;
non-pointer params are required, pointer params optional; the op name still
derives from the method. One or two scalars ride `Arg` well — three or more
deserve a dto. A struct-taking param is rejected with "register it directly".

**Raw form input (`*nexus.Form`) + validation errors (`nexus.Errors`).** For
genuinely dynamic input — file uploads, variable-key forms — declare a `*Form`
param (framework-filled; also reachable below the handler via
`nexus.FormFrom(ctx)`). Reads are source-unified: `Get/All/File` see the same
fields whether the client sent JSON (Inertia's `useForm` default), multipart
(what useForm switches to when a file is attached), urlencoded, or query
fallback. `fm.Bind(&dto)` bridges back to the typed world; typed dtos remain
the default — schema/SDK/validation/maskid ride them, not `fm.Get`.
```go
func NewUploadCv(svc *Svc, ctx context.Context, fm *nexus.Form) (any, error) {
    cv, err := fm.File("cv")            // *FormFile: Name/Size/ContentType/Open (streams)
    ...
    errs := nexus.NewErrors()
    if taken { errs.Field("email", "already taken") }
    if down  { errs.Global("provider unreachable") }
    if errs.Any() { return nil, errs }
```
`nexus.Errors` renders per transport: Inertia pages flash + 303 back (`errors`
prop, `useForm` convention, global under `errors._global`, error bags honored);
REST answers 422 `{"message", "errors": {field: [msgs]}}`; GraphQL carries the
field map in the error's extensions. REST/Inertia only for `*Form` — on
GraphQL/WS the param is a typed nil whose methods no-op.

**Response envelopes (`nexus.Envelope`).** When the API's wire contract wraps every
result (`{status, message, data}`-style), don't convert errors by hand in each
handler — attach the app's wrap function per op and keep handlers on plain
`(T, error)`:
```go
func Wrap[T any](v T, err error) (*Response[T], error) {   // app-owned shape
    if err != nil { return &Response[T]{Status: false, Message: err.Error()}, nil }
    return &Response[T]{Status: true, Data: v}, nil
}

nexus.AsQuery((*UserService).ListUsers, nexus.Envelope(Wrap[[]UserRow]))
```
The GraphQL schema (and generated SDK) declare the wrap's output type — the
envelope is the contract. An error the wrap converts becomes a normal 200/data
response; an error the wrap *returns* follows the standard error path. Binding
and validation failures are never enveloped. REST + GraphQL.

### REST
```go
type GetArgs struct { ID string `path:"id"` }   // path param `:id` binds via the `path` tag (legacy `uri` also works)
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
`dataloader.Fetch[Key, Child]` (DI-injected); (c) inline with trailing DI-injected deps
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

### Decorator-form registration (`//@` annotations) — optional

Instead of listing every handler in a `nexus.Module(...)`, annotate the handler
functions with `//@` doc comments and let codegen do the wiring. **Purely
additive** — it produces the same options as `AsRest`/`AsQuery`/`Provide`, so
annotated and hand-written registrations coexist. Needs the `decorate` package
(`github.com/paulmanoni/nexus/decorate`); the codegen scanner lives in the
`nexus` CLI only, so the app binary links no extra deps.

```go
//@provide
func NewUserService(app *nexus.App) *UserService { ... }

//@rest GET /users/:id
func NewGetUser(s *UserService, p nexus.Params[GetArgs]) (*User, error) { ... }

//@mutation
//@auth Requires("ADMIN")
func NewCreateUser(s *UserService, p nexus.Params[NewUser]) (*User, error) { ... }
```

Annotation catalog (one PRIMARY per func, plus optional modifiers):
```
//@provide                        //@rest <METHOD> <PATH>      //@query / //@mutation
//@subscription                   //@ws <PATH> <TYPE>          //@worker <NAME>
//@auth Required | Requires("X")  (modifier)                   //@use <expr>  (modifier, per-op middleware)
//@<pkg>.<Func> args…             custom extension decorator — emits pkg.Func(args…, fn);
                                  the registrar returns a nexus.Option (e.g. inertia.Page,
                                  reusing its existing signature). pkg is imported from the
                                  annotated file. Args are whitespace-separated; no spaces inside one.
```

**The app needs no wiring** — `main` is just `nexus.Boot()` / `nexus.Run(cfg, …)`:
- `nexus generate handlers [./...]` scans the tree and writes one
  `nexus_handlers_gen.go` per package — `decorate.Register(nexus.Module("pkg", …))`
  with the real `nexus.As*`/`Provide` options — plus a blank-import aggregator in
  the main package so Go pulls every annotated package into the build (Go compiles
  only imported packages; you write no import). `--check` is a CI drift gate.
- `decorate` auto-drains its registry into `Boot`/`Run` via a deferred-options
  hook — no `decorate.Module(...)` call. Each package's endpoints group under a
  `nexus.Module` named after the package (the `main` package → `"app"`), so the
  dashboard groups them automatically.
- **`nexus dev`** AND **`nexus build`** inject the generated files via a build
  overlay (`go run -overlay` / `go build -overlay`) — **nothing is written to the
  source tree** (zero churn). `nexus generate handlers` is the **eject** path:
  run it to commit `*_gen.go` so a bare `go build`/`go install`/`go test` (without
  the nexus CLI) and static tooling (gopls, linters) see the registrations.
  (`nexus build` fails fast if codegen can't resolve a decorator; `nexus dev`
  warns and lets `go run` surface the underlying error.)
- A qualified custom decorator (`//@pkg.Func`, e.g. `//@inertia.Page`) needs the
  `pkg` import resolved for the generated file. The codegen resolves it
  automatically: from the annotated file's imports → its sibling files in the
  same package → a `nexus.toml` `[decorators.imports]` hint → the module import
  graph (`go list`). So you usually need no import in the annotated file; if a
  selector is ambiguous or not a dependency, add `[decorators.imports]` (selector
  → import path) to `nexus.toml`, or import the package. A blank import
  (`_ "…/pkg"`) in the annotated file also works as an explicit opt-in.

Brand decorator-registered endpoints on the dashboard with `nexus.WithIcon(name)`
(a cross-transport per-op option, like `HideFromDashboard()`); extension
registrars bake their icon in (e.g. `inertia.Page` → `app-window`). See
`decorate/DESIGN.md` and `examples/notes` / `examples/inertia`.

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
**SQL drivers are opt-in blank imports** (database/sql style) — `nexus/db` itself
links NO engine; import the one(s) your app opens:
```go
_ "github.com/paulmanoni/nexus/db/postgres" // pgx
_ "github.com/paulmanoni/nexus/db/mysql"
_ "github.com/paulmanoni/nexus/db/sqlite"   // pure-Go engine (~5MB) — don't ship it unused
```
A Config naming an unlinked driver fails at wiring time with the import to add.
File-backed SQLite now gets a small read pool by default (WAL-friendly);
`:memory:` keeps MaxOpen=1. Put a `busy_timeout` pragma in file DSNs.
- `db.BindFromConfig[T]("name", opts...)` — reads `[databases.name]`; `T` embeds
  `*db.Manager`. The `[databases.*]` lookup is deferred to boot, so it works under
  `nexus.Boot` even though Boot loads the TOML after building option args (a bad/missing
  block still fails fast at boot).
- `db.Bind[T]("name", func() db.Config {…}, opts...)` — inline config (no TOML).
- `cache.Bind[T]("name", func() *cache.Config {…}, opts...)` — `T` embeds `*cache.Manager`.
- **`BindFromConfig` reads a nexus.toml block — available on all four binders:**
  `db.BindFromConfig[T]("main")` (`[databases.main]`), `cache.BindFromConfig[T]("session")`
  (`[cache.session]`), `storage.BindFromConfig[T]("uploads")` (`[storage.uploads]`),
  `mail.BindFromConfig[T]("smtp")` (`[mail.smtp]`). Keys are the snake_case field names
  (`redis_host`, `public_base_url`, `from_address`, …); every key is optional; the read
  runs at boot so it works under `nexus.Boot`. Lifecycle options stay explicit in code.
- Options: `db.WithDefault/WithDetails/WithDescription`, `cache.WithDefault/WithDescription`.
- **The default cache is in-memory only and pulls NO heavy deps** (no go-redis, gocache,
  or Prometheus). Redis is opt-in database/sql-style: blank-import
  `_ ".../extension/cache/redis"` and a `production`-mode Manager keeps a Redis connection
  with transparent memory failover. Without that import the binary never links go-redis.
- Cache-backed metrics (multi-replica counters) are opt-in via
  `Config.Stores.Metrics = cache.NewMetricsStore(mgr)`; the default is an in-process
  memory store with no cache dependency.

Handlers/services then take `*DB`, `*CacheManager` as constructor params (the DI container injects).

**Manual API** (for queues or full control): `resource.NewDatabase/NewCache/NewQueue(name,
desc, details, healthy, opts...)` with `resource.AsDefault()/DependsOn(...)/WithDetails(fn)`;
register via `app.Register(r)`, or implement `NexusResources() []resource.Resource` on a
constructor param for auto-detection. Live usage edges: `app.OnResourceUse(target)`.

### File / object storage (`extension/storage`)
A Laravel-Storage-style `Disk` abstraction — one interface, backend chosen by config, so
local-in-dev / S3-in-prod is a `Config` change, not a code change. Wire it exactly like a
cache: a typed `Bind` whose `T` embeds `*storage.Manager`, injected into handlers and shown
on the dashboard (`resource.KindStorage`).
```go
import "github.com/paulmanoni/nexus/extension/storage"

type Uploads struct{ *storage.Manager }

storage.Bind[Uploads]("uploads", func() storage.Config {
    return storage.Config{Driver: "local", Root: "./var/uploads"}   // or Driver:"s3", Bucket, Region, AccessKey, SecretKey, Endpoint
}, storage.WithDefault())
```
Handlers inject `*Uploads` and call the disk directly (Manager embeds `Disk`): `Put` / `Get`
/ `Exists` / `Delete` / `Stat` / `List` / `URL` / `SignedURL`; `PutOption`s `WithContentType`,
`WithSize` (stream without buffering), `Public`. Two backends, **both dependency-free**:
`local` (OS filesystem, path-traversal-safe, atomic writes) and `s3` (any S3-compatible
store — AWS/MinIO/R2/Spaces — over HTTPS with built-in SigV4; **no AWS SDK linked**).
`SignedURL` returns a presigned GET. `nexus docs storage`.

### Outbound mail (`extension/mail`)
A Laravel-Mail / Rails-ActionMailer-style abstraction — app code composes a `Message`
and hands it to one `Mailer` interface; the transport is chosen by config, so
log-in-dev / SMTP-in-prod is a `Config` change, not a code change. Wire it exactly like
a cache or disk — a typed `Bind` whose `T` embeds `*mail.Manager`, injected into handlers
and shown on the dashboard (`resource.KindMail`).
```go
import "github.com/paulmanoni/nexus/extension/mail"

type Mailer struct{ *mail.Manager }

mail.Bind[Mailer]("smtp", func() mail.Config {
    return mail.Config{
        Driver: "smtp", Host: nexus.Get[string]("mail.host"),
        Port: nexus.Get[int]("mail.port", 587),
        Username: nexus.Get[string]("mail.username"),
        Password: nexus.Get[string]("mail.password"),   // from env/nexus.toml
        Encryption: "starttls",                          // none | starttls | tls
        FromAddress: "no-reply@example.com", FromName: "Example",
    }
}, mail.WithDefault())
```
Handlers inject `*Mailer` and call `Send(ctx, mail.Message{To, Cc, Bcc, ReplyTo,
Subject, Text, HTML, Headers, Attachments})` — text+HTML becomes multipart/alternative,
attachments make it multipart/mixed; recipients are validated and the MIME message is
built for you. Two backends, **both dependency-free** (no third-party mail library):
`log` (the **default** empty-driver backend — prints messages, sends nothing, safe for
dev/tests; exposes `.Sent()` for assertions) and `smtp` (any SMTP server over stdlib
`net/smtp` — STARTTLS/587, implicit TLS/465, PLAIN auth; port defaults per mode).
`nexus docs mail`.

### Sessions (`extension/session`)
Django-style server-side sessions — a cookie carries an opaque ID, data lives in
a pluggable `Store`, handlers use a lazy handle. For anonymous visitors and
logged-in users alike; available on REST, Inertia, and GraphQL (`p.Context`).
```go
import "github.com/paulmanoni/nexus/extension/session"

nexus.Boot(session.Module(session.Config{}))   // memory store, 14-day TTL

s := session.Get(p.Context)
s.Set("cart", skus)          // first write mints the ID + sets the cookie
s.Cycle()                    // rotate ID on login (fixation defense)
s.Destroy()                  // logout: delete store entry + expire cookie
```
Lazy like Django: no store hit until touched, no save unless modified
(`s.Touch()` forces one), cookie set on first WRITE only — so write the session
before the response body. Values must round-trip JSON. Stores: the default
`NewMemoryStore()` survives `nexus dev` rebuilds (dev-state) but not production
restarts; `session.CacheStore(nexus.Cache)` rides extension/cache (Redis =
restart-safe + multi-replica); or implement `Store` over your DB. Cookie is
always HttpOnly, SameSite defaults Lax — set `Secure: true` behind TLS.
`nexus docs session`.

### Opaque IDs (`extension/maskid`)
Replaces sequential integer IDs with 22-char opaque strings on the wire and
converts them back before the handler runs — handlers, GORM models and SQL keep
using `int64` keys. One option, no app-code change:
```go
import "github.com/paulmanoni/nexus/extension/maskid"

nexus.Boot(maskid.Module(maskid.Config{Key: os.Getenv("MASKID_KEY")}))
```
`{"id": 41, "ownerId": 7}` goes out as `{"id": "9tKq3nB1wZ0aVdH7cRmXsA", …}`. Covers
all four transports: REST out (the reflective JSON write), REST in (path/query/
header/form/JSON — one hook in `httpx` binding), **GraphQL** (*output* ID fields are
declared as the `MaskedID` scalar rather than `Int` — it can't be a response rewrite
because graphql-go coerces through the declared type; arguments stay `Int`, since a
masked value in `variables` is already converted back when the body is bound, so the
SDL and generated client don't change), **Inertia**
(masked after `resolveProps`, so `Defer`/`Optional` props are covered; the scope is
decided once from the page's props struct, so a bare `[]uint` sidecar of IDs masks
alongside the entity list it pairs with), and
**WebSocket** (inbound envelope `data`, every outbound `Emit`). A request carrying a
raw integer still works — an unmasked value simply doesn't decode — so rollout is
incremental.

Default policy: any JSON key `id`/`ids` or ending in `Id`/`ID`/`_id` (optional
plural) whose value is a whole number. The suffix test is case-sensitive, which is
what keeps `valid`/`paid`/`android` out. Tune with `Include`/`Exclude`/`Match` — and note `Exclude` prunes the whole
subtree under a key, which is how reference data (`countries`, `categories`) stays
numeric even though a lookup row's key is also spelled `id`.

**Scoping** — `Config.Types` (or `MatchType`) narrows *masking* to named response
types, for when masking isn't safe app-wide (some IDs travel to a system outside
the app). The name is the Go type of the response, which is also its GraphQL
object name; pointers, slices and generic wrappers (`Response[T]`) all resolve to
the underlying name. Unmasking is never
scoped and needs no scope — a value converts only if it decrypts, so an
out-of-scope plain integer is unaffected either way.

Codec: deterministic AES over one block (8-byte domain tag ‖ big-endian id) →
base64url. Deterministic keeps URLs bookmarkable and caches working; the tag
authenticates, so a forged mask is rejected rather than decoding into another
record's id. Real encryption, unlike hashids/sqids. Swap it via `Config.Codec`.
`maskid.Mask/Unmask` are available to app code.

**Masking is not authorization.** It removes enumeration and inference; a masked id
is still a bearer reference. Every auth check you needed before, you still need. And
do not enable it for ids that travel to a system outside this app (a legacy backend
the SPA also calls, a partner webhook) — those consumers get strings they can't use,
and handing the browser a way to reverse the mask defeats the point. `nexus docs maskid`.

### Request-scoped derived values (`nexus.NewScoped`)
A fact computed from the request (identity + DB, tenant state) at most once
per request, on first ask, shared by every handler/service/prop that asks —
Django's "compute it in middleware, hang it on request" without the eager cost:
```go
var delegatedScope = nexus.NewScoped[Scope](func(svc *services.UserMgmtService) nexus.Compute[Scope] {
    return func(ctx context.Context) (Scope, error) { ... }
})
nexus.Boot(delegatedScope, ...)          // the handle is an Option
scope, err := delegatedScope.Get(ctx)    // anywhere, any transport
```
The handle auto-provides itself into DI, so a handler can DECLARE the fact
it reads: `func NewUsersPage(ctx context.Context, scope *nexus.Scoped[Scope], ...)`.
Distinct fact types are distinct DI slots; two handles of the SAME T in one
app trip the duplicate-provider boot error — mark extras `.NoProvide()`.
Lazy (never asked → never computed; no Scoped registered → zero overhead),
singleflight per request (concurrent GraphQL resolvers share one compute),
error memoized. Per-request ONLY — a fact that tolerates staleness belongs on
the identity (resolve-time enrichment rides the auth cache); one that outlives
requests belongs in extension/cache. Unit tests inject with
`nexus.WithScopedValue(ctx, handle, v)`. `nexus docs scoped`.

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
    })   // first param MUST be context.Context; the rest are DI-injected

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
`auth.Requires("ROLE_X")` (403). UI toggles ride the same rulebook:
`auth.Can(ctx, "add_user")` and `auth.Gates(ctx, "add_user", "delete_user")
map[string]bool` evaluate through the identical PermissionFn/Backend.Authorize
the `Requires` gate consults, so a page's "can" props cannot drift from the
endpoint gates.

**Op gates — permissions declared once, frontend asks by op name.**
`auth.Requires` stamps its permission list onto the endpoint's registry entry,
and `auth.OpGates(ctx, app)` answers `map[opName]bool` for every registered op
— so the frontend keys on op names it already calls (`can.saveUser`) and no
permission string exists outside the registration. Wire it app-wide as one
Inertia shared prop:
```go
inertia.ShareProvide(func(app *nexus.App) inertia.SharedProvider {
    return func(ctx context.Context) (string, any) { return "can", auth.OpGates(ctx, app) }
})
```
Ops without `Requires` are always true (it reports permission gates, not
authentication); evaluation is server-side through Backend.Authorize, and the
registry is compiled once per version into a table grouped by unique
permission set (~4µs for 200 ops), so it is safe on every page render.
When handlers also need the gates, declare the fact ONCE as a Scoped and
project it with `inertia.ShareScoped("can", CanGates)` — handlers call
`CanGates.Get(ctx)`, pages read `props.can`, one compute per request serves
both; the same bridge works for any named request fact (features, quota). A
failed derivation omits the key from the render; handler Gets still error. Extractors: `auth.Bearer()`, `auth.Cookie(name)`,
`auth.APIKey(header)`, `auth.Chain(...)`. Typed user in a handler:
`u, ok := auth.User[MyUser](p.Context)`. Logout: take `*auth.Manager`, call
`Invalidate(token)` / `InvalidateByIdentity(id)`. A full OAuth2 server is
`oauth2.Module(oauth2.Config{...})` (`extension/oauth2`) — password grant →
JWT access/refresh at `/oauth/token`.

**Cohesive backend (`Config.Backend`).** Instead of a static `Scheme.Resolve`
(which can't see DI deps — forcing package globals + a backfill `Invoke`) plus a
separate `Authorization` block, declare ONE DI-constructed backend that owns
resolve + login + authorize: `Backend: auth.UseBackend(func(db *DB, srv *Srv)
*AuthBackend { return NewAuthBackend(db, srv) })` (or `auth.StaticBackend(v)` for
no deps). The framework discovers capabilities by type assertion — implement any
subset: `Resolve(ctx, token)` (fills schemes with a nil `Resolve`), `Login(ctx,
Credentials)` (powers `Manager.Login`), `Authorize(id, required) bool` (replaces
`Config.Authorization`), plus the token-server trio `Issue(ctx, *Identity) (any,
error)` (login response / token pair), `RevokeToken(ctx, token) error` (logout),
`TokenHandler() httpx.HandlerFunc` (raw grant endpoint). The ctor returns YOUR
concrete type, not the `auth.Backend` login interface. Fully additive — the zero
value keeps existing configs identical.

**Endpoints (`Config.Endpoints`).** Let `auth.Module` mount its own HTTP front
doors from the backend's capabilities, so one `auth.Module(auth.Config{...})`
owns the whole surface (no hand-wired `AsRest` lines): `Endpoints:
auth.Endpoints{Login: "/api/auth/login", Logout: "/api/auth/logout", Token:
"/oauth/token", Revoke: "/oauth/token/revoke"}`. Each is off unless its path is
set; all are Public. `Login` runs `Backend.Login` then `Backend.Issue`; `Logout`
/`Revoke` do `Manager.Invalidate` + `Backend.RevokeToken` (token via
`Endpoints.LogoutExtract`, default `Bearer()`); `Token` serves
`Backend.TokenHandler`. This supersedes the now-deprecated `auth.LoginEndpoint`
/`auth.LogoutEndpoint` (still work as thin wrappers). For a full OAuth2 server,
`oauth2.Backend(oauth2.Config{...})` returns a ready `auth.BackendOption`
implementing every capability — drop it into `Config.Backend` (`oauth2.Module`
is now a thin wrapper over exactly this, holder-free). `nexus docs auth`.

**Passwords & credential login (Django-style, swappable).** `auth.Module`/`Resolve` above
verifies an *existing* token; these fill in the *login* half, each a pluggable interface
with a shipped default (no new deps — `x/crypto` + stdlib `crypto/pbkdf2`):
- **Hashing** — `auth.Hasher` / `auth.Hashers` (≈ Django `PASSWORD_HASHERS`). Encoded
  hashes self-describe (`<id>$<payload>`) so a set verifies any member algorithm and
  **rehashes on login** when stale. `auth.BCrypt()` (default), `auth.Argon2id()`,
  `auth.PBKDF2()`; `auth.DefaultHashers()`.
- **Policy** — `auth.PasswordValidator` (≈ `AUTH_PASSWORD_VALIDATORS`): `MinLength`,
  `NotNumericOnly`, `NotCommon`, `NotSimilarToUser`; run via `auth.ValidatePassword(...)` /
  `auth.DefaultValidators()`.
- **Login** — `auth.Backend` + `auth.Authenticate(ctx, cred, backends...)` (≈
  `AUTHENTICATION_BACKENDS`, tried in order). `auth.ModelBackend` checks a pluggable
  `auth.UserStore` (implement `ByUsername`/`ByID`/`SetPassword` for your model; ships
  `auth.NewMemoryUserStore()` for dev). Wrong password *or* unknown user →
  `auth.ErrInvalidCredentials` (no enumeration; timing equalized).
```go
store := auth.NewMemoryUserStore()
store.CreateUser("alice", "s3cret-pw", "ADMIN")
id, err := auth.Authenticate(ctx, auth.Password{Username: "alice", Password: "s3cret-pw"},
    auth.NewModelBackend(store))
```
`nexus docs auth`.

**Built-in web security** (headers + CSRF) is separate from identity — it's the
`[runtime.middleware.security]` block (§2) / `extension/security` plugin: safe response
headers on by default, opt-in CSRF. `nexus docs security`.

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

**Typed Inertia pages and shared props.** `inertia.Page` records its component, so
`client.d.ts` declares `NexusPageProps` (component → the handler's props type) and
`NexusSharedProps` (from `inertia.ShareScoped[T]` / `inertia.ShareTyped[T]`; plain
`Share` stays untyped). A page types its props with
```ts
import type { NexusPageProps } from 'nexus-client'
const props = defineProps<NexusPageProps['Users/Index']>()
```
(indexed access only — Vue's compiler rejects a generic helper), and `usePage().props`
is typed through the generated `inertia.d.ts`. Pages are not REST calls in the SDK.
`nexus({ pages: 'src/Pages' })` in `vite.config` warns in dev and fails `vite build`
when a registered component has no file.

**Envelope-aware calls (`nx.op`) + batching.** Ops registered with
`nexus.Envelope` are marked in the manifest; `await nx.op('usersList', vars)`
picks query/mutation from the manifest, unwraps `{status, message, data}`
(resolves `data`, throws `NexusOpError` with the envelope's message on
`status:false`; `{unwrap:false}` returns the raw envelope), and is typed
`Promise<GqlData<'usersList'>>`. Independent queries issued in the same
microtask (a `Promise.all` opening a dialog) coalesce into ONE aliased
GraphQL request automatically (`{batch:false}` opts out). Vue:
`useOpQuery(name, args)` (in-flight dedupe per op+args) and
`useOpMutation(name, {refresh: ['usersList'], latest, onSuccess(data, message)})`
— `refresh` refetches every mounted `useOpQuery` of those ops after success,
`latest` is the auto-save race guard.

**Simplest enable — one switch (`sdk = true`):** set `Config.SDK` (or `[runtime] sdk =
true` in nexus.toml) and nexus generates + serves the full typed SDK and, when a frontend
dir is present (any `vite.config.*`), dumps the SDK files into `web/sdk` + wires tsconfig
so `import 'nexus-client'` resolves with types — no `client.Config` ceremony. **The dump
runs in development only** (`nexus dev` or `environment = "development"`); a production
binary never writes files. `client.Off` on `OutDir` is an explicit "no dump". PocketBase-style. **Independent of
`introspection`** — the SDK is the app's own browser bundle's import, so it keeps serving
in a locked-down production binary; the routes are public and the manifest maps your API
surface, so vendor with `nexus client --out` instead if you don't want that published.
`introspection` governs `/__nexus`; `sdk` governs the client. For finer control (custom path, route middleware,
explicit OutDir, per-deployment gating) set `Config.Client` / `nexus.ClientUse(...)`
directly instead.

---

## 11. CLI cheatsheet

```
nexus new <dir>      Scaffold an app + nexus.toml. --frontend vue|react (a Vite project
                     under web/), --inertia [--ssr], --db, --cache, --auth,
                     --module <path>, --yes (no prompts). --tooling: deprecated, ignored.
nexus init [dir]     Add a Vite frontend (web/) to an existing project and patch main.go.
                     --frontend (req). --force: add the project files to an existing
                     web/, keeping index.html and src/ (the viteless → Vite migration).
nexus dev [dir]      Live dev: the app + dashboard on its own origin, and — when the
                     frontend dir has a package.json — its Vite beside it (deps
                     installed on first run; open the app URL it prints, never Vite's).
                     Go rebuilds are build-then-swap (old binary serves through the
                     compile; broken builds keep it up; no-op builds skip the restart).
                     DWARF is stripped and the frontend bundle is stubbed out of
                     the dev binary (--debug / --no-embed-stub opt back in).
                     --dist keeps web/dist rebuilt (vite build) in the background so
                     go build always embeds the current frontend. --frontend <dir>
                     overrides the detected dir. --go-run = legacy loop.
                     --frontend-cmd: deprecated, ignored.
nexus build          npm ci (if needed) → vite build [→ vite build --ssr] → web/dist,
                     then go build embeds it. ONE binary (frontend + Go). -o <path>.
nexus client [--out dir]   Write the embedded JS/TS client SDK to disk.
nexus generate frontend    Typed TS source tree from a manifest (--check = drift gate).
nexus generate handlers [./...]  Wire //@-annotated handlers: write nexus_handlers_gen.go
                     per package + a main-package import aggregator. --check = CI drift gate.
                     (Run automatically by nexus dev/build; see §5.)
nexus docs [topic]   Inline reference. --web opens the README.
nexus pki ...        Generate mTLS certs for the peer mesh.
```
`nexus build` produces ONE binary (frontend + Go). There is no deployment-split CLI and
no Dockerfile generator.

---

## 12. Conventions & gotchas

- **Pure-Go build** — no `-tags`, no CGO. A frontend needs **Node.js 20+ and npm at dev
  and build time** (a real Vite project); the shipped binary runs without them.
- **Dashboard 404s unless `introspection = true`** (or `nexus dev`). It's locked down
  by default for production.
- **In dev, open the app's origin** (`:8080`), never Vite's port — pages load their
  modules from Vite through the hot file; there is no proxy. `web/dist` is the production
  artifact, built by `nexus build` and embedded.
- **Frontend deps**: `nexus dev`/`nexus build` run `npm ci` (or `npm install` without a
  lockfile) when `web/node_modules/.bin/vite` is missing. Commit `web/package-lock.json`
  and `web/sdk`; `web/dist/*` (except the committed `index.html` stub) and
  `web/node_modules` are gitignored. Keep `typescript` on `~6.0` (vue-tsc 3.3 crashes on 7).
- **Deploy with `NEXUS_ENVIRONMENT=production`** — scaffolds ship `environment =
  "development"` in nexus.toml, which turns on dev-only behaviour (the hot file; with
  `sdk = true`, the SDK dump into `web/sdk`).
- **Handler constructors are `NewXxx`**; the `New` prefix is stripped for op names.
- Don't reference `nexus.DeployAs` / `nexus.IfDeployment` — not implemented.
- `nexus docs <topic>` is the authoritative per-feature reference inside the installed
  binary; prefer it when unsure of an exact signature.
