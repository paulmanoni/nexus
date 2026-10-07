# nexus — framework guide for Claude Code

nexus is a Go backend framework: typed reflective handlers over REST + GraphQL +
WebSocket, dependency injection (a built-in zero-dep container; fx optional), a **Vite**
frontend embedded in the binary, and a live introspection dashboard at `/__nexus`. This
file tells you how to use every feature. Verify APIs against the installed version; `nexus
docs <topic>` prints an inline quick-reference for any feature (`nexus docs --list`).

Import path: `github.com/paulmanoni/nexus/v2`. Pure-Go build — no CGO, no build tags.
A frontend is an ordinary npm-managed Vite project: **Node.js 20+ and npm are dev- and
build-time requirements**; the runtime is still a single Go binary with `web/dist`
embedded, and runs without Node (an Inertia SSR server is the one opt-in exception).

---

## 1. Frontend (Vite, embedded SPA / Inertia)

nexus drives the project's **own Vite** (`web/node_modules/.bin/vite`) and embeds its
output: `nexus build` runs `vite build` into `web/dist`, and `go build` embeds it via
`//go:embed`. Any framework, Vite plugin or npm library works. A `web/` without a
`package.json` is served as-is; `nexus init --frontend vue --force` adds the Vite files
and keeps the sources.

### Layout
```
web/
  package.json          # vite ^6.4.3, @vitejs/plugin-vue ^5.2.4 (react: plugin-react ^5.2 + React 19),
                        # typescript ~6.0.3, vue-tsc ^3.3.11 (vue-tsc 3.3 crashes on TypeScript 7)
  package-lock.json     # commit it (or pnpm/yarn/bun's lockfile) — installs are frozen to it
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
`nexus.Frontend`/`frontend.Plugin` call names (AST scan, resolved against the package dir)
> `web/` when it has a `package.json` — `nexus build` resolves it the same way (it takes
`--frontend` too). **No `package.json` → no Vite**: a hand-written or prebuilt `dist` is served as-is
(e.g. `examples/petstore-spa`).

### main.go wiring
```go
import "embed"

//go:embed all:web/dist
var webFS embed.FS

func main() {
    nexus.Boot(nexus.Frontend(webFS, "web/dist") /*, modules… */)
}
```
`nexus.Boot(opts...)` loads `nexus.toml` automatically — runtime `config.Runtime`, every
`[extensions.*]` block, the `[env]` bridge, and the `config.Get` value store — then
runs the app. It's sugar for `nexus.Run(config.MustLoad(),
append([]nexus.Option{nexus.MustLoadExtensions()}, opts...)...)`; a missing `nexus.toml` is tolerated,
a malformed one — or one with an unknown key or undeclared section — fails boot. Override the path with `NEXUS_CONFIG` or use
`nexus.BootFrom(path, opts...)`. Reach for `nexus.Run(cfg, opts...)` directly when you
build `config.Runtime` in Go. (Extension packages still need their blank import — Go links only
imported code; `Boot` removes the load calls, not the imports.)
`nexus.Frontend(fs, root, opts...)` is SPA-aware: extensionless paths fall back to
`index.html`, and REST/GraphQL/WS routes win on conflict. Mount under a sub-path with
`nexus.FrontendAt("/admin")`. **Caching comes from the build:** a file is `immutable`
only when the Vite manifest lists it as output *and* its name carries a content hash;
everything else (and the shell) is revalidated with an ETag, so repeat requests are
304s. In production, boot fails fast when the bundle has neither `index.html` nor a
Vite manifest — a manifest alone boots a module-only (Inertia) build, whose unknown
routes then 404; in development an unbuilt bundle serves a placeholder instead.

**The Vite handshake (`nexus-vite-plugin` + `internal/vitehot`).** With `nexus()` in
`vite.config`, `vite dev` writes `<outDir>/.vite/nexus-hot.json` (the dev server's
bound origin, base, entries, pid) and removes it on exit. `nexus.Frontend` and the
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
serving, never for files Vite hot-updates. Design:
`docs/design/frontend-seam.md`.

**`[env]` → the bundle.** `nexus dev`/`nexus build` pass nexus.toml's `[env]` table to
Vite as `NEXUS_FRONTEND_ENV` (JSON of dotted keys); the plugin defines
`import.meta.env.<dotted.key>` in dev and build. A Vite you start yourself gets none.

### Build / serve commands
```
nexus dev                # build-then-swap Go loop + the project's own Vite — see below
nexus build              # install (if needed) → vite build [→ SSR build] → web/dist, then go build
```
`nexus build` treats `<dir>/package.json` as "there is a frontend" (none → pure-Go
build). Steps: install deps only when `node_modules/.bin/vite` is missing or an earlier
install was interrupted — with the project's own package manager (`packageManager` in
package.json, else the lockfile: npm/pnpm/yarn/bun, frozen when a lockfile exists; a
clear error when the tool isn't on PATH; Yarn Plug'n'Play is refused — set
`nodeLinker: node-modules`) → write
`web/sdk/nexus-vite-plugin.{js,d.ts}` → `vite build` → if `src/ssr.ts` exists, `vite build
--ssr src/ssr.ts --outDir dist/ssr` (without emptying `dist`) → require
`dist/.vite/manifest.json` or `dist/index.html` → `go build`.

### `nexus dev` — the dev model (IMPORTANT)
`nexus dev` supervises the project's **own Vite** beside the Go app:
- First run installs dependencies (the project's package manager, as for `nexus build`)
  when `node_modules/.bin/vite` is missing, writes `web/sdk/nexus-vite-plugin.*`, then spawns `vite` in the frontend dir
  (its own process group, no forced `--host`) with `NEXUS_FRONTEND_ENV`.
- The dev origin comes from the **hot file**, never from Vite's stdout. **Open the URL
  `nexus dev` prints — the Go app's origin** (`http://localhost:8080` or your `addr`); the
  dashboard is on the same origin. Vite's own `Local:`/`Network:` banner is hidden (it
  would send you to the wrong port); its other output is prefixed `[web]` (`--verbose`
  shows everything). `--tui` runs Vite too.
- On exit Vite gets SIGTERM, then SIGKILL after 2s, and `nexus dev` waits, so the plugin
  removes its hot file. There is no Inertia "mode" any more (no `go list -deps` scan, no
  `[runtime.inertia] enabled`): SPA and Inertia apps share one dev topology.
In production the embedded `web/dist` is served at the app port via `nexus.Frontend`.

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

**The link is the rebuild.** Compilation is cached per package; the one step no
cache makes incremental is the link, and it dominates (~all of a warm rebuild).
Two defaults follow from that, both dev-only:
- **DWARF is stripped** (`-ldflags=-w`, the old opt-in `--fast`). Pass `--debug`
  to keep it when you need delve or a complete panic trace.
- **The frontend bundle is stubbed out of the dev binary.** Under `NEXUS_DEV`
  `nexus.Frontend` reads `web/dist` from disk (and pages load their modules from Vite
  anyway), so the embedded copy is dead weight that gets relinked on every save. `nexus
  dev` maps it to empty files via the same `go build -overlay` it already uses
  for handler codegen — no build tags, no source changes. Scoped strictly to the
  tree a `nexus.Frontend` call names, so assets your app genuinely reads at
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

**Keeping in-memory state across a rebuild (`dev.Preserve`).** The rebuild replaces
the process, so maps die with the old binary. Hand the state to the dev loop instead:
```go
func NewStore() *Store {
    s := &Store{notes: map[int]Note{}}
    dev.Preserve("notes", s)          // no-op outside nexus dev
    return s
}
func (s *Store) SnapshotDev() ([]byte, error) { return json.Marshal(s.notes) }
func (s *Store) RestoreDev(b []byte) error    { return json.Unmarshal(b, &s.notes) }
```
Restore happens inside `dev.Preserve`, so lazy DI construction is fine; the snapshot is
written on the graceful shutdown `nexus dev` triggers before the swap. Without methods to
write, use `dev.PreserveJSON(name, get, set)`. extension/auth's memory token store and
extension/session's memory store are preserved this way, so a sign-in survives a
rebuild. Dev-only (gated on the state file
`nexus dev` passes), per-session (state survives rebuilds, not Ctrl-C), graceful exits
only, and best-effort — a failed snapshot/restore is reported and skipped, never fatal.
Caches are deliberately not preserved. `nexus docs devstate`.

For state that already has its own on-disk format (an embedded key/value store,
a SQLite handle), `dev.StateDir()` returns that session directory — point
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
The app logs through `log/slog`: `App.Logger()` — also DI-provided as `*slog.Logger`,
taken by the db/cache binders and `nexus.Managed` — writes JSON to stdout (AddSource,
level from `[runtime.logging] level`, default info); `nexus.WithLogger(l)` replaces it
(wrap a zap core in an `slog.Handler` for zap's encoder; don't `Provide` a second one).
`nexus dev` reshapes the app's structured JSON log lines (slog's, or zap's) into a columnar,
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
prints the app's URL. Scaffolds write
`environment = "development"` to nexus.toml; **deployments set
`NEXUS_ENVIRONMENT=production`**, which overrides it. SSR: `ssr.ts` uses
`@inertiajs/vue3/server`; `nexus build` writes `web/dist/ssr/ssr.js` with its deps bundled
(`ssr.noExternal`), so `node web/dist/ssr/ssr.js` (:13714) runs beside the binary without
`node_modules` (it is embedded with `all:web/dist`, but `nexus.Frontend` never serves
`dist/ssr`); main.go passes `inertia.Config{SSR: ssrhttp.New("")}`; under `nexus dev`
pages render client-side.

---

### Reactive templ views (`nexus/view`)
`github.com/paulmanoni/nexus/v2/view` (its own module) renders templ components and
keeps them reactive with no JS build. Plain templ; reads decide reactivity:
`{{ c := view.State(ctx, 0) }}` (component state), `{ c.Get() }` / `attr={…c.Get()…}`
(kept current in the browser), `onclick={ c.Set(…) }` or `view.Do(func(e view.Event){…})`
(the only actions — signals have Get and Set), `if c.Get()…` (branch chosen in the
browser), and server code reading a signal (`{{ }}`, `for`, component args) makes the
component a **shard** re-rendered on the server (a nexus op). Shared page state is DI:
a struct of `*view.Signal` fields (`view.Initial(v)` defaults), read with
`view.Use[*T](ctx).Field` — each page gets its own copy; services come back as is.
`//nexus:page GET /` / `//nexus:auth` / `//nexus:use` above a component register it; a shard inherits
its pages' gates (module-wide render graph; they must agree) or names its own. Zero
wiring: the generator emits `view_gen.go` per package (pages, shards, `view.Expose` for
every `view.Use[T]`) and `view_imports_gen.go` in main. That generated Go is never committed:
**`nexus dev`** compiles on start and on every `.templ` save and writes it beside the `.templ`
files, so any editor sees it (gitignore `*_templ.go`, `view_gen.go`, `view_imports_gen.go` —
`nexus new` does; dev hints when git doesn't; `--no-view-files` keeps it in memory, the build
overlaying it) and runs `tailwindcss --watch` for any
stylesheet with `@import "tailwindcss"` (outside the Vite frontend; `input.css`→`output.css`,
writing `sources.generated.css` with @source for the project + Go deps shipping .templ, and an
@import for each `[runtime.tailwind] imports` entry — `"<module>/<file>.css [layer(base)]"`, a component
library's stylesheet resolved from its Go module).
**`nexus build`**: views via the overlay, Tailwind minified. **`nexus test` / `nexus vet`**:
go test/vet through the same overlay. `nexus generate views [--check]` writes/verifies on
disk. **Editor: `nexus lsp`** for `.go` and `.templ` — it fronts gopls and opens the
generated Go (views + `//nexus:` handler registrations) as unsaved buffers, so Go calling a
component type-checks and go-to-definition lands in the `.templ`; `.templ` files get the views
compiler's errors, Go type errors of their expressions, and definition/hover/completion
through gopls via templ's source map; bad `//nexus:` directives are diagnostics. It's a
drop-in gopls (accepts `serve`/gopls flags, passes other subcommands through), so VS Code
uses it via `go.alternateTools.gopls` and a wrapper script.
Component libraries (templUI): reactive entries in a `templ.Attributes{…}` literal
(Props.Attributes) compile like element attributes; `view.Assets(prefix, handler)` serves
library/CSS files. **Live views** — three tiers as in Phoenix: templ components (stateless), live views,
signals (browser). A live view is a struct embedding `view.LiveView` **by value** (helpers: `Connected`, `Subscribe`,
`Track`/`Untrack`, `PushPatch`/`PushNavigate`, `PutFlash`/`Flash` (cleared by the page's next event), `ID`); the
same type is a page when routed — `view.Live[*T](path, gates…)` (`.Provide(NewT)` optional: the DI instance, else
the zero value, is the template) or `//nexus:live /path` on the type (+ type-level `//nexus:auth`/`//nexus:use`) —
and a part of a page when embedded — `@view.Component[*T](id, props)` (no registration unless it takes deps:
then `view.Live[*T]("")` / `//nexus:live` with no path). Server-owned state per place shown over a WebSocket;
conventions `Mount(ctx, deps…, [props P]) error` (P a struct: routed → bound from `path:`/`query:` tags +
`validate:`, 422 on failure; embedded → the parent's), optional `Update(ctx, deps…, props P)` (a URL patch or new
props; else Mount again), `templ (x *T) Render()`, events = exported `func(ctx, deps…, args…) error`
(deps are pointer/interface params) sent with `view.Send(x.Method, args…)`; the page is re-rendered
and patched in place (focus/typing kept; signals win over the server copy; an element with an `id` and
`data-nx-ignore` is left as the browser has it until its id changes — script-drawn charts, app shells; a component library's random ids — templUI/shadcn-templ `id-`+`rand.Text()` — are renamed after their place on the page, so one state renders one markup). Push: `x.Subscribe(topics…)`
in Mount (embedded views too) + `view.Broadcast(ctx, topic, data)` from anywhere → the subscribed views' optional
`Info(ctx, deps…, msg view.Message) error`, then re-render; across replicas with `view.UseRelay(r)` (`view.Relay`: Publish/Subscribe bytes;
Redis pub/sub `extension/cache/redis/viewrelay.New(Config{URL})`, `view.NewMemoryRelay()`), data as JSON —
read with `msg.Decode(&v)`. Forms: `view.Submit(x.Add)` /
`view.Change(x.Validate)` on a form send its fields to an event whose last param is a `form:`-tagged
struct (httpx binder); failing its `validate:` tags or returning `nexus.Invalid()` re-renders with `view.Errors(ctx).Field(name)`,
success resets the form. With CSRF on, the runtime sends `X-XSRF-TOKEN` on shard re-renders and adds a `csrf_token` field to same-origin POST forms as they submit. Field rules (docs/guide/views.md "Form fields"): a field keeps what the user typed until
the server's value (input `value`, textarea text, select's `selected` options, `checked`) changes; a focused field
is never overwritten; `view.Value(v)` (spread: `<input { view.Value(x.V)... }/>`, also select/textarea) makes a field
take the server's value on every render. The first render
is HTTP-only (a join id lets the socket skip resending it); updates travel as a LiveView-style render
tree: viewgen post-processes templ's Go (`viewgen.Instrument`; `view.Record`/`Rec` S/Open/Close/ForStart/
Item/ForEnd) so a render records statics vs dynamics, loops as item frames, branches/component renders as
frames; statics sent once per connection by fingerprint id, then only changed dynamics (`{"u":{i:…}}`,
loop steps `{"k":[…]}`, long markup by ref `{"r":id}`, opaque markup as a token patch `{"p":…}`) — rdiff.go,
mirrored by runtime.js; uninstrumented code (hand-written components, plain `templ generate`) is one
dynamic (`go run …/view/viewgen/cmd/instrument <dir>`; `make view-ui` for the kit); only modules that
require nexus/v2 are instrumented. **Change tracking:** a live page whose fields are all `view.Assign[T]` (`Get`/`Set`/`Update`)
skips spots (`Rec.Guard`: loops/branches/component calls using no template locals) whose Assigns and `view.Errors`
didn't change; checked against a full render under nexus dev/tests (`NEXUS_VIEW_VERIFY`); the compiler warns
(lsp/generate views/dev/doctor, `viewgen.Plan.Warnings`) on a plain field of such a page and a `view.Use` service call in its Render.
**Embedded live views**: instance per id while rendered (dropped with its subscriptions/presence when not);
`view.UpdateComponent[*T](ctx, id, props)`; `view.Send(c.M)` reaches its own instance (Send names an embedded
type, the browser finds the nearest `<nx-c data-nx-ct>`); with Assigns its event renders only it + spots around it. A field tagged `view:"-"` (not read by Render, or fixed after
Mount) leaves a page tracked. Split Render along what changes together (`@table(p.tableView())`…): a part is skipped whole.
**Uploads**: a `view.Upload` field (`Allow(view.UploadConfig{Accept, MaxEntries, MaxSize})` in Mount), `<input type="file"
{ p.Avatar.Input()... }/>`, `Entries()` (Progress/Done/Err), `Busy()`, `Consume(func(e, *os.File) error)` in the submit event,
`view.CancelUpload(&p.Avatar, ref)`; files POST to `<page>/_upload?t=` (gates, owner-bound, one-shot) into temp files.
**Streams**: a `view.Stream[T]` field — `Configure(idFn)`, `Insert/Prepend/InsertAt/Delete/DeleteID/Reset/Limit`; template
`<ul { s.Attrs()... }>for _, x := range s.Items() { <li id={ s.ID(x) }> }` (Items = latest change only; the runtime applies each
batch once by id). **Presence**: `x.Track(topic, key, meta)` (connected only), `x.Untrack`, `view.Presences(topic)`
([]Presence{Key, Metas}), `view.PresenceDiff` via Info; across replicas over the relay (state every 10s, `view.PresenceTTL`). Reconnect = jittered backoff + queued events + **resume**: the server
parks the page (state + subscriptions) for `view.ResumeGrace` (30s) under a token; same page + same identity
carries on; otherwise a fresh mount, and the browser first re-sends each `view.Change` form (LiveView form
recovery) before queued events, holding the fresh render until those replies arrive. Scale: a page runs on its own goroutine after
the upgrade (the request returns; app stop closes live conns), keeps a shadow tree (fp + maphash sigs; strings
>1KB kept for token patches) released after `view.LiveIdleTrim` (2m) idle; ~60KB/page; 20k users @1 event/5s:
p99 7ms on 10 cores.
`@view.Link(href, attrs…) { … }` is in-app navigation: fetch + patch the body, head assets merged,
live sockets follow, history/back work. From a live page it goes over the socket (LiveView patch/navigate):
same path → *patch* (props re-bound from the URL → `Update`, else Mount; tree diff);
another live page → the connection is handed to its `_live` route through the app router (gates/DI/params
as a page load) and it sends its tree against the connection's statics; else `{"redirect"}` → HTTP load.
`x.PushPatch(href)` / `x.PushNavigate(href)` from events; `nx:navigate` fires on window.
**Forms** (a struct embedding `view.Form`; a page holds it as a field or embeds it — `view.Form`'s own
promoted methods are never events, the form struct's conventions stay shadowable): the struct declares
everything (`form:`/`validate:`/`label:` (else humanised)/`help:`/`placeholder:`/`span:`/`input:` tags);
optional methods on it: `<Field>Choices(ctx) []view.Choice` (receiver = current values → dependent selects;
select rendering), `Validate(ctx) error` (clean(); gates submit — the submit runs only when all rules pass,
and gets the MERGED value), `Init(ctx)` on pointer (once, before the page's Mount) and `Save(ctx) error` on
pointer (the `__save` built-in takes the submit when RenderForm/ui.Form get no method). Fields are the values
(read/write directly; `Load(v)` copies a record's matching fields in). State: loaded data under the browser's
fields; `change` merges only present fields (`fields` lists every control, so unticked boxes go false,
unrendered fields keep values); `Load/Reset` → gen++ (`data-nx-gen` change makes fields take server values);
success → back to loaded. `Changed(field)`, `Update(func())` (mutate inside; validated), `Dirty()`
(`data-nx-dirty`), `Valid/Submitted/Error/F/FieldNames/ChoicesFor`.
Wiring: `view.RenderForm(f, extras…)` (first func = submit, second = change → `data-nx-then` + `__form`
event; `view.LiveValidation`, `view.ConfirmLeave`, `view.FormAttrs`/`templ.Attributes`); `view.ActiveForm(ctx)`
flows to fields. Kit: `ui.Form(f, extras…)` (childless → `ui.AllFields()` grid + `ui.Submit("Save")`),
`ui.Field(name, ui.Label/Help/Placeholder/Options/Disabled/Choices{…})`, explicit `TextField/...Field`,
`ui.Live`, `ui.Submit` (spins while form busy). viewgen fails `F("typo")` in-package (did-you-mean); `ui.Field`
errors at render. `@view.CSRF()` for plain-HTTP forms.
**JS commands** (Phoenix's `JS`): `onclick={ view.JS(view.Show("#m").AddClass("on", "#m").Push(p.Load, id)) }`
— commands chain (each a `JSOp` method; a chain is an immutable value, `a.Then(b…)` joins; `view.JS(a, b)` and
`ops...` spreads still work); also `Confirm(msg)` (stops the chain on no), `SetValue(v, sel)` (fires input/change),
`Copy(sel)`/`CopyText(s)` (sets `data-copied`), `ScrollTo(sel)` — a `templ.ComponentScript` (also in `templ.Attributes`; viewgen wraps it in
`view.ScriptAttr` like `view.Send`); ops `Show/Hide/Toggle` (`view.Display(v)`), `AddClass/RemoveClass/
ToggleClass`, `SetAttr/RemoveAttr/ToggleAttr`, `Focus/FocusFirst`, `Push/PushTo`, `Transition(classes, sel)`,
`PushFocus/PopFocus` (a stack), `Exec(attr, sel)` (runs `view.Commands(…)` JSON — or any script, e.g. view.Send —
held in an attribute: a dialog's `data-cancel`), `Dispatch(event, sel, view.Detail(v), view.NoBubble())`;
`view.Animate(during, from, to)` + `view.Time(d)` (200ms) make Show/Hide/Toggle transitions (classes not sticky; a
new transition on an element finishes the running one). Server side: `x.PushJS(ops…)` / `x.PushEvent(name,
payload)` (window CustomEvent) from an event, Info or connected Mount ride the next reply (`push` field) and run
after it is applied (`view.This()` = the live root; an Error reply drops them; the HTTP render has none); viewtest
`p.Eval(js)`. Target `view.This()` = the
element (pushed: the live root), `view.Closest()`/`view.Inner()` scope a selector. Runtime `__nx.js` (runtime.js) records per element each
attribute a command changed with the server's value at the time; `syncAttributes` re-applies it while the
server renders that attribute unchanged, and drops it when the server changes it (server wins). Show/Hide edit
the `style` attribute's display (+ remove `hidden`). `Debounce(d)`/`Throttle(d)` are chain steps timing the steps after them (runtime `nx.js` hands the rest to
`nx.debounce`/`nx.throttle`, per element+event type; throttle keeps a field's last input; a waiting debounce in a
form runs before its submit). `view.This()` = JS `this`: a command target (selector params are `view.Target` =
any: a string or This(); `""` still means the element) and `.Value()`/`.Checked()`/`.Attr(n)` as Send/SendTo/Push
args, `{"$nx":…}` markers the runtime's `readThis` fills when sending (an event's number/bool param takes a numeric
string). `data-nx-busy` on an element: `aria-busy="true"` from its event's
send to its reply (runtime `busyEls`, kept through morphs, released on socket close; inline CSS makes it inert);
forms being submitted use the same. `@view.Behaviors()` (`/_view/behaviors.js`, also in kit `ui.Script()`):
`data-nx-filter`/`-item`/`-empty`, `data-nx-check-all` (+`data-nx-checks`, `data-nx-checked-count`), `data-nx-valid`;
re-applied after morphs (guarded writes). Ladder: server event → JS command → behavior → island/own script. viewtest: `Visible/Hidden/HasClass/NoClass`. Generator: `view/viewgen` (+ `viewgen/jsgen`, coherence-tested in goja).
**Islands**: `var Chart = view.NewIsland[ChartProps]("Chart")` declares one (props type → registry
`SetIsland` → manifest `islands` → `NexusIslandProps` in client.d.ts; `*view.Signal[T]` types as `T` via
`registry.SchemaAs`); `@Chart(props, view.Visible(), view.SSR()) { fallback }` places it. It mounts a
component of the Vite frontend `nexus.Frontend` serves: each file under `web/src/islands` is one, named by
its path without the extension (.vue → Vue, .tsx/.jsx → React, .ts/.js exporting `mount(el, props, ctx)` →
`{update, unmount}`); strategies load/`Idle()`/`Visible()`/`Media(q)`; props are JSON (an object). Signal
props stay live (runtime hydrates + tracks them; `update` on change) and islands set them back: Vue
`update:<prop>` (defineModel), React `set<Prop>`, `ctx.set`. `view.SSR()` POSTs to the islands server
(`nexus({ islands: { ssr: true } })` builds `dist/ssr/islands.js`, a Node server with deps inside, :13715,
`view.IslandServer(url)`), skipped in dev, on live-socket re-renders, and when it's down; the browser then
hydrates (`data-ssr`). nexus-vite-plugin's `islands` option (default `src/islands`) builds a `nexus-islands`
entry (loader → one lazy chunk per island; an index-less project needs no entry), flags `islands` in the
hot file, and checks declared names vs files (dev warn / build error). The Go side loads islands from Vite
in dev and from the manifest otherwise (`vitemanifest.IslandsFile`/`IslandPrefix`; `EntryKey` skips the
loader; a name missing from the build → `data-error`). Live re-renders update a mounted island's props (no
remount); `view.Link`/shards unmount islands that leave. No loader (a Go test) → children stay +
`data-error`. `/_view/import.js` (a module script `view.Script` loads first) gives the classic runtime
`import()`. `_setup.ts` default export: Vue app hook / React wrapper.
**Testing**: `view/viewtest` — `p := viewtest.Mount[*T](t, app, viewtest.As(&auth.Identity{ID: "7"}))` (live page found by its
`view.LiveTag` route tag; `viewtest.At(path)` for params) or `viewtest.Get(t, app, path)`; the real runtime.js +
twins run in goja on a small Go/JS DOM (`view/internal/browser`, x/net/html parsing, virtual timers) against an
httptest server and the real socket. `Fill/Select/Check/Click/Submit/Press` (settle first), `Wait`,
`Text/Attr/Value/Exists`, `Expect(loc).Text/Value/Enabled/Disabled/Visible/Absent/…` (retrying). Locator = field
name, else CSS selector. No layout/CSS, no islands, no third-party scripts — for those,
`viewtest.Browser(t, app, path, opts…)` opens the page in headless Chrome over CDP (own small client, no
chromedp): `Click` (real mouse at the element's box), `Fill`, `Text`, `Box`, `Visible`, `Eval`, `Viewport`,
`Screenshot`, `ExpectURL`, `Expect(loc).Text/ContainsText/Visible/Hidden`; skips without Chrome
(`NEXUS_CHROME`). `viewtest.As(&auth.Identity{…})` = authtest's test credential.
**Component kit (`view/ui`)**: Button, Field/Input/Select/Textarea/Checkbox (errors from `view.Errors`), Tabs,
Dialog (server-owned via `OnClose`, or browser-side by `ID` + `ui.OpenDialog`), Dropdown/RowActions menus,
DataTable/TableRow (server paging/search/sort: `OnSearch` = `view.Change` with `form:"q"`/`"size"`, `OnSort`/
`OnPage` funcs returning `view.Send`; row `Href` click-navigates; `Actions` menu), Toast (each `ID` once),
Loader, PageHeader, Badge, Icon — templ + Tailwind utilities over `--ui-*` tokens (fall back to shadcn tokens;
light/dark). `@ui.Script()` after `@view.Script()` loads `/_view/ui/ui.{css,js}` (served on import). ui.js
(attributes + delegation, never edits runtime.js): `ui.Loading(view.Send(x.Run), "preview")` covers ids until
the live root drops `aria-busy` (attr form `ui.LoadingAttr`), `data-ui-hotkey`, `data-ui-copy`, menus,
row clicks, `nxui.toast`. Dialogs and tabs are JS commands: a browser-side Dialog carries `data-cancel`
(Hide + PopFocus) its X/`data-ui-close`/Esc/backdrop Exec; `data-ui-open`/`nxui.dialog.open` run PushFocus +
Show + FocusFirst (close buttons `data-nx-nofocus`); a Tab's onclick sets aria-selected across the strip
(`view.Within("[data-ui-tabs]")`), switches Panels, then Execs its OnSelect (`data-ui-select`) — the choice
survives re-renders. viewtest runs ui.js too (inert MutationObserver). `nexus add ui <component|all> [--dir ui] [--package p]` vendors a component.
`nexus docs views`, docs/guide/views.md, example `view/example` (+ `web/`; `/registry` uses the kit).

## 2. App entry & config (`nexus.toml`)

`nexus.Boot(opts...)` loads `nexus.toml` automatically (runtime config +
`[extensions.*]` + the `[env]` bridge + the `config.Get` value store). Edit settings in
the file, not in code; absent keys fall back to framework defaults. The explicit form
(`config.MustLoad()` + `nexus.MustLoadExtensions()` → `nexus.Run`) still works for
apps that build `config.Runtime` in Go.

**All runtime keys live under `[runtime]`** (or a `[runtime.<sub>]` table).
`[databases.*]` and `[extensions.*]` are top-level.
Runtime config lives in package `config` (`github.com/paulmanoni/nexus/v2/config`):
`config.Runtime` and its sub-structs, `config.Load`/`MustLoad`/`Read`, `config.Get`.
Any value is readable via `config.Get[T]("section.key")` — the dotted key mirrors the
TOML table path.

**nexus.toml is strict** (dev and production alike). Every table is declared by its
owner: package `config` ([runtime], [databases], [env], [extensions], [decorators], the
deploy-manifest tables), the framework extension the app imports ([cache] ←
extension/cache, [storage], [mail], [jobs]), an extension decoder
(`RegisterExtensionDecoder` → [extensions.<name>]; `config.DeclareExtension` adds its
keys), or the app. Boot fails with file:line + a did-you-mean (bootui block, `*config.Error`
with `Problems`) on a top-level key (`environment` → `[runtime] environment`), an
unknown key or sub-table of a declared table, an undeclared section, an
`[extensions.x]` whose package isn't imported. **App sections:**
`var Shop = config.Section[ShopConfig]("shop", ShopConfig{…defaults})` — package-level,
decodes `[shop]` over the default (`toml` tags; `time.Duration` reads "30s"), strict on
T's keys; `Shop.Get()`, `Shop.Present()`; T may be `map[string]X` for `[x.<name>]`
tables or `map[string]any` for a free-form table. One owner per name (a duplicate
panics at init). `config.Get` still reads declared sections (with ENV override).
`nexus config check [path]` runs the boot rules in CI (reads the app's `config.Section`
/ `RegisterExtensionDecoder` calls from source; `--json`); `nexus lint` reports the same
as errors. `nexus config schema [--framework] [-o f]` prints the JSON schema generated
from the declared types (published: `#:schema
https://paulmanoni.github.io/nexus/nexus.toml.schema.json`, regenerate
docs/public/nexus.toml.schema.json when config types change — a drift test checks).
`nexus migrate v2` moves misplaced keys it recognises into their tables.

```toml
[runtime]
environment    = "development"          # development | staging | production; NEXUS_ENVIRONMENT overrides
introspection  = true                   # opens /__nexus (OFF by default → 404s)
introspection_networks = ["10.0.0.0/8"] # allowed even when introspection is off
trace_capacity = 1000                    # request-trace ring buffer (0 = off)
sdk            = true                    # one switch: generate+serve the typed client SDK
# dotenv       = [".env", "!secrets.env"] # loaded before ${VAR}s expand (default [".env"];
                                          # ! = required; real env vars win)

[runtime.server]
addr = ":8080"
route_prefix = ""                        # prepended to every REST/GraphQL/WS route
# strip_trailing_slash = true            # "/users/" routes as "/users" (internal
                                          # rewrite, no redirect; off by default)
# idle_timeout   = "120s"                 # keep-alive cap (default 120s; "-1s" = Go's)
# read_timeout   = "0s"                   # OFF by default (would cut large uploads)
# write_timeout  = "0s"                   # OFF by default (would cut SSE/downloads)
# max_body_bytes = 104857600              # default 32MB; -1 turns the cap off.
                                          # Over-limit → 413. One endpoint moves
                                          # its own with nexus.MaxBody(n);
                                          # nexus.Timeout(d) bounds one endpoint.
# shutdown_timeout = "10s"               # graceful-drain window on SIGINT/SIGTERM.
                                          # Default 10s in prod, 250ms under nexus dev.
                                          # In-flight request contexts are cancelled when
                                          # the window closes, so handlers that select on
                                          # ctx let shutdown finish early.

[runtime.server.listeners.admin]         # optional multi-scope listeners
addr  = "127.0.0.1:7000"
scope = "admin"                          # public | internal | admin

[runtime.telemetry]                      # trace export to an OpenTelemetry collector
# otlp_endpoint = "http://localhost:4318" # OTLP/HTTP JSON → /v1/traces; unset = off
# service_name  = "orders"                # default: the dashboard name
# [runtime.telemetry.otlp_headers]       # e.g. authorization = "Bearer ${OTLP_TOKEN}"

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
# tune them or enable the opt-ins. CSRF follows what the app uses: it turns on
# by itself with cookie sessions, a cookie auth scheme or Inertia (each calls
# App.RequireCSRF), and stays off for a token-only API. `csrf` forces it.
[runtime.middleware.security]
# headers        = false                 # turn the default headers off
# frame_options  = "SAMEORIGIN"          # "-" omits X-Frame-Options
# referrer_policy = "no-referrer"
csp            = "default-src 'self'"     # opt-in Content-Security-Policy
hsts_max_age   = 31536000                 # opt-in HSTS (seconds; needs https)
# csrf         = false                    # force CSRF off (or true: force on)

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

# Config server (optional) — decoded by MustLoadExtensions; values via config.Get.
[extensions.config]
endpoint = "http://localhost:8078"
identity = "myapp"
profile  = "default"

# Env bridge — TOP LEVEL. [env.*] tables become process environment
# variables AND are exposed to the frontend. The table path after `env.` is
# the variable name, so this sets env vars "client.id" and "client.url":
[env.client]
id  = "myapp-web"
url = "${PUBLIC_URL}"              # ${ENV} expanded at load
```
**`[env.*]` bridge:** every key under `[env]` is published (a) as a process env
var read by the Go app/extensions via `os.Getenv("client.id")`, and (b) to the
frontend whenever `nexus dev`/`nexus build` start Vite (passed as
`NEXUS_FRONTEND_ENV`): nexus-vite-plugin replaces each exact member reference
`import.meta.env.client.id` in frontend source with its value, in dev and build. Only
referenced keys reach the browser — the values are never added to the
`import.meta.env` object, so whole-object access and the bracket form
`import.meta.env["client.id"]` see none of them. Nested tables flatten with dots
(`[env.a.b] c` → `a.b.c`). A `${VAR}` that is unset where `nexus build` runs drops
that key with a warning. SECURITY: `[env]` is still the Go app's process
environment too — a value the frontend references ships in the bundle, so reference
only client-public data (an OAuth client id, a public URL), never a server secret.

`nexus docs nexustoml` documents every key and the strictness rules. You can also pass `config.Runtime{...}`
inline to `nexus.Run` instead of the file.

**Introspection gate:** the entire `/__nexus` surface (dashboard + JSON APIs) is
**off by default and 404s** so production binaries are locked down. Set
`introspection = true` (or `config.Runtime.Introspection`) for dev; in production prefer an
admin CIDR allowlist (`introspection_networks = ["10.0.0.0/8"]`). `nexus dev` runs
with it open.

### HTTP router backend (pluggable; stdlib by default)

nexus is **router-agnostic** behind the `github.com/paulmanoni/nexus/v2/httpx` seam.
Handlers and middleware see an `*httpx.Ctx` (a transport-neutral request handle);
the concrete router is an adapter chosen at boot. **The default is the stdlib
`net/http.ServeMux` (`httpx/stdrouter`) — zero third-party router deps, so the
default binary links no gin/sonic/etc.** gin and chi are opt-in:

```go
import (
    "github.com/paulmanoni/nexus/v2"
    "github.com/paulmanoni/nexus/httpx/ginrouter/v2" // or .../httpx/chirouter
)

nexus.Boot(nexus.WithRouter(ginrouter.New()))     // one line; or config.Runtime.Router
nexus.Run(cfg, nexus.WithRouter(chirouter.New()))
```

`nexus.WithRouter(...)` (equivalently `config.Runtime.Router`) is the only switch — no
nexus.toml key (an adapter must be imported to link anyway). Selecting gin/chi pulls
their dependency trees back in; the stdlib default does not. Route strings use the
canonical `:id` / `*rest` syntax on every backend (chi/std adapters translate).

**`ginrouter` is a SEPARATE module** so gin stays out of the main module's
dependency graph entirely — `go get github.com/paulmanoni/nexus/httpx/ginrouter/v2`
to use it (the import path is unchanged; it just versions independently). `stdrouter`
and `chirouter` ship inside the main module (chi has no transitive deps).

Chain execution (the `c.Next()` / `c.Abort()` flow, recovery, error accumulation)
lives in `httpx.Ctx`, not the router — so every middleware runs identically on any
backend, and the router only matches paths + returns params. App-level middleware
(`nexus.Middleware(...)`, plus the built-in CORS/security/rate limit) wraps the whole mux
so it runs even on 404/405 (CORS preflight relies on this); per-op middleware runs inside
the matched route. `nexus.Middleware` takes `middleware.Middleware` values or DI
constructors returning one, ordered by `Stage` (`middleware.Edge` → the built-ins →
`Session` → `Auth` → `App`, the default) and then declaration order.

`App.Router() httpx.Router` exposes the live router (replaces the old
`App.Engine() *gin.Engine`). Low-level handlers that need raw HTTP take an
`*httpx.Ctx` parameter (replaces `*gin.Context`); `httpx.H` is the JSON map
shorthand (replaces `gin.H`).

### DI container backend (pluggable; built-in by default)

nexus has its **own dependency-injection container** — `github.com/paulmanoni/nexus/v2/di`,
a small zero-third-party-dependency engine — wired behind the same kind of seam as
the router. **It is the default, so the default binary links no `go.uber.org/fx`,
`dig`, `multierr`, or `atomic`.** `go.uber.org/fx` is opt-in:

```go
import (
    "github.com/paulmanoni/nexus/v2"
    "github.com/paulmanoni/nexus/di/fxcontainer/v2"
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
- **`nexus.FailBoot(err)`** surfaces an option-build error at boot (replaces the old
  `nexus.Raw(fx.Error(err))` pattern). `nexus.Raw(di.Option)` remains the low-level
  escape hatch.
- **Value groups / optional deps:** internal wiring (e.g. the GraphQL field group,
  the optional default auth gate) uses `di.Annotate(fn, di.ParamTags/di.ResultTags(...))`
  with `group:"…"` / `optional:"true"` tags rather than `fx.In`/`fx.Out` marker structs,
  so both backends translate them identically. Constructors are lazy (run only when a
  result is demanded) and singletons; invokes run eagerly in registration order — same
  semantics as fx.

**`fxcontainer` is a SEPARATE module** so fx stays out of the main module's dependency
graph — `go get github.com/paulmanoni/nexus/di/fxcontainer/v2` to use it. The builtin
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
- `nexus.Provide(fns...)` — constructors into the DI graph; service→service/resource
  edges are drawn from a service constructor's params and `NexusResourceProvider`s are
  registered automatically.
- `nexus.Supply(vals...)` — ready-made values. `nexus.Setup(fns...)` — pre-serve work
  (migrations, indexes, seeds) after resources start, before listeners open.
  `nexus.Invoke(fn)` — eager construction / side effect.
- `nexus.Path("/x")` — module URL prefix (REST + GraphQL). `nexus.RoutePrefix("/x")` —
  REST-only prefix.

**Controllers (`nexus.Controller` / `nexus.Resource`).** A struct whose methods are
actions, DI-constructed once, one dashboard module (`UsersController` → `users`):
`nexus.Controller[*UsersController]("/users", auth.Required()).Provide(NewUsersController).Get("/:id",
(*UsersController).Show)`. Actions must be method expressions on the type; bare scalar
params bind from the route's path params positionally (inferred `nexus.Arg`; explicit
`Arg` wins), a trailing struct is the body. `nexus.Resource[T](prefix)` registers the
conventional actions T defines — Index `GET /`, Show `GET /:id`, Create `POST /`, Update
`PUT`+`PATCH /:id`, Destroy `DELETE /:id` — plus `.Member(verb, name, …)` /
`.Collection(…)`; nested prefixes bind every param (router builders get the full stacked
prefix). `ActionAuthorizer` (`Authorize(ctx, action string) error`) runs before each action
via a wrapper, so its error takes the action's normal path as `nexus.Forbidden` (403) unless
it already carries a code, and the refusal still `errors.Is` its original error. GraphQL actions
(`.Query/.Mutation`) keep the method-derived name and mount at `<prefix>/graphql` like any
router. `.ActionDefaults(func(method, path, action string) []nexus.RestOption)` sets the
options every REST action starts from (explicit options win; `nexus.NoActionDefaults()`
opts one action out; calls add up) — the hook a controller flavour uses for custom
actions. Inside `nexus.Module("x", nexus.Path("/x"), ctrl)` a controller (or router)
mounts under `/x`; a controller's prefix is REST-only, so its GraphQL actions serve on the
enclosing module's endpoint (or the app's `/graphql`), never `<prefix>/graphql` (plain
routers keep `<prefix>/graphql`). `.TrailingSlash()` registers each action at `/p` and `/p/`.
`inertia.Component("Admin/Page")` renders any action as that page (built on
`nexus.RestOptions`, which bundles REST options). **Decorator form:** `//nexus:controller <prefix>
[trailing-slash]` on a type (+ type-level `//nexus:auth`/`//nexus:session`/`//nexus:use` shared by every
action) makes its annotated methods one `nexus.Controller[*T]` chain; methods take
`//nexus:page METHOD PATH [Component]` (component defaults to `<Folder>/<Method>`; `//nexus:inertia.Page`
reads the same), `//nexus:rest`, `//nexus:query`, `//nexus:mutation`; paths are relative (`/` = the prefix);
an action may repeat `//nexus:page`/`//nexus:rest`; the constructor still needs `//nexus:provide`. Annotated
pointer-receiver methods on types without `//nexus:controller` are collected per type
(`nexus.ControllerActions`, paths as written) and served by the `nexus.Controller`/`Resource`
declared for that type in Go — so `nexus.Module("admin", nexus.Path("/admin"),
nexus.Resource[*T]("/").Provide(NewT))` takes the annotated routes, with path and gates in
code; undeclared, they register on their own under the package's module.
`inertia.AsPage()` (built on `nexus.ActionOption`) is the Go form of a component-less `//nexus:page`.
Every annotation has a Go equivalent (table in docs/guide/controllers.md).
Directives are Go directive comments: gofmt leaves `//nexus:x` unspaced and moves it
below the doc prose (the scanner reads every line, so placement is free); go/doc hides it.
The v1 `//@x` / `// @x` spelling is a file:line error — run `nexus migrate v2`.

**Named routes & URLs.** Every REST route has a name — its handler's, first letter
lowered (`(*Users).Show` → `show`; controller actions: the method), in its module's
namespace (`users:show`) or the router chain's, stacked by `Include`
(`v1:billing:show`). `nexus.Name("x")` overrides (`Name("")` = the namespace itself; two
explicit alike fail boot; default names clashing on different paths go ambiguous);
`nexus.DefaultName` is an extension's default (view.Page → component, view.Live → its
router, auth → `login`/`me`/…); `nexus.NoName()` for plumbing. `nexus.URL(ctx, name,
params…)` / `nexus.Reverse` build the path in the app serving ctx (any transport; else
the one running app) with `Path`/prefixes/`route_prefix` applied and ids maskid-masked.
Handles: `AsRest`, `inertia.Page`, `view.Page` return `*nexus.Route` (an Option) with
`URL(ctx, params…)`/`Reverse`/`Method()`; `view.Live[*T]` → `page.URL(ctx, …)`;
`ctrl.URL(ctx, (*C).Show, 7)`; `router.URL(ctx, "rel", …)`. Params: scalars fill path
params in order, `nexus.P{"id": 7}` by name, a struct's `path:`/`query:` fields,
`nexus.Query{…}`. Failures panic under nexus dev/tests (did-you-mean), log + `"#"` in
prod. A page linking to its own route (form posting back) is a Go init cycle on the
handle's package var — declare it, assign it in `init()`. `nexus.WithApp(ctx, app)` picks
the app outside a request. `App.Routes()`, `nexus routes --name`, `GET /__nexus/routes`.
`nexus docs urls`.

**Inertia resources (`inertia.Resource[T](prefix)`).** A controller whose actions are
pages and forms: Index `GET /`, New `GET /new`, Show `GET /:id`, Edit `GET /:id/edit`
render `<Folder>/<Method>` (folder from the type, `ArticlesController` → `Articles`;
`ResourceAs[T]("Admin/Articles", prefix)` names it); Create `POST /` → 303 to the new
record's Show (its `ID`/`json:"id"` field, maskid-masked), Update `PUT`+`PATCH /:id` → 303 to
Show, Destroy `DELETE /:id` → 303 to Index (fallbacks when those are missing). Custom
`Member`/`Collection`/verb actions follow suit: GET → page `<Folder>/<Method>`, other verbs
→ 303 back (Referer, else one segment up); `nexus.NoActionDefaults()` keeps one JSON.
`nexus.Invalid()` from a write flashes + goes back. Write routes are tagged
`registry.PageActionTag` and leave the REST SDK; the frontend sends them with
`pageAction('Articles/Update', { id })` from `nexus-client/pages`, which returns `[method,
url]` (spread into `form.submit(...)`, or `router.visit(url, { method })`), typed by
`NexusPageActions`. Handlers returning nothing render through the renderer's
`EmptyRenderer`; Inertia props may be a string-keyed map as well as a struct. The CSRF
middleware mirrors its token into an `XSRF-TOKEN` cookie and accepts `X-XSRF-TOKEN`
(axios's convention), so Inertia forms pass with `csrf = true`.

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

Every transport uses the same shape — a method on a service is the documented
default:
```go
func (s *UserService) CreateUser(ctx context.Context, in CreateUser) (*User, error)
nexus.AsMutation((*UserService).CreateUser)        // op "createUser"
```
A free function works too, with DI deps as parameters and `nexus.Params[T]` last
when it needs more than ctx + args:
```go
func ListPets(svc *PetService, db *DB, p nexus.Params[ListArgs]) ([]Pet, error)
```
- The receiver (or first `*Service`-wrapper dep) grounds the op under that service
  (pin with `nexus.OnService[*XService]()`).
- `nexus.Params[T]` exposes `.Context` and `.Args`.
- Return `(T, error)` — `T` is the GraphQL type / REST JSON body. A raw handler takes
  `*httpx.Ctx` (and its deps) and writes the response itself; `AsRest` also takes a
  factory — `func(deps…) httpx.HandlerFunc`, built once at boot (v1's `AsRestHandler`).
- The op name is the method or function name, first letter lowered (`ListPets` →
  `listPets`), or `nexus.Op("…")`. v1 dropped a `New` prefix; `nexus migrate v2`
  adds `nexus.Op` to v1 `NewXxx` registrations so wire names stay put.
- Struct tags drive schema + validation: `graphql:"title,required" validate:"required,len=3|120"`, `path:"id"` for REST path params (also `query:"x"`, `header:"X"`, `form:"x"`, `json:"x"`).
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
derives from the method. **Body mode:** a trailing struct is the body and the names map
onto the scalars before it (`Update(ctx, id int64, in UserInput)` + `Arg("id")`); its
exported fields merge into the synthesized args (unexported body types fine), and on REST a
name that is a route segment binds from the path only (`json:"-"`), so a body can't
override it. `validate:` tags are enforced on every transport. One or two scalars ride `Arg` well — three or more
deserve a dto. A struct-taking param is rejected with "register it directly".

**Errors (`nexus.Error`) — one model, one table per transport.** Return
`nexus.Err(code, msg)` / `nexus.Errf(code, fmt, …)` (a `%w` sets Cause) /
`nexus.Invalid().Field(name, msg).Global(msg)` / a bare code (`return nil, nexus.NotFound`).
`nexus.Error{Code, Message, Fields, Cause}`; codes `InvalidInput` 422, `Unauthenticated` 401,
`Forbidden` 403, `NotFound` 404, `Conflict` 409, `TooMany` 429, `Unavailable` 503, `Internal`
500 (GraphQL `extensions.code` `INVALID_INPUT`, `UNAUTHENTICATED`, …, + `errors` field map).
REST body `{"code","message","errors"}`; WS `error` event `{type,code,message,errors}`;
Inertia: InvalidInput flashes + 303 back, other errors → `ErrorPage` at the code's status;
views: `view.Errors(ctx)`. An error without a code is `Internal`, its message shown under
`nexus dev` and "internal error" otherwise. `errors.Is(err, nexus.NotFound)` matches a code;
`nexus.ErrorOf`/`CodeOf` map any error; `nexus.WriteError(c, err)` for raw handlers.
`validate:` tags (`required`, `len=a|b`, `int=a|b`, `oneof=a|b`) run on every transport after
binding → InvalidInput with per-field messages; auth gates/rate limits answer through the
same table (`middleware.ErrorBody`). The boot-time option is `nexus.FailBoot(err)`.

**Raw form input (`*nexus.Form`) + validation errors (`nexus.Invalid()`).** For
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
    errs := nexus.Invalid()
    if taken { errs.Field("email", "already taken") }
    if down  { errs.Global("provider unreachable") }
    if errs.Any() { return nil, errs }
```
An InvalidInput renders per transport: Inertia pages flash + 303 back (`errors`
prop, `useForm` convention, global under `errors._global`, error bags honored);
REST answers 422 `{"code", "message", "errors": {field: [msgs]}}`; GraphQL carries the
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
envelope is the contract. The wrap receives the mapped `*nexus.Error` (an uncoded
error's message hidden outside dev). An error the wrap converts becomes a normal 200/data
response; an error the wrap *returns* follows the standard error path. Binding
and validation failures are never enveloped. REST + GraphQL.

### REST
```go
type GetArgs struct { ID string `path:"id"` }   // path param `:id` binds via the `path` tag
nexus.AsRest("GET", "/users/:id", NewGet)
```

### GraphQL (auto-mounted on `/graphql`)
```go
nexus.AsQuery((*UserService).SearchUsers)
nexus.AsMutation((*OrderService).CreateOrder, auth.Required(), auth.Requires("ROLE_CREATE"))
```
Field name = the method or function name, first letter lowercased
(`SearchUsers` → `searchUsers`). Fields are partitioned by service; service-less
handlers mount on a default partition.

**The engine is behind a seam.** The schema is always derived from handlers (no
hand-built schema is mounted), and no graphql-go type appears in the public API — the
engine lives in `internal/graph` + `internal/gqlhttp`. What handlers and middleware see is
`github.com/paulmanoni/nexus/v2/gql`: `Params[T].Info` is a `gql.Info` (`FieldName`,
`ParentType`, `Operation`, `OperationName`); a GraphQL-only middleware is a
`gql.Middleware` (`func(next gql.Resolver) gql.Resolver` over a `gql.Field{Context, Args,
Source, Info}` — change Context/Args/Source before `next(f)`), attached with
`nexus.GraphMiddleware(name, desc, mw)` or as `middleware.Middleware.Graph`. Named input
types come from Go types: `nexus.RegisterGqlType("Status", Status("a"), Status("b"))`
(enum) / `nexus.RegisterGqlType[Address]("ShippingAddress")` (input object), referenced
by `graphql:"field,type=Status"`. Custom arg checks return `nexus.Invalid()` from the
handler (no GraphQL-only validators). `internal/apicheck` fails `go test` if an exported
identifier reaches a graphql-go type.

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
Built-in `ping/authenticate/subscribe/unsubscribe` are handled by the hub (and can't
be registered as handler types). **A connection's user is what the server
authenticated for the upgrade request** — `extension/auth` registers its identity via
`nexus.RegisterRequestIdentity` — so `EmitToUser` reaches the real owner; no query
param or `authenticate` message can claim an id. **Rooms are joined server-side**
(`sess.JoinRoom` after checking the caller); a client's own `subscribe` is refused
unless the path opts in with `nexus.ClientRooms(func(userID, room string) bool)`.
**Handlers see the connection's auth**: `p.Context`/`sess.Context()` carry the
identity and auth state from the upgrade request (captured once per connection), so
`auth.Current`, `auth.User[T]` and `auth.Can` work in WS handlers. Extensions add
values with `nexus.RegisterWSCarrier` — only ones that may outlive the request (never a
`Scoped` memo or a session handle).

### Decorator-form registration (`//nexus:` annotations) — optional

Instead of listing every handler in a `nexus.Module(...)`, annotate the handler
functions with `//nexus:` doc comments and let codegen do the wiring. **Purely
additive** — it produces the same options as `AsRest`/`AsQuery`/`Provide`, so
annotated and hand-written registrations coexist. Needs the `decorate` package
(`github.com/paulmanoni/nexus/v2/decorate`); the codegen scanner lives in the
`nexus` CLI only, so the app binary links no extra deps.

```go
//nexus:provide
func NewUserService(app *nexus.App) *UserService { ... }

//nexus:rest GET /users/:id
func NewGetUser(s *UserService, p nexus.Params[GetArgs]) (*User, error) { ... }

//nexus:mutation
//nexus:auth Requires ADMIN
func NewCreateUser(s *UserService, p nexus.Params[NewUser]) (*User, error) { ... }
```

Annotation catalog (one PRIMARY per func, plus optional modifiers):
```
//nexus:provide              //nexus:rest <METHOD> <PATH>     //nexus:query / //nexus:mutation
//nexus:subscription         //nexus:ws <PATH> <TYPE>         //nexus:worker <NAME>
//nexus:auth Required | Requires PERM… | Public  (modifier)
//nexus:use <expr>           (modifier, per-op middleware)
//nexus:session Required     (modifier — session.Required(), the 428 flow-continuity gate)
//nexus:router [<name>] <prefix> [parent=<n>] [auth=…] + //nexus:on <name>
                             (FastAPI-style routers: name defaults to the package (ops
                             auto-join); stacked prefixes, shared gates, cross-package
                             membership; Go API: nexus.NewRouter/Include — pass the root to Boot)
//nexus:module <name> | //nexus:path <prefix> | //nexus:routeprefix <prefix>
                             (PACKAGE doc comment — name the module group / prefix its routes)
//nexus:page <METHOD> <PATH> [Component]  inertia page (component optional on a controller method)
//nexus:job [queue] [timeout=D] [retry=N] [unique=D] [name=X]  background job (extension/jobs)
//nexus:controller <prefix> [trailing-slash]  (TYPE doc comment — its annotated methods
                             become one nexus.Controller chain; see Controllers above)
//nexus:<pkg>.<Func> args…   custom extension decorator — emits pkg.Func(args…, fn); the
                             registrar returns a nexus.Option (e.g. inertia.Page, reusing its
                             existing signature). pkg is imported from the annotated file.
                             Args are whitespace-separated; no spaces inside one.
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
  In a test binary linking several annotated packages, scope each InProcess
  boot with `nexus.DecoratedModules("<pkg>")` so one module's test is isolated
  from the others' registrations (the drain is a snapshot — repeated boots in
  one binary all see them; no names = drop all decorated registrations).
  (`nexus build` fails fast if codegen can't resolve a decorator; `nexus dev`
  warns and lets `go run` surface the underlying error.)
- A qualified custom decorator (`//nexus:pkg.Func`, e.g. `//nexus:inertia.Page`) needs the
  `pkg` import resolved for the generated file. The codegen resolves it
  automatically: from the annotated file's imports → its sibling files in the
  same package → a `nexus.toml` `[decorators.imports]` hint → the module import
  graph (`go list`), where the main module's own packages outrank dependency
  packages sharing the name. Package selectors inside `//nexus:use` expressions
  resolve through the same cascade. So you usually need no import in the
  annotated file; if a selector is ambiguous (inside the module, or between
  foreign packages with no local candidate) or not a dependency, add
  `[decorators.imports]` (selector → import path) to `nexus.toml`, or import the
  package. A blank import (`_ "…/pkg"`) in the annotated file also works as an
  explicit opt-in.

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
    "github.com/paulmanoni/nexus/v2"
    "github.com/paulmanoni/nexus/v2/db"
    "github.com/paulmanoni/nexus/v2/extension/cache"
    _ "github.com/paulmanoni/nexus/extension/cache/redis/v2" // opt into Redis (omit → memory-only)
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
_ "github.com/paulmanoni/nexus/v2/db/postgres" // pgx
_ "github.com/paulmanoni/nexus/v2/db/mysql"
_ "github.com/paulmanoni/nexus/v2/db/sqlite"   // pure-Go engine (~5MB) — don't ship it unused
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
- **Per-cache store — `driver`** (`cache.Config.Driver`, `[cache.<name>] driver`, env
  `CACHE_DRIVER` / `CACHE_<NAME>_DRIVER`): `""`/`"auto"` = Redis in production when the
  backend is linked (the default); `"memory"` = never Redis (opt one cache out while others
  keep it); `"redis"` = Redis in every environment. An unknown driver, or `redis` without the
  backend import, fails `cache.Bind` at boot; the dashboard shows the effective driver.
- Cache-backed metrics (multi-replica counters) are opt-in via
  `config.Runtime.Stores.Metrics = cache.NewMetricsStore(mgr)`; the default is an in-process
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
import "github.com/paulmanoni/nexus/v2/extension/storage"

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
import "github.com/paulmanoni/nexus/v2/extension/mail"

type Mailer struct{ *mail.Manager }

mail.Bind[Mailer]("smtp", func() mail.Config {
    return mail.Config{
        Driver: "smtp", Host: config.Get[string]("mail.host"),
        Port: config.Get[int]("mail.port", 587),
        Username: config.Get[string]("mail.username"),
        Password: config.Get[string]("mail.password"),   // from env/nexus.toml
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
import "github.com/paulmanoni/nexus/v2/extension/session"

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
restarts; `session.CacheStore(resource.Cache)` rides extension/cache (Redis =
restart-safe + multi-replica); or implement `Store` over your DB. Cookie is
always HttpOnly, SameSite defaults Lax — set `Secure: true` behind TLS.
`nexus docs session`.

### Opaque IDs (`extension/maskid`)
Replaces sequential integer IDs with 22-char opaque strings on the wire and
converts them back before the handler runs — handlers, GORM models and SQL keep
using `int64` keys. One option, no app-code change:
```go
import "github.com/paulmanoni/nexus/v2/extension/maskid"

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

### Config values (`config.Get`)
`config.Get[T]("key", default...)` reads from, highest priority first: (1) an ENV
override (`db.port` → `DB_PORT`), (2) the `[extensions.config]` snapshot when wired
(hot-reloadable, remote-capable), (3) the **`nexus.toml` base layer** seeded by
`Boot`/`config.MustLoad`. Layers resolve per-key, so a key absent from a higher layer
falls through. Read anywhere:
```go
addr := config.Get[string]("runtime.server.addr")     // straight from nexus.toml
port := config.Get[int]("db.port", 5432)              // 2nd arg = default
ttl  := config.Get[time.Duration]("cache.ttl", 5*time.Minute)
```
The dotted key mirrors the TOML table path — `[runtime.storage] url` →
`config.Get[string]("runtime.storage.url")` — **no extension needed** for plain
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

**Background jobs (`extension/jobs`).** Queued, retried, cancellable work with progress.
A job is `func (s *Svc) M(ctx context.Context, run *jobs.Run, a Args) error` (receiver from
DI, args JSON): `var X = jobs.Define((*Svc).M, jobs.Queue("low"), jobs.Timeout(d),
jobs.Retry(n), jobs.Backoff(fn), jobs.Unique(ttl), jobs.Name("…"))` — the handle is a
`nexus.Option` (pass to Boot with `jobs.Module(jobs.Config{})`) and `X.Enqueue(ctx, args,
jobs.Delay(d)|jobs.At(t))`; `jobs.DefineFunc` for plain funcs (closures need `jobs.Name`).
Decorator: `//nexus:job [queue] [timeout=2h] [retry=3] [unique=10m] [name=x]` on a method or func;
enqueue annotated jobs with `jobs.Enqueue(ctx, (*Svc).M, args)` (ambiguous if a method is
defined twice). `*jobs.Run`: `Progress(done,total,msg)` (returns ctx.Err() once it should
stop), `SetResult`, `Checkpoint`/`Resume`, `ID/Attempt/Actor` (enqueuer via
`nexus.RequestIdentity`). `jobs.Permanent(err)` skips retries; panics fail with the stack.
Inject `*jobs.Manager` for `Get(ctx, id)` / `Cancel(ctx, id)` / `List(ctx, jobs.Filter{…})`.
`X.Schedule("0 7 * * *" | "@every 15m" | "CRON_TZ=… …", args)` is an Option (registers the job
too) enqueuing one job per tick across replicas (deterministic tick ID; missed ticks skipped).
Drivers behind `jobs.Store`: `memory` (default; in-process, carried across `nexus dev`
rebuilds), `db` = `jobsdb.Bind[DB]()` (`extension/jobs/jobsdb`, GORM, any dialect; tables
`nexus_jobs`/`nexus_job_uniques` auto-created, `jobsdb.NoMigrate()`/`Migrate`; version-checked
CAS writes), `redis` = `jobsredis.Bind(jobsredis.Config{URL, Prefix})` (SEPARATE module
`extension/jobs/jobsredis`; `[jobs.redis] url`, else `REDIS_URL`; Lua claim/insert, WATCH/MULTI
updates, `{nexus:jobs}:` keys), `rabbitmq` = `jobsamqp.Bind(jobsamqp.Config{URL, Prefix,
ConsumerTimeout, DeliveryLimit})` (SEPARATE module `extension/jobs/jobsamqp`; a `jobs.Broker`,
not a Store: persistent messages in quorum queues `nexus.jobs.<q>` + `.failed` + `.delay.<ms>` TTL
queues; confirms; ack after the job; `x-consumer-timeout` default 8h (RabbitMQ 3.12+, keep above
the longest Timeout; changing args needs the queue deleted); no per-job state → Get/Cancel/List
return `jobs.ErrUnsupported`, Unique refused at boot, schedules should run on one replica;
checkpoints travel in the message). A `jobs.Store` or `jobs.Broker` in DI (optional deps of
`jobs.Module`; both is a boot error) selects its driver. Shared stores: lease per claim (`[jobs] lease` 30s, renewed every lease/3), takeover of a
dead worker's jobs (a crash spends an attempt), ownership-checked writes (`jobs.ErrLostOwnership`),
cross-process cancel via the lease heartbeat, `poll` (1s) for idle workers. `[jobs]` `run`
(false = enqueue only), `shutdown_grace` (10s, 0 in dev; then cancel + requeue),
`[jobs.queues] name = workers`. At-least-once delivery. Dashboard: a `jobs` queue node with the
driver, per-queue counts and the latest failure. Each app binds a job's receiver separately, so
two apps in one process don't share services.

---

## 8. Auth (`extension/auth`)

One module; the app writes one interface. Design + history: docs/design/v2-auth.md; guide:
docs/guide/auth.md; `nexus docs auth`.
```go
nexus.Boot(auth.Module(auth.Config{Users: auth.UseUsers(NewUsers)}), …)   // Users is the one required setting

type Users struct{ db *DB }   // implements auth.Users, checked at boot (missing method named)
func (u *Users) FindLogin(ctx, login string) (*auth.Identity, string /*encoded pw*/, error)
func (u *Users) Load(ctx, id string) (*auth.Identity, error)
// optional: SetPassword(ctx, id, encoded) error · CheckLogin(ctx, *Identity) error · Public(*Identity) any
```
- **Identity** `{ID, Kind, Perms, User, Actor, Scheme}` — Perms match with wildcards (`orders.*`, `*`);
  Kind ("staff") gates areas/`auth.Kind`; User is the app's user (`auth.User[T]`); set Kind in FindLogin.
  Read with `auth.Current(ctx)` (nil = anonymous), `auth.ID[T](ctx)`, `auth.User[T](ctx)`, or as a
  handler param: `*auth.Identity` (nil = anonymous) / `auth.Identity` (401 without a sign-in) —
  built on `nexus.RequestParam[T](fill)`.
- **Schemes** `[auth.schemes.<name>]`, tried apikey → jwt → bearer → session (default: one session
  "web"): `session` (extension/session; turns CSRF on), `bearer` (opaque 256-bit tokens stored as
  SHA-256; `ttl`, `refresh = "720h"` — Go durations, never "30d"), `apikey` (`header`), `jwt` (verify
  tokens issued elsewhere: `secret` HS256 | `public_key` PEM RS256/ES256 | `jwks` URL; issuer, audience,
  subject, leeway; alg pinned to the key; shares Authorization with bearer by token shape). A failing
  credential = anonymous, reason in the 401 under nexus dev + trace. `Users.Load` cached per id
  (`[auth] cache`, default 5m, negative = off). `Config.Tokens` (memory default, warned in prod + `nexus doctor` —
  `authdb.Bind[DB]()` from `extension/auth/authdb` (table `nexus_auth_tokens`) or `auth.CacheTokens(cache)`
  in prod; a DI `auth.TokenStore` is picked up), `Config.Throttle` (`auth.CacheThrottle`), `Config.Clients`, `Config.Settings` ([auth] in Go).
- **Sign-in**: `auth.Login(ctx, auth.Password{Login, Password})` (throttle `[auth.throttle]` → FindLogin →
  `[auth.passwords]` hashers, rehash when stale → area kinds → CheckLogin; wrong pw = unknown login =
  422 "invalid login or password") → `auth.SignIn(ctx, id[, auth.Using("api"), auth.ReturnTo(next)])`
  (session cycled + CSRF rotated, or tokens returned once as `Credential{access_token, refresh_token,
  expires_in, next}`); `auth.SignOut`, `auth.SetPassword(ctx, id, plain)`, `auth.RefreshToken`,
  `auth.Refresh(ctx, uid)` (drop Load cache).
- **Gates** (every transport): deny-by-default — every endpoint/page/view needs a sign-in unless
  `auth.Public()` (`[auth] default = "public"` opts out); `auth.Required()`, `auth.Requires(p…)` (403),
  `auth.RequiresAny(p…)`, `auth.Kind(k…)`; directives `//nexus:auth Public|Required|Requires p…|RequiresAny
  p…|Kind k…`. UI: `auth.Can`, `auth.Gates`, `auth.OpGates(ctx, app)` (map[opName]bool from the
  registrations; false for ops the visitor can't call). Per-object: `auth.Policy[T](rule)` (an Option) +
  `auth.Check(ctx, perm, obj)` / `auth.Allowed`.
- **Areas** `[auth.areas.<n>]` prefix, kinds, login, home, forbidden: kind-gated; page visit without a
  sign-in → area login `?next=` (302 / 409 + X-Inertia-Location); refused page visit → forbidden path;
  `[auth] login / home / forbidden / next_param` outside areas. `next` validated (no //host, \, scheme,
  control chars, through decoding rounds); `auth.Next(ctx)`. `session = "<scheme>"` on an area: that
  session scheme holds the area's sign-in alone (stored token in its own cookie, Path = prefix; SignIn under
  the area uses it; idle doesn't apply) — else areas share the app session.
- **Endpoints** `[auth.endpoints]` login / logout / me (`nx.auth.*`; me = `{user, can, actor?}`) / token
  (OAuth2 password, refresh_token, client_credentials; CSRF-exempt via `App.ExemptCSRF`) / revoke (RFC
  7009). OAuth2 clients `[auth.oauth2.clients.<id>]` (secret | secret_hash, grants, perms, kind) or
  `auth.Clients`; client_credentials → identity `client:<id>`; `[auth.oauth2] require_client`.
- **OIDC** `type = "oidc"` (issuer → discovery, client_id/secret, login + redirect paths, claim "email",
  scopes): authorization code + PKCE + state/nonce, ID token verified via JWKS, account via
  `Users.FindLogin(email)` or optional `Provision(ctx, scheme, claims)`; needs a session scheme.
  **Roles** `[auth.roles] clerk = [perms…]` + `Identity.Roles` (expanded into Perms on load).
  **Catalogue** `[auth] perms = […]` (optional): gates/roles naming an undeclared perm fail boot.
  jwt `revocable = true`: RevokeUser reaches tokens issued (iat) before it.
- **Pages**: Inertia gets an `auth` prop `{user, can}` (`[auth] page_prop`, "-" off; app props win).
- **Ending sessions**: per-user epoch in the token store — `auth.RevokeUser(ctx, uid)` (everywhere; WS and
  live-view connections close at their next message via `nexus.RegisterConnectionCheck`),
  `auth.Revoke(ctx, token)`, `auth.Sessions`/`RevokeSession`, `auth.Keys.Create/List/Revoke` (need
  `auth.TokenLister`), `[auth.sessions] single / end_on_password_change (default true) / idle`.
- **Impersonation** `auth.Impersonate(ctx, id)` / `StopImpersonating` (`.Actor` = real user;
  `[auth.impersonation] permission, endpoint`; no escalation/nesting). **Jobs** run as their enqueuer
  (`nexus.RegisterIdentityRestorer` → Users.Load); enqueued while impersonating, the record keeps the real
  user (`Record.Impersonator`, via `nexus.RequestImpersonator`).
- **Tests**: `extension/auth/authtest` — `app.With(authtest.As(&auth.Identity{…}))`, `authtest.AsUser("7")`
  (a header honoured only in test binaries), `authtest.Users` (in memory); `viewtest.As(&identity)`.
- **Tools**: `nexus auth check [nexus.toml]`; `nexus lint` warns on `[auth] default = "public"`; dashboard
  Auth tab (schemes, areas, every endpoint's gate with Public flagged, policies, throttle locks + unlock, a
  user's sessions/keys with revoke, sign out everywhere, recent 401/403s). Boot fails on a wrong-signature
  optional Users method; a 429 carries Retry-After (`nexus.Error.RetryAfter`); `jobs.AsSystem()` runs a job
  without an identity; the `auth` prop is typed (`NexusSharedProps["auth"]`).
- The v1 API (resolvers, `Backend`, extractors, `Manager`, `Endpoints`, `ErrorHandler`, `IdentityFrom`,
  `Roles`/`Scopes`/`Extra`, `extension/oauth2`, `inertia/iauth`) was removed in 2.9; `nexus migrate v2`
  renames Subject→ID, Optional→Public and flags the rest.

**Built-in web security** (headers + CSRF) is separate from identity — it's the
`[runtime.middleware.security]` block (§2) / `extension/security` plugin: safe response
headers on by default, CSRF on when the app uses cookies or forms. `nexus docs security`.

---

## 9. Dashboard (`/__nexus`)

Live introspection UI (needs introspection open — see §2). A server-rendered
**templ + templUI console** (`extension/dashboard/console*.templ`; nifi-style slate
top bar, status bar, dense tables, light/dark) with tabs: **Architecture** (`/__nexus/`
— the Vue topology canvas embedded as an island: graph grouped by module, drill-down,
collapsed at scale, bundled edges, ELK layout, minimap, live traffic pulses),
**Endpoints** (`/__nexus/ui/endpoints` — module/service rail, server-side search ·
sort · 100-row pages; detail page with input schema, recent errors, a REST/GraphQL/WS
tester and rate-limit overrides), **Services**, **Resources**, **Workers & Crons**
(run/pause/resume), **Traces** (+ waterfall per trace), **Auth** (cached identities,
invalidate, live 401/403 rejections from the trace buffer; when auth is wired), **Runtime** (global chain, plugins, middleware,
GraphQL cache). Gate it behind your own middleware via `config.Runtime.Middleware.Dashboard`.
Editing it: `make dashboard` regenerates the templ code and the embedded Tailwind CSS
(`assets/console.css`, both committed — a plain `go build` needs neither tool); the
canvas is `ui/` (`npm run build`, committed `ui/dist`). The templUI components are
vendored under `internal/ui` (no templUI module dependency).

The dashboard is **WebSocket-driven, not polled**: the console re-renders its live
regions when `/__nexus/live` pushes (a 304 when nothing on screen changed), and
`/__nexus/live` pushes one
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

**Error pages (`inertia.Config.ErrorPage`).** Without it, a page handler error — or a
failing `Defer`/`Optional` prop — falls through to the REST `{"code", "message"}` JSON and the
Inertia client shows its "invalid response" modal. With `ErrorPage: "Error"`, GET visits
render that component with `inertia.ErrorProps{Status, Message}` at the error's status
(the code's, else 500; shared props included), and form submits 303 back with
the message under `errors._global`. Redirects/validation unchanged; the error is still
traced.

**Typed page URLs (`pageUrl`).** Links come from the Go routes, keyed by component:
`import { pageUrl } from 'nexus-client/pages'` → `router.visit(pageUrl('Users/Show',
{ id, tab }))` → `/users/42?tab=…`. Path params (from the route template) are required,
the handler's `query:`/`form:`-tagged args are optional query params, anything else is a
type error; ids take `string | number` (maskid), `route_prefix` is applied. Several routes
per component: GET before POST, trailing-slash twins collapse, the most specific route
whose path params are given wins; `{ route }` forces one, `{ query }` adds undeclared
params. Generated as `web/sdk/pages.{js,d.ts}` (dev dump, `nexus client --out`, served at
`/__nexus/client/pages.js`); the Vite plugin aliases `nexus-client/pages` and the tsconfig
merge maps it. `registry.FieldSchema.Path/Query` carry each field's URL binding name.
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

**Simplest enable — one switch (`sdk = true`):** set `config.Runtime.SDK` (or `[runtime] sdk =
true` in nexus.toml) and nexus generates + serves the full typed SDK and, when a frontend
dir is present (any `vite.config.*`), dumps the SDK files into `web/sdk` + wires tsconfig
so `import 'nexus-client'` resolves with types — no `client.Config` ceremony. **The dump
runs in development only** (`nexus dev` or `environment = "development"`); a production
binary never writes files. `client.Off` on `OutDir` is an explicit "no dump". PocketBase-style. **Independent of
`introspection`** — the SDK is the app's own browser bundle's import, so it keeps serving
in a locked-down production binary; the routes are public and the manifest maps your API
surface, so vendor with `nexus client --out` instead if you don't want that published.
`introspection` governs `/__nexus`; `sdk` governs the client. For finer control (custom path, route middleware,
explicit OutDir, per-deployment gating) set `config.Runtime.Client` / `nexus.ClientUse(...)`
directly instead.

---

## 11. CLI cheatsheet

```
nexus new <dir>      Scaffold an app + nexus.toml. --frontend vue|react (a Vite project
                     under web/), --inertia [--ssr], --db, --cache, --auth,
                     --module <path>, --yes (no prompts).
nexus init [dir]     Add a Vite frontend (web/) to an existing project and patch main.go.
                     --frontend (req). --force: add the project files to an existing
                     web/, keeping index.html and src/.
nexus dev [dir]      Live dev: the app + dashboard on its own origin, and — when the
                     frontend dir has a package.json — its Vite beside it (deps
                     installed on first run; open the app URL it prints, never Vite's).
                     Go rebuilds are build-then-swap (old binary serves through the
                     compile; broken builds keep it up; no-op builds skip the restart).
                     DWARF is stripped and the frontend bundle is stubbed out of
                     the dev binary (--debug / --no-embed-stub opt back in).
                     --dist keeps web/dist rebuilt (vite build) in the background so
                     go build always embeds the current frontend. --frontend <dir>
                     overrides the detected dir. Compiled views are written
                     beside the .templ files (--no-view-files: in memory).
nexus build          install (if needed) → vite build [→ vite build --ssr] → web/dist,
                     then go build embeds it. ONE binary (frontend + Go). -o <path>.
nexus test / vet     go test / go vet with the generated code (handlers, views) overlaid;
                     arguments pass through.
nexus lsp            Language server for .go and .templ: gopls plus the generated code
                     as editor buffers (see Reactive templ views). --gopls, --log.
nexus client [--out dir]   Write the embedded JS/TS client SDK to disk.
nexus add ui <component>  Copy view/ui components (+ ui.js/ui.css) into the app to own them.
nexus generate frontend    Typed TS source tree from a manifest (--check = drift gate).
nexus generate handlers [./...]  Wire //nexus:-annotated handlers: write nexus_handlers_gen.go
                     per package + a main-package import aggregator. --check = CI drift gate.
                     (Run automatically by nexus dev/build; see §5.)
nexus docs [topic]   Inline reference. --web opens the docs site (paulmanoni.github.io/nexus).
nexus migrate v2 [dir]  Codemod a v1 project for v2: /v2 import paths (Go + templ), go.mod
                     requires at v2.0.0 (view dropped — it's in the root module), moved
                     symbols (nexus.Config → config.Runtime, nexus.Get → config.Get, …;
                     table in --help), the error model, uri: → path: tags, //@x →
                     //nexus:x annotations, misplaced nexus.toml keys, nexus.Op for v1
                     NewXxx op names; a // TODO(nexus v2) where a step needs a person.
                     gofmt'ed, idempotent. --dry-run lists every edit. Step by step:
                     docs/guide/migrating-to-v2.md.
nexus doctor         Check the project (Go vs go.mod, nexus v2, nexus.toml, Node/package
                     manager/Vite, Tailwind CLI, generated views) — each problem with its
                     fix — then audit nexus.toml's manifest. `nexus doctor <file|->` audits
                     a manifest only.
nexus release vX.Y.Z Release the repo's modules: checks (CHANGELOG section, clean tree, no
                     go.work replace outside the repo), root tag + push, submodules moved
                     onto it (GOPROXY=direct) + tagged + pushed, CLI install check. Prints
                     the plan; --yes runs it.
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
- **Frontend deps**: `nexus dev`/`nexus build` install them with the project's package
  manager (lockfile-driven, frozen) when `web/node_modules/.bin/vite` is missing. Commit
  the lockfile
  and `web/sdk`; `web/dist/*` (except the committed `index.html` stub) and
  `web/node_modules` are gitignored. Keep `typescript` on `~6.0` (vue-tsc 3.3 crashes on 7).
- **Deploy with `NEXUS_ENVIRONMENT=production`** — scaffolds ship `environment =
  "development"` in nexus.toml, which turns on dev-only behaviour (the hot file; with
  `sdk = true`, the SDK dump into `web/sdk`).
- **Handlers are methods on services** (or plain functions); an op is named after the
  method/function as written — no `New` prefix rule in v2.
- Don't reference `nexus.DeployAs` / `nexus.IfDeployment` — not implemented.
- **The CLI as a project tool**: `go get -tool github.com/paulmanoni/nexus/cmd/nexus/v2@latest`
  pins it in go.mod; run `go tool nexus dev|build|test …`. Its deps join go.mod as indirect
  requirements, never the shipped binary. A go.work pointing at a local nexus checkout must
  also `use` its `cmd/nexus` module, or `go tool nexus` builds the published CLI.
- `nexus docs <topic>` is the authoritative per-feature reference inside the installed
  binary; prefer it when unsure of an exact signature.
