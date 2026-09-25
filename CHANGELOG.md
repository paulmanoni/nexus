# Changelog

All notable changes to nexus are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Vite handshake: the plugin tells, the app reads.** `nexus-vite-plugin`
  writes `<outDir>/.vite/nexus-hot.json` — the dev server's real origin,
  base, entries and pid — once `vite dev` is listening, and removes it on
  shutdown. `ServeFrontend` and the Inertia engine read it per request, so
  pages load modules from wherever Vite actually bound (including when 5173
  is taken), a Vite restart on a new port applies with no Go restart, and
  `npm run dev` + `go run .` with `environment = "development"` is a complete
  dev setup without `nexus dev`. The file is read from disk only, never
  served, and followed only while the dev server it names is live (running
  pid, or an origin that answers); one left by a killed dev server reads as
  absent and is logged once. The
  origin is the socket's bound address, not `localhost`: two dev servers can
  hold the same port on 127.0.0.1 and ::1, and `localhost` reaches either.
  `NEXUS_VITE_DEV` remains as a fallback.
- **Inertia pages render into `index.html`.** The engine used to synthesise its
  own document, so everything an app put in `index.html` — title, meta,
  stylesheets, a loader — was missing on server-rendered pages. It now puts the
  page object on the mount element of the real document (Vite's transformed
  page in dev, the built page in production) and adds no asset tags of its own.
  A module-only build still gets a synthesised document, with tags under the
  path the bundle is actually served at (`App.FrontendMount`), not `/`.
- **Dev reload that doesn't fight HMR.** Under `nexus dev` a `.vue` save was
  applied in place by Vite and then thrown away by a full reload ~130ms later.
  The reload shim now reloads only when a new server process is serving (a
  boot ID), or when the dev server starts, stops or moves to another port —
  never for files Vite handles. SPA pages carry the shim too.
- **`public/` files in development** are proxied from the app's origin to the
  Vite dev server (loopback clients naming a loopback host, not through a
  proxy or tunnel; Vite's own routes are never forwarded), so runtime paths like
  `fetch('/config.json')` and template `<img src="/logo.png">` stay live.
- **`nexus({ input })`** declares an Inertia app's entry module once; the
  plugin also forces `build.manifest: true` and sets `server.origin` so CSS
  `url()` and asset imports resolve against the dev server when the page is
  served from the app's origin.
- **Asset caching from the build, not from path conventions.** A file is
  `immutable` only when the Vite manifest lists it as build output *and* its
  name carries a content hash; everything else is revalidated with an ETag.
  Embedded files had no modification time, so non-hashed files could never
  be answered with a 304 before.
- **`nexus.toml` keys nothing reads are reported** at boot and by
  `nexus lint`, with a hint naming the right table (`did you mean
  [runtime.server] addr?`) or listing what the table accepts.
- `db.Config.Address()` — host:port safe to log, unlike `DSN()`.
- `nexus --version`; command groups in `nexus --help`; a `--help` pointer on
  command-line errors.
- **Typed Inertia pages and shared props.** `inertia.Page` tags its route with
  the component (`registry.PageTag`), and the client SDK emits
  `NexusPageProps` (component → the handler's props type; a union when one
  component has several props types) and `NexusSharedProps` in `client.d.ts`.
  A page reads its props with `defineProps<NexusPageProps['Users/Index']>()`.
  `inertia.ShareScoped` now records its type, and `inertia.ShareTyped[T]` is
  the typed sibling of `Share`; untyped `Share` keys ride an index signature.
  Typed shares are optional (`can?:`): the engine omits a key whose compute
  fails, so the type does not promise what the page may lack.
  A generated `inertia.d.ts` (referenced from `client.d.ts`, served at
  `/__nexus/client/inertia.d.ts`, written by the dump and `nexus client`)
  types `usePage().props` through `@inertiajs/core`'s `InertiaConfig`. The
  manifest gains `endpoints[].page` and `sharedProps` (additive, `client.v1`).
  `inertia.Prop` fields are typed as optional `unknown` instead of an empty
  `Prop` interface (`registry.SchemaOpaque`).
- **`import type { NexusPageProps } from 'nexus-client'` resolves.** The
  tsconfig merge maps `nexus-client` to the SDK's `client.d.ts` (the docs
  promised the name; nothing mapped it), and `nexus-vite-plugin` aliases it to
  `client.js` for Vite (a project's own mapping or alias for the name wins).
  When the config lists `include`, the SDK's `client.d.ts` joins it, so the
  `inertia.d.ts` augmentation applies even in a component that only calls
  `usePage()`. A solution-style root (`files: []` + `references`, the
  create-vue / create-vite layout) is left alone and the referenced config
  covering `src/` gets the mapping. Every dev mount wires it, including the
  implicit `nexus dev` one; `Client.TSConfig = client.Off` opts out.
- **`nexus({ pages })` checks page components exist.** Every component the
  manifest names (default dir `src/Pages`) must have a file, matched
  case-exactly as `import.meta.glob` keys are: `vite dev` warns once per
  missing one, `vite build` fails listing them — a typo in `inertia.Page` is
  a build error instead of a blank NotFound render. `pages: false` turns it off.
- **`nexus.Tag(key, value)`** — the exported cross-transport option for
  stamping an endpoint's registry tags, for extensions that mark the
  endpoints they register. Keys this package's options own (`auth.public`,
  `auth.flow`, `auth.requires`, `dashboard.*`, `nexus.envelope`) panic at
  registration, naming the option to use. `App.RegisterSharedProp(key, reflect.Type)` records
  a typed page-wide shared prop.

### Changed

- **Breaking (behaviour): production binaries no longer write `web/sdk`.**
  The boot-time client SDK dump ran in every mode, so a production binary
  with the SDK enabled, started in a directory holding a
  `web/vite.config.ts`, wrote `./web/sdk` (and edited `web/tsconfig.json`)
  on every boot. It now runs only under `nexus dev` or with
  `environment = "development"` — the rule the Vite hot file follows — and
  a production binary writes nothing, silently. Vendor the files at build
  time with `nexus client --out`.
- **One SDK location: `web/sdk`.** `nexus-vite-plugin`'s `sdkDir` defaults
  to `sdk` (the Go app's dump target) instead of `src/sdk`; a project with only
  `src/sdk/manifest.json` keeps reading it. The tsconfig merge no longer adds
  `baseUrl` (TypeScript 6 rejects it as deprecated); an existing one is
  honoured, and mapped paths are now computed relative to it.
- **An explicit "no SDK dump" is honoured.** The frontend-dir detection
  filled any empty `client.Config.OutDir`, so "no dump" (`frontend.Plugin`
  with `RuntimeSDK: false`) became a dump into `web/sdk`. New
  `client.Off` on `OutDir` / `TSConfig` / `ViteConfig` means "none, don't
  detect one"; the detection fills only unset fields, and `frontend.Plugin`
  maps an empty `SDKOutDir` to `client.Off`.
- The `nexus dev` SDK auto-mount now dumps into `web/sdk` on purpose (the
  page-props types and the manifest `nexus-vite-plugin` reads) but no longer
  edits `tsconfig.json`; `Client.OutDir = client.Off` keeps the routes
  without the files, `Client.DevDisabled` still closes both.
- **Inertia pages are no longer emitted as REST calls** in the SDK's
  `RestEndpoints` or `extension/frontend`'s `index.ts`: a page is rendered,
  not called. Its props type lives in `NexusPageProps`.
- **A missing frontend build is loud.** An Inertia page with no dev server
  and no manifest renders an error page naming both paths in development
  (500), and logs once in production — previously both shipped a blank page.
- **`ServeFrontend` boots a build with no `index.html`** when a Vite manifest
  proves it built (a module-only Inertia build); unknown routes then 404. A
  bundle with neither still fails fast in production, and serves a
  placeholder in development.
- A hot file whose dev server has exited reads as absent (after checking its
  pid, then whether its origin answers) instead of turning every page into an
  error; a build into the same outDir restores a live dev server's hot file.
  Boot leniency for an unbuilt bundle now needs `nexus dev` or a live dev
  server — `environment = "development"`, which scaffolds ship, no longer
  skips the production fail-fast.
- `/.vite/` (the manifest and the hot file) is never served, and the bundle is
  never directory-listed.
- The Vite plugin's CORS allowlist covers loopback, `*.localhost`, `*.test`,
  this machine's addresses and `nexus({ appOrigin })`; a `--host` bind writes a
  network-reachable origin, so LAN and mobile testing work.
- The production `index.html` is `Cache-Control: no-cache` with an ETag
  (304 when unchanged) instead of `no-store`.
- **Dependency outages are reported once.** A database or Redis that is
  down logs one line per resource — name, address, and a `fix` — instead of
  a line per retry attempt every few seconds; retry attempts are Debug,
  Redis falling back to memory is a Warn, and GORM's own logger goes through
  the app's zap logger instead of stdout.
- **Wiring errors name the consumer** (`needed by main.NewHandler
  (handler.go:24)`), diagnose pointer/value mismatches, name both colliding
  constructors on a duplicate provider, and render as a structured block
  with a `fix` line; the fx backend no longer exits 1 with empty stderr.
  A port already in use names the port and the two ways out.
- Output flags are one spelling: `--out`/`-o` everywhere (`--output` still
  parses). `--tsconfig` is a real alias of `--jsconfig`.
- The SDK auto-dump no longer prints a line per unchanged file on every
  restart.
- `nexus dev` opens the app's own URL once a Vite hot file exists.

### Deprecated

- The in-process codegen driver: `extension.Plugin.Generate`,
  `extension.Generate`, `nexus.GenerateDriver`, `App.RegisterGenerateDriver`,
  `App.GenerateDrivers`. Nothing ever read a registered driver back —
  frontend codegen runs in the CLI — so `extension.Use` no longer registers
  one and `frontend.Plugin` no longer declares it. A set `Generate` is still
  validated and flags the plugin on the dashboard; a second one no longer
  panics.
- `nexus build --package` and `nexus init --dir` (pass the positional
  argument), `nexus dev --fast` (it is the default; `--debug` is the
  inverse), `nexus pki --dns`/`--ip` (now `--dns-name`/`--ip-address`).

### Removed

- `--json-in` on `lint`, `doctor` and `routes` (JSON is the default input).
- `nexus routes --deployment` and the `DEPLOYMENT` column — they filtered a
  field no real app populates since `DeployAs` was removed.

### Fixed

- `nexus routes|lint|doctor --binary` failed on every app: print mode wraps
  the manifest in markers the parser never stripped.
- `routes --kind http|graphql|websocket` and `--auth none` matched nothing
  and exited 0; filter values are now normalized and validated.
- The SDK dump found the frontend dir only by `vite.config.ts`; a
  `vite.config.mjs`/`.js`/`.mts`/`.cjs` project got no SDK and no tsconfig
  mapping.
- The Inertia scaffold's `main.ts` failed strict `vue-tsc` (TS2769): its page
  glob now names the module shape.
- The frontend scaffold's README told you to `npm install` a project with no
  `package.json`; `nexus dev` announced "ready" after the app had died.

### Release notes

- `cmd/nexus` and `extension/cache/redis` now use internal packages added in
  this release (`internal/vitehot`, `internal/logx`). Release the parent
  module first, then bump the submodules' requirement — the usual
  "submodules require parent" step — before they build outside go.work.

## [1.59.1] - 2026-09-18

### Fixed

- **`auth.OpGates` docstring** — the example predated
  ShareProvide/ShareScoped and showed a `inertia.Share("can", fn)`
  call that never existed (Share takes one provider) closing over an
  `app` that isn't available at declaration time. The doc now shows
  the canonical CanGates Scoped + `inertia.ShareScoped` wiring and
  states the key-space boundary explicitly: op names only,
  registration-stamped permissions only — a codename that gates no
  op is invisible to OpGates by construction (check holdings via
  auth.Can/Gates or an identity-derived Scoped instead).

## [1.59.0] - 2026-09-18

### Added

- **`inertia.ShareScoped(key, handle)` — one declaration serves
  handlers and pages.** The bridge between a request-scoped fact
  (`nexus.NewScoped`) and the frontend: handlers call
  `handle.Get(ctx)`, pages read `props.<key>`, and the Scoped memo
  guarantees one compute per request for both. The key is declared
  at registration; a failed derivation omits the key from the render
  (pages degrade) while handler-side Gets still surface the error.
  Permissions are the first instance —

      var CanGates = nexus.NewScoped[map[string]bool](
          func(app *nexus.App) nexus.Compute[map[string]bool] {
              return func(ctx context.Context) (map[string]bool, error) {
                  return auth.OpGates(ctx, app), nil
              }
          })
      nexus.Boot(CanGates, inertia.ShareScoped("can", CanGates), ...)

  — but any named request fact (features, quota, tenant state)
  rides identically.

## [1.58.0] - 2026-09-18

### Added

- **Client SDK: envelope-aware `nx.op`, same-tick query batching, op
  composables.** Ops registered with `nexus.Envelope` are marked in
  the manifest and the generated `GraphqlOps` map;
  `await nx.op('usersList', vars)` picks query/mutation from the
  manifest and unwraps `{status, message, data}` — resolving `data`
  (typed `Promise<GqlData<K>>`) and throwing `NexusOpError` with the
  envelope's message on `status:false` (`{unwrap:false}` for the raw
  envelope). Independent queries issued in the same microtask
  coalesce into ONE aliased GraphQL request per path, with per-alias
  errors rejecting only their own caller. Vue gains
  `useOpQuery(name, args)` (in-flight dedupe per op+args) and
  `useOpMutation(name, {refresh, latest, onSuccess(data, message)})`
  — `refresh` refetches every mounted `useOpQuery` of the named ops
  after success; `latest` is the auto-save race guard. Verified end
  to end from Node against a live app; emitted d.ts passes
  `tsc --strict`. `nexus docs clientops`.

## [1.57.0] - 2026-09-18

### Added

- **Scoped handles auto-provide into DI.** A handler can declare the
  request-scoped fact it reads as an ordinary dependency —
  `func NewUsersPage(ctx context.Context, scope *nexus.Scoped[Scope], ...)`
  — instead of touching the package-level handle. Providers are lazy,
  so nothing injects → nothing runs; the request path is unchanged.
  `*Scoped[A]` and `*Scoped[B]` are distinct DI slots; two handles of
  the SAME T in one app fail boot with the container's
  duplicate-provider error — mark extras `.NoProvide()`, or give each
  fact its own named type.

## [1.56.0] - 2026-09-18

### Added

- **`nexus.NewScoped` — request-scoped derived values.** A fact
  derived from the request (identity + DB, tenant state, a feature
  evaluation) computed at most once per request, on first ask, and
  shared by every handler, service and prop that asks after:

      var delegatedScope = nexus.NewScoped[Scope](
          func(svc *services.UserMgmtService) nexus.Compute[Scope] { ... })
      nexus.Boot(delegatedScope, ...)
      scope, err := delegatedScope.Get(ctx)

  Lazy (never asked → never computed; no Scoped registered → the
  store middleware is never installed), singleflight per request
  (parallel GraphQL resolvers share one compute), error memoized
  alongside the value. Per-request lifetime ONLY — by design no TTL,
  no cross-request cache, no invalidation: staleness-tolerant facts
  belong on the identity, longer-lived facts in extension/cache.
  Unit tests inject with `nexus.WithScopedValue(ctx, handle, v)`.
  Full-request overhead with one handle and one Get: noise against
  the bare-handler baseline. `nexus docs scoped`.

## [1.55.0] - 2026-09-18

### Performance

- **`nexus.Arg` ops call the method directly.** The v1.54 adapter
  invoked the original method through a `reflect.MakeFunc` trampoline
  — double reflection plus argument re-packing, measured at +18% /
  +5 allocs on a trivial REST op. The synthesized args struct now
  exists only for binding and schema; the shape inspector feeds the
  named scalar slots straight from the bound struct's fields into the
  original function, in the one reflective call every handler already
  pays. An Arg op now costs exactly what the hand-written wrapper
  did (same allocations, time within noise).

- **`nexus.Envelope` is generic — no reflect.Call per request.**
  `Envelope[T, W](wrap func(T, error) (W, error))` captures the wrap
  in a typed closure (one type assertion + a native call) instead of
  invoking it reflectively; `reflect.TypeFor` still supplies the
  schema types. Source-compatible — call sites passing a typed func
  infer T and W — and the wrap's shape check moves from boot to the
  compiler. Overhead over a bare handler: +295ns/+3 allocs before,
  ~+110ns/+1 alloc after (the wrap call and the envelope value it
  actually builds).

- **`*Form` machinery is gated on a precomputed shape flag** — ops
  that don't declare a `*Form` parameter no longer pay a per-request
  slot scan.

  Benchmarks live in perf_newfeatures_test.go so regressions on
  these paths show up in `go test -bench`.

## [1.54.0] - 2026-09-18

### Added

- **`nexus.Arg` — scalar parameters become named wire arguments, no
  wrapper structs.** A registration option that kills the one-line
  args-struct adapter a scalar-taking service method used to force:

      nexus.AsQuery((*UserService).GetUser, nexus.Arg("id"), nexus.Op("userShow"))
      nexus.AsRest("GET", "/users/:id", (*UserService).GetUser, nexus.Arg("id"))
      nexus.AsMutation((*UserService).Move, nexus.Arg("id", "employerId"))

  Names map positionally onto the handler's last len(names)
  parameters (Go reflection cannot see parameter names). The args
  struct is synthesized at registration — fields tagged
  json/query/uri/graphql — so binding, the GraphQL schema and the
  generated SDK see exactly what a hand-written wrapper declared;
  non-pointer parameters are required arguments, pointer parameters
  optional, and the op name still derives from the method. One or
  two scalars ride Arg well; three or more deserve a dto. Misuse is
  a boot error: struct parameters ("register it directly"),
  Params[T] handlers, arity or name mismatches.

## [1.53.0] - 2026-09-17

### Added

- **`*nexus.Form` — raw form input, source-unified.** Declare it as a
  handler parameter (framework-filled, like `*httpx.Ctx`) or reach it
  below the handler via `nexus.FormFrom(ctx)`:

      func NewUploadCv(svc *Svc, ctx context.Context, fm *nexus.Form) (any, error) {
          title := fm.Get("title")
          cv, err := fm.File("cv")   // streams; never fully buffered
      }

  `Get / Lookup / All / Int / Bool / File / Files / Value / Bind` read
  the same fields whether the client sent a JSON body (Inertia
  `useForm`'s default), multipart/form-data (what useForm switches to
  when a file is attached), urlencoded, or — lowest precedence — the
  URL query, so a working form doesn't break the day a file input is
  added. `Bind(&dto)` bridges back into the typed world. Typed dtos
  remain the primary shape (schema, SDK, validation tags, and maskid
  ride them — raw reads bypass maskid). REST/Inertia only; on
  GraphQL/WS the param is a typed nil whose methods no-op.
  `nexus docs forms`.

- **`nexus.Errors` — field + global validation errors, one type, per-
  transport rendering.** `Field("email", "taken")` accumulates;
  `Global("provider unreachable")` writes under the reserved
  `_global` key on the same object a form already watches. Returned
  as the handler's error: Inertia pages flash + 303 back into the
  `errors` prop (the useForm convention; `X-Inertia-Error-Bag`
  honored; `inertia.Invalid` shares the path), REST answers
  `422 {"message", "errors": {field: [msgs]}}` (a validation failure
  is a normal outcome, not a 500), GraphQL carries the field map in
  the error's extensions. Field keys should match the dto's json tags
  so `useForm` binds messages onto the right inputs.

## [1.52.0] - 2026-09-17

### Added

- **Op gates: permissions declared once, on the registration.**
  `auth.Requires("add_user")` now stamps its permission list onto the
  endpoint's registry entry, and `auth.OpGates(ctx, app)` evaluates
  every registered op's own declaration for the current identity —
  returning `map[opName]bool` keyed by the op names the frontend
  already calls. No permission string exists outside the
  registration, and no page hand-builds a parallel "can" table that
  can drift from the gates. App-wide wiring is one Inertia option via
  the new `inertia.ShareProvide` (a DI-built shared prop):

      inertia.ShareProvide(func(app *nexus.App) inertia.SharedProvider {
          return func(ctx context.Context) (string, any) {
              return "can", auth.OpGates(ctx, app)
          }
      })
      // SPA: v-if="can.saveUser"

  Ops without `Requires` are always true (it reports permission
  gates, not authentication); evaluation runs server-side through the
  configured `PermissionFn` / `Backend.Authorize`, so superuser
  bypasses and custom logic hold. Built for per-render use: the
  registry compiles once per registry version (new
  `registry.Version()` mutation counter) into a table grouped by
  unique permission set — one authorize call per set, not per op;
  ~4µs / 4 allocs for 200 ops.

## [1.51.0] - 2026-09-17

### Added

- **Service methods register as handlers directly — no wrapper.** A
  method (or free function) shaped `func(ctx context.Context, args T)
  (R, error)` is a valid handler as-is: the method expression's
  receiver becomes a DI-injected dep, the trailing struct binds like
  `Params[T].Args`, and the op name derives from the method name
  (`CreateUser` → `createUser`):

      nexus.AsMutation((*UserService).CreateUser, auth.Requires("add_user"))
      nexus.AsRest("POST", "/users", (*UserService).CreateUser)

  A bound method value (`svc.CreateUser`) works too and names the
  same op — the runtime's `-fm` wrapper suffix is now stripped in
  name extraction. This removes the one-line `NewXxx` delegation
  wrapper per endpoint; the registration list becomes the single
  declaration of op + route + permission. Use pointer receivers: a
  zero-arg value-receiver method expression would read the receiver
  struct itself as the args container.

- **`nexus.Envelope(wrap)` — per-op response envelopes.** Apps whose
  wire contract wraps every result (`{status, message, data}`-style)
  no longer convert in each handler. The wrap is the app's own
  `func(T, error) (W, error)`, instantiated per registration:

      nexus.AsQuery((*UserService).ListUsers, nexus.Envelope(Wrap[[]UserRow]))

  GraphQL declares W in the schema (introspection and the generated
  SDK describe the real contract); REST serializes W. An error the
  wrap converts into a value ships as a normal 200/data response; an
  error the wrap returns follows the standard error path. Binding and
  validation failures are never enveloped, and a wrap whose input
  type doesn't match the handler fails at boot naming both types.

- **`[runtime.server] strip_trailing_slash = true`** routes
  `"/users/"` as `"/users"` — an internal rewrite at the App
  boundary, not a redirect, so non-GET bodies survive and every
  router backend behaves identically. Off by default.

- **`auth.Can(ctx, perm)` / `auth.Gates(ctx, perms...)`** evaluate
  UI permission toggles through the same `PermissionFn` /
  `Backend.Authorize` the `Requires()` endpoint gate consults, so a
  page's "can" props share one rulebook with the gates. `Gates`
  returns `map[string]bool` with every requested permission present;
  anonymous is always false.

- **`inertia.AlwaysFunc(fn)`** — `Always` for a render-time
  computation: same inclusion rule (sent on every visit, partials
  included), but `func() (T, error)`, so the error propagates through
  the render instead of being swallowed by an immediately-invoked
  closure in the props literal.

## [1.50.0] - 2026-09-16

### Changed — BREAKING

- **SQL drivers are opt-in blank imports (database/sql style).**
  `nexus/db` linked all three engines unconditionally, so a Postgres
  app shipped the ~5MB pure-Go SQLite engine it never opened, and
  vice versa. `nexus/db` now links NO engine; import the one(s) your
  app actually opens:

      _ "github.com/paulmanoni/nexus/db/postgres"
      _ "github.com/paulmanoni/nexus/db/mysql"
      _ "github.com/paulmanoni/nexus/db/sqlite"

  A `Config` naming an unlinked driver fails at wiring time with the
  exact import line to add, so the migration is one line per driver
  and cannot fail silently. `nexus new --db` scaffolds emit the right
  import; custom GORM dialects can `db.RegisterDriver` their own name.

### Fixed

- **File-backed SQLite gets a real connection pool.** The old
  `MaxOpen=1` default applied to ALL SQLite, so WAL bought nothing
  and every query in a file-backed process queued behind one
  connection. Only `:memory:` keeps the single shared connection
  (its schema dies with the connection); file DSNs now get a small
  read pool — writes still serialize inside SQLite itself, so keep a
  `busy_timeout` pragma in the DSN, as the scaffolds do.

## [1.49.0] - 2026-09-15

### Performance

- **The trace event bus is sharded by trace ID.** With the dashboard
  enabled, every request published 2-3 events through one
  process-wide mutex — the last hot-path serialization point in the
  framework. The ring and fan-out now shard by TraceID (one trace,
  one shard, so per-trace ordering is preserved; the global monotonic
  event ID recovers cross-shard order in Subscribe backlogs), and
  each subscriber's delivery is fanned in through per-shard feed
  channels, so producers never contend with other shards' publishes
  racing toward the same consumer. All existing semantics hold and
  are race-tested: exactly-once reconnect via sinceID,
  drop-don't-block for slow consumers, safe cancellation. 10-way
  parallel publish: 222ns → 115ns with the dashboard enabled but no
  client connected, 361ns → 268ns with a live stream attached. Small
  buses stay single-shard (exact global ring order).

## [1.48.0] - 2026-09-15

### Performance

- **`nexus dev` builds strip the symbol table too (`-ldflags="-w -s"`).**
  Measured on a ~95MB app: steady rebuild 2.2s → 1.9s, and the smaller
  binary also cuts the macOS code-signing cost the dev loop pre-pays
  before each swap. Safe for panic tracebacks — the Go runtime
  symbolizes from pclntab, not the symtab — and `--debug` restores
  both symtab and DWARF for delve, as before. The change lives in the
  CLI: reinstall `cmd/nexus` to pick it up.

## [1.47.0] - 2026-09-15

### Added

- **`extension/session` — Django-style server-side sessions.** A
  cookie carries an opaque 256-bit ID, the data lives in a pluggable
  `Store`, and handlers use a lazy per-request handle — for anonymous
  visitors and logged-in users alike, on REST, Inertia, and GraphQL
  (`p.Context`):

      nexus.Boot(session.Module(session.Config{}))

      s := session.Get(p.Context)
      s.Set("cart", skus)   // first write mints the ID + sets the cookie
      s.Cycle()             // rotate the ID on login (fixation defense)
      s.Destroy()           // logout: delete + expire the cookie

  Semantics mirror Django's: lazy (no store hit until the handler
  touches it), save-only-if-modified (`Touch()` forces a TTL
  refresh), and the cookie is set on the first WRITE — anonymous
  requests that never touch the session get no Set-Cookie. Stores:
  the default `NewMemoryStore()` is bounded, swept, and survives
  `nexus dev` rebuilds via the dev-state machinery; production wants
  `session.CacheStore(nexus.Cache)` (Redis via extension/cache =
  restart-safe and multi-replica) or your own DB-backed `Store`.
  Cookies are always HttpOnly, SameSite defaults to Lax; set
  `Secure: true` behind TLS. `nexus docs session`.

## [1.46.0] - 2026-09-15

### Performance

- **maskid: single-pass byte masking for REST and WebSocket
  responses.** The tree pipeline cost every masked response two full
  JSON encodes plus a decode into a `map[string]any` tree; masking now
  marshals once and rewrites the bytes in one linear pass, copying
  everything verbatim except the integer spans the policy claims. On a
  50-row list response: 90.7µs → 23.8µs, 1681 → 410 allocations,
  76.5KB → 22.9KB. Walk semantics are replicated exactly (Exclude
  subtree pruning, arrays inheriting the introducing key, only integer
  literals convert) and pinned by a differential test against the old
  pipeline. Responses also keep their struct field order instead of
  the tree's alphabetical re-sort. Inertia keeps the in-place prop
  masking it had.
- **The per-request `httpx.Ctx` is pooled** (its writer wrapper
  inlined), the routers' global middleware chain is built once instead
  of per request, `Query`/`ClientIP` memoize per request, and the
  binder stopped re-splitting struct tags per field per request.
  Six-run medians: REST −11% latency with 32 → 24 allocations, CRUD
  read −13% with 31 → 23. One contract note, documented on the type:
  a handler must not retain the Ctx past its return — gin's
  long-standing rule, now nexus's too.
- **auth: concurrent resolves of the same token single-flight** into
  one backend call instead of a stampede; the identity cache's hit
  path takes a read lock instead of serializing every authenticated
  request; a `CacheOption` that left MaxEntries zero is no longer an
  unbounded map keyed by client-supplied tokens (defaults to 4096).
- **GraphQL: one body read per locked-down request** — the production
  gate binds the full request once (maskid unmask included) and hands
  it to the cached handler, instead of gate and handler each reading
  and parsing the same bytes. The document cache — previously a
  single mutex every GraphQL request took — shards 16 ways at
  full size; small caches keep exact global LRU order.

### Changed

- **`nexus dev` request-log fields are legible and semantic.**
  `dur=610µs status=200` rendered in near-invisible dim gray on light
  and dark terminals alike. Keys use a brighter muted tone, values the
  terminal's default foreground, and the request vocabulary colors by
  meaning: status 2xx green / 3xx cyan / 4xx amber / 5xx red, `dur`
  amber past 500ms, `error=` echoing the level color.

## [1.45.0] - 2026-09-15

### Added

- **Structured, colored boot diagnostics for config errors.** A missing
  env var in nexus.toml used to surface as a Go panic — the one line
  the operator needed buried under a goroutine dump. Config mistakes
  are operator errors, not bugs: `MustLoadConfig`, `MustLoadExtensions`
  and `Boot` now render a short aligned block and exit with status 2
  instead of panicking:

      ✗ nexus: cannot load config (expand env vars)

        file   nexus.toml:139
        error  env var OATS_DB_PASSWORD is not set
        fix    export OATS_DB_PASSWORD=…  — or write
               ${OATS_DB_PASSWORD:default} in nexus.toml for a fallback

  TOML syntax errors additionally show go-toml's annotated snippet with
  a caret under the offending token. Output is colored when stderr is a
  terminal and under `nexus dev` (whose log view passes ANSI through);
  `NO_COLOR` disables it. Under the hood the error is the new typed
  `nexus.ConfigError` (source, stage, line, cause, hint), built on
  `manifest.ExpandError` / `manifest.MissingEnvError` — `LoadConfig`
  still returns an error with equivalent flat text, so callers that
  handle it themselves are unaffected.

## [1.44.0] - 2026-09-15

A hardening and housekeeping release: a full audit of the codebase
(security, hot paths, dead code, duplication) applied as ~40 commits.
Three items are security-relevant; two are breaking for how the repo is
consumed, though not for code that imports the library.

### ⚠ Release / consumption changes

- **`cmd/nexus` is now its own Go module.** `go get
  github.com/paulmanoni/nexus` previously downloaded ~175MB of module
  cache the library never links — wazero (via viteless), `x/tools`,
  esbuild, bubbletea, cobra — all of it CLI-only. The CLI now versions
  independently, like `httpx/ginrouter` and `di/fxcontainer` before it.
  `go install github.com/paulmanoni/nexus/cmd/nexus@latest` keeps
  working throughout: until the first `cmd/nexus/vX.Y.Z` tag exists it
  resolves to the old in-parent CLI. Release order matters — tag the
  parent first, then bump the submodule requires, then push the nested
  tags; the checklist lives in `cmd/nexus/go.mod`. In-repo development
  builds every module against the checked-out tree via the new root
  `go.work`.
- **`extension/cache/redis` is now its own Go module**, taking go-redis
  (~19MB) out of the main dependency graph — the "default cache pulls
  no heavy deps" promise is now true of the download, not just the
  link. The blank-import opt-in is unchanged; run
  `go get github.com/paulmanoni/nexus/extension/cache/redis` once.

### Security

- **The config server's HMAC auth is now real.** `AuthHMAC` mode
  shipped as a phase-1 stub that accepted *any* non-empty
  `Authorization` header while the operator-facing validation insisted
  a secret be configured — so it looked enforced and wasn't. Server and
  clients now implement the documented scheme (HMAC-SHA256 over
  `app:timestamp:path`, 30s skew window, constant-time compare); the
  signed path means a token minted for one app/profile cannot fetch
  another's snapshot. The version-poll endpoint, which sent no
  credentials at all, signs too.
- **`Ctx.ClientIP` no longer believes client-supplied forwarded
  headers.** `X-Forwarded-For` / `X-Real-IP` were trusted
  unconditionally, letting any direct client pick its own IP — which
  bypassed per-IP rate limits and could mint unbounded limiter state.
  Forwarded headers are now honored only when the socket peer is a
  trusted proxy (default: loopback + private ranges, the LB-in-VPC
  case), and `X-Forwarded-For` resolves right-to-left across trusted
  hops so client-prepended entries can't spoof even behind a real
  proxy. Tune with `[runtime.server] trusted_proxies` (or
  `httpx.SetTrustedProxies`); an explicit empty list trusts no proxy.
  **Behavior change:** apps fronted by a proxy on a *public* address
  must list it in `trusted_proxies` to keep seeing real client IPs.
- **The rate limiter evicts.** Its bucket map was keyed by client
  input (an IP under `PerIP`) with no eviction — one entry per distinct
  client for the process lifetime, i.e. an unbounded-memory primitive
  once combined with the header spoofing above. Buckets idle past ten
  minutes are now swept, and the hit path takes a read lock instead of
  serializing every request behind the store-wide write lock.
- **GraphQL-subscription WebSockets are bounded.** The connection had
  no read limit, no deadlines, no pong requirement, and no cap on
  subscriptions per socket — unbounded frames, leaked goroutines on
  half-open connections, unbounded contexts per client. Now: 512KiB
  read limit, ping/pong liveness with read and write deadlines, and at
  most 100 live subscriptions per connection. Cleanup also no longer
  closes a channel that subscription goroutines were still sending on
  — a send-on-closed-channel panic in an unrecovered goroutine, i.e. a
  remote process crash under ordinary disconnect timing.

### Added

- **`[runtime.websocket]` hub tuning.** `max_connections`,
  `max_message_bytes` and `workers` now flow into every `AsWS` hub.
  The options existed for years but nothing passed them, so the
  5000-connection cap, 512KiB frame limit and 32-worker pool were
  unreachable from configuration.
- **`/__nexus/ready` finally gates on peers.** The readiness endpoint
  documented peer gating but nothing ever wrote the peer table — a k8s
  probe wired to it would route traffic to a pod whose upstreams were
  all down. `extension/peer`'s prober now reports peer-level
  reachability through the new `App.ReportPeerHealth` after every
  probe round.
- **`Module()` is the canonical entry point on every extension.**
  cors, errors, frontend, openapi, security and tls previously exposed
  only `Plugin(cfg)`; they now match auth/maskid/oauth2/peer/proxy/
  config/inertia. `Plugin` remains as an alias.
- **`nexus.DeclareVolume`** (method and Option) — `UseVolume` is
  deprecated but still works; its data-driven counterpart was already
  named `DeclareVolumeProvider`.

### Fixed

- **dataloader: nested queries fetch instead of returning zero
  values.** Dispatch was gated by a loader-lifetime `sync.Once`, so
  keys loaded after the first batch fired — level-2 resolvers in a
  nested query, exactly the case dataloaders exist for — silently
  resolved to the zero value and were deduped away on retry. Dispatch
  is now per batch; results merge into a shared per-request cache, and
  a fetch error fails only its own batch's thunks.
- **Dashboard traces show real durations.** `AsRest` invoked the trace
  middleware inline at the end of the chain, where its `c.Next()`
  returned immediately — every REST request reported ~0ms and a
  pre-handler status. The handler now brackets its own body via the
  new `trace.StartRequest`/finish pair.
- **`nexustest`/`InProcess` sees decorator endpoints.** The test
  harness skipped the deferred-options drain, so `//@`-annotated
  handlers existed in production but were invisible to tests.
- **Manifest auto-load honors `NEXUS_CONFIG`.** It stat'ed cwd-relative
  `nexus.toml`, so a `NEXUS_CONFIG` deployment loaded runtime config
  from one file and its manifest blocks from another (or none).
- **`nexus routes` takes `--toml`, matching `lint` and `doctor`.** The
  old `--yaml` flag set a format nothing parsed and fell through to the
  JSON parser — a leftover from the YAML-to-TOML manifest migration.
- **`nexus dev --tui` runs the child in dev mode.** It spawned `go run`
  with no `NEXUS_*` environment at all: stale embedded frontend instead
  of `web/dist` from disk, no PreserveDev state, peer/config dev gates
  locked.
- **WS hub shutdown no longer strands reader goroutines.** The blocking
  sends on the unregister channel had no drain after `Stop`, parking up
  to thousands of `readPump` goroutines forever.
- **`nexus.AuthRoute(...)` works on `AsWS`.** It was the one
  cross-transport option missing its WebSocket half, and now sits in
  the `EndpointOption` compile-time assertion so it can't regress.

### Performance

- **The GraphQL production gate stops re-parsing every request.** With
  introspection locked down, every request paid an uncached
  `parser.Parse` plus four AST walks *in front of* the document cache
  built to avoid exactly that. Verdicts are now memoized per query
  string (bounded; unique-query floods just degrade to the old cost).
- **maskid sheds its per-key allocations.** `isID` ran up to three
  `ToLower`s per JSON key of every masked response; verdicts now cache
  per key (bounded — inbound unmask keys are client-supplied) and
  `typeNames` caches per reflect.Type.
- **REST arg binding caches its tag survey per args type**, as the
  GraphQL side already did.
- **Metrics error ring writes are O(1).** The newest-first prepend
  copied the whole 1000-entry ring on every error while holding the
  entry lock; it is now a circular buffer, and the success path's
  timestamp is an atomic instead of a mutex acquisition per request.
- **The dashboard's initial JS chunk drops from 2.3MB to 913KB.**
  elkjs (~1.4MB minified) loads lazily on the Architecture tab's first
  layout, and the fonts ship latin + latin-ext only — the
  cyrillic/greek/vietnamese subsets browsers never requested are gone
  (17 woff2 files → 6, ~150KB off every dashboard-enabled binary).

### Removed

- **Dead-on-arrival API with zero callers anywhere:** the
  `graph.CacheMiddleware` / `CachedFieldResolver` pair (bare maps
  written from concurrent resolvers — data races waiting to happen),
  `AsyncFieldResolver`, `LazyFieldResolver` and the `With*Field`
  decorators, the `WithTypedResolver` reflection engine,
  `MustGetRoot`/`GetRootOr`, `LoggingMiddleware`,
  `DataTransformResolver`, `ConditionalResolver`,
  `GetMiddlewareCount`, `App.RateLimiter()`, `client.GenerateDTS`,
  and `middleware`'s never-wired `Phase` constants, `WithRejectHook`,
  and `Builtin()`/`Custom()` constructors.
- **Deprecated aliases whose only callers were the framework's own
  tests:** `App.Engine()` (use `Router()`), `nexus.Description()` (use
  `Describe()`), and the GraphQL `Desc()`/`Middleware()` options (use
  `Describe`/`GraphMiddleware`).
- **The orphaned `DeployAs` option plumbing.** `nexus.DeployAs` never
  existed; the option-side seam was inert end to end (its own godoc
  example did not compile). The registry's deployment column and
  setter remain — they are real API.
- **`extension/visitors`** — 947 lines with no reference anywhere in
  the repo, docs, or changelog.

### Changed

- **`github.com/go-viper/mapstructure/v2`** replaces the archived
  `mitchellh/mapstructure` (same API, maintained upstream).
- **Internal consolidation with one behavioral fix:** the four typed
  resource binders (db/cache/mail/storage) now share their mechanics
  in `internal/bindutil`, which also makes options lazily evaluated in
  all four — previously only `db.BindFromConfig` worked under
  `nexus.Boot`; the cache/mail/storage variants applied options before
  nexus.toml was parsed. The two TypeScript SDK emitters share one
  rendering core (`internal/tsgen`), CRUD's REST and GraphQL handler
  factories are one set, and schema generation and arg mapping share a
  single field-name resolver so the SDL can never disagree with
  binding.

## [1.43.0] - 2026-08-10

### Changed

- **`sdk = true` is now independent of `introspection`.** It previously took
  effect only under `nexus dev` or with introspection on, and even then mounted
  behind the introspection gate — so a production binary that (correctly) locked
  `/__nexus` down also stopped serving the client its own frontend imports,
  which is the one thing the switch exists to do. The two flags now govern
  separate surfaces: `introspection` the dashboard, `sdk` the client, neither
  implying the other. The SDK routes are public, as a browser fetching
  `client.js` requires. What that publishes is a map of the API surface — paths,
  methods, argument and response shapes; no data, and no route that was not
  already listening. To ship a frontend without publishing the map, vendor the
  files at build time with `nexus client --out` and leave the flag off. An
  explicit `Config.Client` mount is unchanged and still gated.

## [1.42.0] - 2026-08-04

### Changed

- **maskid: excluding a key now prunes its whole subtree**, not just that one
  scalar. This is what makes reference data expressible. A lookup row's primary
  key is spelled `id` like every other, so excluding `id` is not an option — but
  excluding the field that *holds* the lookups (`countries`, `categories`)
  spares every ID inside it, in both directions. Without it a masked country id
  broke `country === 1` comparisons and made a masked `id` unusable as a
  `?category=` query value, since the inbound key was not ID-shaped and so was
  never converted back. Mask the records; leave the code tables numeric.

## [1.41.2] - 2026-08-04

### Fixed

- **maskid: an Inertia page's scope is now decided once, from its props struct,
  rather than per prop.** A page carries heterogeneous props — an entity list
  whose type is in scope sitting next to a bare `[]uint` of the same entity's
  IDs (`appliedAdvertIds`) — and judging each prop by its own root type masked
  the first and not the second. The two then no longer compared equal, so a
  membership test against the sidecar list silently returned false for every
  row. An in-scope page now masks every prop it carries, leaving the field
  policy to decide which keys are IDs. Pages out of scope keep the per-prop
  behaviour.

## [1.41.1] - 2026-08-04

### Fixed

- **maskid: the type scope now resolves through generic response envelopes.**
  A handler returning `Response[T]` or `Page[T]` has the reflect name
  `Response[github.com/you/app/svc.Invoice]`, which no scope would name, so
  every such response fell out of scope and went unmasked on the JSON-walking
  transports (REST, Inertia, WebSocket). Type arguments are now unwrapped too.
  GraphQL was unaffected, since it scopes on the object type itself.

## [1.41.0] - 2026-08-04

### Added

- **`maskid.Config.Types` / `MatchType` — scope masking to named response types.**
  Masking was previously all-or-nothing, which rules it out for an app where only
  part of the surface can safely hand out opaque IDs (the rest feed a system
  outside the app that expects integers). Naming the types that stay inside masks
  those and leaves the others alone. The name is the Go type of the response,
  which is also its GraphQL object name; pointers, slices and the generic
  envelopes handlers return (`Response[T]`, `Page[T]`) all resolve to the
  underlying name. Only masking is scoped — unmasking always runs and needs no scope, since
  a value converts only if it decrypts.

### Changed

- **GraphQL arguments keep their `Int` declaration under maskid.** v1.40.0
  declared masked ID *arguments* as `MaskedID` too, which churned the SDL and the
  generated client for every caller. It turns out to be unnecessary: a masked
  value in `variables` is already converted back to an integer when the request
  body is bound, before graphql-go coerces it. Output fields still become
  `MaskedID` — there a rewrite genuinely can't work. An inline literal in a
  hand-written query now needs a raw integer.
- The GraphQL `GET` handler unmasks its `variables` query parameter, which
  bypassed request binding and so missed the conversion the `POST` path got.

## [1.40.0] - 2026-08-04

### Added

- **`extension/maskid` — opaque IDs on the wire, with no handler changes.**
  `maskid.Module(maskid.Config{Key: …})` replaces integer IDs with 22-character
  opaque strings on the way out and converts them back on the way in, so handlers,
  models and SQL keep using `int64` keys. Covers all four transports: REST out and
  in (path, query, header, form, JSON body — hooked once in `httpx` binding),
  GraphQL (ID fields declared as the new `MaskedID` scalar in both outputs and
  arguments, since graphql-go coerces through the declared type and a rewrite
  can't work there), Inertia (masked after prop resolution, so `Defer`/`Optional`
  props are included), and WebSocket (inbound envelope data, every outbound
  `Emit`). Requests carrying raw integers still work, so rollout is incremental.
  Default codec is a deterministic, authenticated AES permutation over a single
  block — real encryption rather than the reversible arithmetic of hashids/sqids
  — swappable via `Config.Codec`. Field policy is tunable with
  `Include`/`Exclude`/`Match`. Masking removes enumeration and inference; it is
  not access control. `nexus docs maskid`.

- **`nexus.DevStateDir()`** — the `nexus dev` session directory, for state that
  already has its own on-disk format and so doesn't fit `PreserveDev`'s
  snapshot-as-bytes model. Returns `""` outside `nexus dev`.

### Changed

- **An OAuth2 login now survives a `nexus dev` rebuild.** `extension/oauth2`'s
  default token store is go-oauth2's in-memory store, which is literally
  `NewFileTokenStore(":memory:")`; under `nexus dev` it is now pointed at a file
  in the session state directory instead. Tokens are opaque values held in the
  store rather than self-contained JWTs, so this alone keeps sessions alive
  across a rebuild — no more re-authenticating on every save. Same lifetime as
  `PreserveDev`: survives rebuilds, not a Ctrl-C. Production binaries are
  unaffected (they never see `NEXUS_DEV_STATE`), and setting `Config.TokenStore`
  opts out either way.


## [1.39.0] - 2026-08-04

### Security

- **WebSocket upgrades now default to same-origin.** Every upgrader in the
  framework hardcoded `CheckOrigin: func(*http.Request) bool { return true }` —
  the dashboard's trace stream (`/__nexus/events`) and live snapshot
  (`/__nexus/live`), every `nexus.AsWS` endpoint, and GraphQL subscriptions.

  Browsers apply neither CORS nor the same-origin policy to WebSocket
  handshakes, and the handshake carries cookies. So any page a victim visited
  could open a socket to a nexus app and read or send whatever it carries, as
  the victim — cross-site WebSocket hijacking. Confirmed against a running app:
  an upgrade with `Origin: https://evil.example` returned `101 Switching
  Protocols`; it now returns `403 Forbidden`.

  Worst case was the dashboard, whose streams carry every request trace and the
  cached auth identities. That surface needs introspection open — always true
  under `nexus dev`, off by default in production — so the realistic exposure
  was developer machines and deployments that opened introspection. `AsWS`
  endpoints and GraphQL subscriptions had no such gate and were exposed
  wherever they were mounted.

  Requests with no `Origin` header are still accepted: non-browser clients
  don't send one, and they carry no ambient authority to abuse.

  **This can break a legitimately cross-origin frontend.** Allowlist it:

  ```toml
  [runtime.websocket]
  allowed_origins = ["https://app.example.com", "*.example.com"]
  ```

  `"*"` restores the old accept-everything behavior. Loopback origins are always
  allowed under `nexus dev`, where the SPA (:5173) and app (:8080) are
  cross-origin by design. A `*.example.com` entry deliberately does not match
  the parent `example.com`.

- **`extension/inertia`'s validation-error cookie no longer hardcodes
  `Secure: false`.** It carries the user's submitted field values and was sent
  in cleartext even on HTTPS sites. Now follows the request scheme, honoring
  `X-Forwarded-Proto` for the usual TLS-terminating-proxy deployment, and sets
  `SameSite=Lax`.

### Added

- **`[runtime.server] idle_timeout`** (default **120s**, new). Go falls back to
  `ReadTimeout` when `IdleTimeout` is unset, and `ReadTimeout` was unset too —
  so idle keep-alive connections were held indefinitely and a few thousand cheap
  connections could exhaust the process's file descriptors. `"-1s"` restores
  Go's behavior. (`extension/tls` already set this; the main listener didn't.)
- **`[runtime.server] read_timeout` / `write_timeout` / `max_header_bytes`** —
  all OFF by default and deliberately so: a `write_timeout` cuts server-sent-event
  streams and long downloads mid-flight, and a `read_timeout` cuts large uploads.
  The framework can't tell which an app serves, so these are yours to set.
- **`[runtime.server] max_body_bytes`** — caps request bodies, 413 over the
  limit. **OFF by default**, for the same reason: nexus can't know whether an app
  accepts large uploads, and rejecting them at a framework-chosen ceiling would
  be a worse failure than the risk. Worth setting — without it every
  JSON-binding handler is an unbounded memory sink for anonymous clients.
- `httpx.CheckWebSocketOrigin` / `httpx.SetAllowedWebSocketOrigins` — the shared
  origin policy, for adapters outside the framework.

## [1.38.0] - 2026-08-03

### Changed

- **`nexus dev` rebuilds are ~30% faster.** Measured on a ~114MB app: 4.27s →
  2.99s per rebuild. Profiling the build's action graph makes the reason plain —
  of ~2000 actions in a warm rebuild, every one but the link is a cache hit, so
  the only lever that moves is making the linker emit less. Two defaults follow,
  both dev-only:
  - **DWARF is stripped by default** (`-ldflags=-w` — what `--fast` used to opt
    into). Worth ~20% of every rebuild, and a dev binary is discarded on the next
    save. `--debug` keeps it for delve and complete panic traces; `--fast` still
    exists and is now the default.
  - **The frontend bundle is stubbed out of the dev binary.** Under `NEXUS_DEV`
    `ServeFrontend` already reads `web/dist` from disk, and the SPA is served by
    viteless on :5173 regardless — so the embedded copy is dead weight that gets
    relinked on every save (9.5MB / 198 files on the app measured). `nexus dev`
    now maps it to empty files through the same `go build -overlay` it already
    uses for handler codegen, which Go applies to `//go:embed` reads: no build
    tags, no scaffold change, existing apps included. `--no-embed-stub` opts out.

    Scoped strictly to the tree a `ServeFrontend` call names, so assets an app
    genuinely reads at runtime (fonts, templates, seed data) are never touched.
    The bundle's `index.html` stays real HTML (boot fails fast without it) and
    the Vite `manifest.json` stays real too, since `extension/inertia` resolves
    entry chunks through the embed when `NEXUS_VITE_DEV` isn't set.

  Worth keeping in perspective: with build-then-swap this is
  latency-until-your-change-is-live, not downtime — the app keeps serving
  through the whole compile, and the swap itself is ~22ms.

### Added

- `nexus dev --debug` — keep DWARF in the dev binary (the inverse of `--fast`).
- `nexus dev --no-embed-stub` — embed the real frontend bundle in the dev binary.

## [1.37.1] - 2026-08-03

### Fixed

- **Ctrl-C on `nexus dev` no longer takes 5 seconds.** Shutdown ran
  `http.Server.Shutdown` with an unbounded context, and nothing cancelled the
  contexts of the requests it was waiting on — so a single request still in
  flight (an SSE stream, a long poll, a slow query, a browser mid-request) held
  the app open until the dev loop gave up and sent SIGKILL. Measured on a
  scaffolded app: 5.05s with one in-flight request, now 0.02s. This also cost a
  full 5s on every build-then-swap **rebuild** that happened to catch a live
  request, not just on Ctrl-C. Hijacked WebSockets were never affected.

### Added

- **`[runtime.server] shutdown_timeout`** (and `Config.Server.ShutdownTimeout`)
  — the graceful-drain window on SIGINT/SIGTERM. Defaults to 10s in production
  and 250ms under `nexus dev`, where nothing in flight survives the rebuild
  anyway. A malformed duration falls through to the default rather than
  refusing to boot.
- **`di.WithStopTimeout`** bounds the whole lifecycle stop chain, so a resource
  whose `Close` blocks can't hold the process open either. Recorded on
  `di.Spec` and honored by both containers (the fx adapter maps it onto
  `fx.StopTimeout`), and enforced by running `Stop` on its own goroutine — a
  hook that ignores its context outright is abandoned rather than waited on.

### Changed

- In-flight request contexts are now derived from a cancellable root
  (`http.Server.BaseContext`) and cancelled when the drain window closes, so a
  handler that selects on its context returns immediately and shutdown
  finishes early instead of running out the clock. Under `nexus dev` the cancel
  is immediate. Connections that still won't budge are closed outright, which
  `Shutdown` alone never did.
- The dev loop's SIGTERM→SIGKILL grace period drops from 5s to 750ms, and
  escalation now prints `● app didn't exit within 750ms · SIGKILL` instead of
  pausing silently. A healthy app bounds its own shutdown well inside that.

## [1.37.0] - 2026-07-25

### Added

- **`nexus.PreserveDev` — in-memory state that survives a `nexus dev` rebuild.**
  A rebuild replaces the process, so anything living in a map used to die with
  the old binary: seeded users, rows you POSTed, fixtures set up by hand. A
  value that implements `nexus.DevState` (`SnapshotDev() ([]byte, error)` /
  `RestoreDev([]byte) error`) and registers itself with
  `nexus.PreserveDev(name, v)` now hands its state to the dev loop on the way
  out and takes it back on the way in. Restore happens inside `PreserveDev`, so
  lazily constructed DI values work without ordering rules; the snapshot is
  written on the graceful shutdown `nexus dev` already triggers before swapping
  in the new binary. `nexus.PreserveDevJSON(name, get, set)` covers state you
  can marshal directly, with no methods to write.

  Dev-only (gated on the state file the CLI passes, so a production binary
  carries a no-op), per-session (state survives rebuilds, not a Ctrl-C),
  graceful exits only, and best-effort — a failed snapshot or restore is
  reported on stderr and skipped, never fatal, so stale state from a struct you
  just reshaped can't stop the app from booting. Caches are deliberately not
  preserved: they're rebuildable by definition, and restoring typed values
  through an `any`-shaped store is unsound. `nexus docs devstate`.
- **`auth.MemoryUserStore` implements `DevState`**, so dev users survive a
  rebuild once registered (`nexus.PreserveDev("auth.users", store)`). Password
  hashes travel as-is, so restored users authenticate exactly as before; a user
  the new process seeds itself wins over the snapshot, so changing the seed in
  code does what you expect.

## [1.36.0] - 2026-07-25

### Changed

- **`nexus dev` rebuilds are build-then-swap.** The loop used to kill the app and
  then compile, so every save took the server down for the whole build. The next
  binary now compiles while the current one keeps serving, and the swap happens
  only once the build is green. Measured restart outage on a small app: **1413ms
  (`go run`) → 22ms**, and it no longer scales with build time. Three
  consequences: a **failed build leaves the running app up** (the compile error
  prints, the last good build keeps serving); a save that doesn't change the
  binary **skips the restart entirely** (Go's output is content-addressed, so
  comment-only edits and edits outside the build graph preserve app state); and
  the app is exec'd directly instead of under `go run`, dropping that
  supervisor's ~40MB RSS. The freshly built binary is pre-executed once —
  aborted inside the Go runtime, before any package init or `main` — so the OS
  pays its first-exec cost (~450ms of code-signature validation on macOS) while
  the old process is still answering. `--go-run` restores the legacy loop.
- **The dev watcher is scoped to real build inputs.** `_test.go` files (never
  compiled into the binary), `testdata/`, and nested modules with their own
  `go.mod` no longer trigger rebuilds — unless the root module `replace`s into
  such a module, which makes it a genuine build input.
- **`nexus dev` starts faster.** The handler codegen's `go list` calls (one for
  the main package plus one per annotated package, every restart) collapse into
  a single cached `go list -find ./...` per session, invalidated by a
  go.mod/go.sum change or a lookup miss: 203ms → ~1ms per restart on a small
  app, 317ms → 0 on a large tree. The Inertia auto-detection and the viteless
  dev server boot now run off the critical path instead of ahead of the first
  compile, and the ready line no longer burns its frontend grace window on apps
  with no dev server: first build 0.85s → 0.66s, ready 1.31s → 1.16s with a
  frontend, 2.82s → 1.32s without one.

### Added

- **`.nexusignore`** — a per-project ignore list next to `nexus.toml`, read at
  startup and honored by both the Go watcher and the `--dist` frontend build.
  Patterns are a documented subset of `.gitignore`: a trailing slash matches
  directories only, a pattern without a slash matches at any depth, a slash
  anchors it to the project root, `**` spans directories, and `!` re-includes
  (later rules win). An ignored directory is pruned, so nothing inside it is
  watched. For generated trees, fixtures, or a sibling service's source living
  in the same repo.

### Fixed

- **A package created mid-session never rebuilt.** A newly created directory was
  filtered out as irrelevant before it could join the watch set, so neither its
  creation nor any later edit inside it reached the rebuild signal. Directory
  creates now register the tree first, and count as a change when it already
  holds build inputs (`mkdir pkg && write pkg/x.go` is one editor action, so the
  file usually lands before the watch does).

## [1.35.0] - 2026-07-23

### Added

- **`extension/inertia/inertiatest` — an in-process test harness for Inertia
  pages.** The Inertia-aware layer over `nexustest`: it boots a listener-less
  `App` (real router, middleware, DI, and reflective dispatch — no socket),
  issues visits with the correct `X-Inertia` headers, decodes the page object
  (from an XHR's JSON body or an initial load's `data-page` attribute), and
  returns a `*Page` with prop / merge / defer / redirect / validation
  assertions. A cookie jar persists across visits, so flash-error and session
  flows (e.g. a failed submit that 303s back with its errors) work like a real
  browser via `Visit.Follow()`. `New(t, cfg, opts...)` boots and wraps in one
  call; `Wrap(t, app)` layers Inertia visits over a `nexustest.App` you already
  built, so one app serves both REST/GraphQL and Inertia assertions. Visit
  helpers (`Get`/`Post`/`Visit`/`Partial`/`Load`), request options
  (`Version`/`ErrorBag`/`Except`/`Reset`/`Header`), and fluent `Page`
  assertions (`AssertComponent`/`AssertProp`/`Bind`/`AssertMerge`/
  `AssertDeferred`/`AssertError`/…) collapse the old boot-a-real-port
  boilerplate. Documented under `nexus docs inertiatest`.

## [1.34.0] - 2026-07-21

### Added

- **`extension/proxy` — a strangler-fig bridge.** Reverse-proxies routes to a
  legacy upstream (e.g. a Django app being migrated) AND registers each proxied
  route on the dashboard, tagged as a proxy (new `registry.ProxyTag`), clustered
  in a dashboard module — so the architecture graph becomes a live migration
  board showing which routes are still forwarded vs. already served natively.
  The core move is **auto-yield**: at boot, any configured route that already
  has a native nexus handler at the same method+path is skipped, so migrating a
  route is purely additive (add the `AsRest`, rebuild, and it leaves the
  "Proxied" cluster automatically). Includes a live migration burndown via a
  `migration` dashboard snapshot-extra, an optional catch-all `Fallback` for the
  long tail, and header/path passthrough (stdlib `httputil.ReverseProxy`, no new
  deps). Optionally **launches and supervises the upstream process** via
  `Config.Command` — distinct Dev/Prod argv (dev picked under `nexus dev` /
  `NEXUS_DEV`), working dir, env, line-prefixed child logs, a readiness gate,
  and graceful interrupt-then-kill shutdown — so one `nexus dev` boots both
  nexus and the legacy app. The dashboard gains a **"Proxied"** header panel (a
  live burndown of the `proxied` snapshot-extra: upstream, migrated-of-total, and
  per-route proxied/migrated status), and proxied routes cluster in their own
  module on the architecture graph — so the dashboard doubles as a migration
  cockpit.

- **`BindFromConfig` parity for cache / storage / mail.** `db.BindFromConfig`
  read a `[databases.*]` block; the other resource binders had no equivalent, so
  wiring a cache/disk/mailer from config meant hand-writing a `build func()`
  closure full of `nexus.Get` calls. Now `cache.BindFromConfig[T]("name")`,
  `storage.BindFromConfig[T]("name")`, and `mail.BindFromConfig[T]("name")` read
  the `[cache.<name>]` / `[storage.<name>]` / `[mail.<name>]` blocks directly.
  Every key is optional (cache overlays `NewConfig()` defaults; storage/mail
  default fields to zero), the build runs at boot so it works under `nexus.Boot`,
  and the driver's required-field validation still fires at boot. Lifecycle
  options (`WithDefault`, `WithDescription`) stay explicit in code — the block
  describes the connection, code describes its role.
- **`nexus.EndpointOption` — a name for the cross-transport per-op option
  contract.** `AsRest` / `AsQuery` / `AsWS` each take a transport-specific option
  interface; an option that works on all three (Public, Describe, WithIcon,
  HideFromDashboard, Use → auth.Required/Requires) had to satisfy all three by
  convention with nothing to name it. `EndpointOption` is that intersection —
  return it from your own cross-transport option, and a compile-time assertion
  keeps every built-in one honest.
- **`App.Router() httpx.Router`** — the correctly-named accessor for the app's
  router seam.

### Changed

- **`ginAuthMiddleware` → `authMiddleware`, `cors.ginHandler` → `corsHandler`,
  `auth.Describe` → `auth.InspectExtractor`.** Post-router-seam cleanup: internal
  helpers and one exported function carried gin/`Describe` names that no longer
  matched what they do (the middleware is router-agnostic; `Describe` collided
  with the cross-transport `nexus.Describe` option). Internal renames are
  invisible; `auth.Describe` stays as a `// Deprecated:` alias of
  `InspectExtractor`.

### Deprecated

- **`App.Engine()`** — use `App.Router()`. Both return the same `httpx.Router`;
  "Engine" was a leftover from the gin-only era. Alias kept.
- **`auth.Describe(Extractor)`** — use `auth.InspectExtractor`. Alias kept.

### Removed

- **`extension/tour`** — the guided-product-tour plugin has been removed. It was
  self-contained (nothing else in the framework imported it), so removal is a
  clean drop; apps that used it should pin an earlier nexus version or vendor the
  package.

## [1.33.3] - 2026-07-17

### Fixed

- **`go build` on Windows works again.** The embedded viteless dep cross-process
  cache lock (`viteless/internal/store`) used `golang.org/x/sys/unix` (`Flock`,
  `LOCK_SH/EX/UN`) unconditionally, so any `go build` of a nexus app on Windows
  failed with `undefined: unix.LOCK_SH`. Bumped to **viteless v0.2.1**, which
  splits the lock syscall behind a `flockFile`/`funlockFile` seam — `flock(2)` on
  unix, `LockFileEx`/`UnlockFileEx` on Windows — preserving the same advisory
  whole-file locking semantics on both. Pure dependency bump; no nexus API change.

## [1.33.2] - 2026-07-09

### Fixed

- **WebSocket handler panics no longer crash the process.** A WS message handler
  runs in the connection's read-loop goroutine (`ws.Hub.readPump`), and an
  unrecovered panic in a goroutine takes down the whole Go process — so a handler
  doing `m[k]=v` on a nil map or an out-of-range index could kill the server,
  while the identical bug in a REST handler was caught. `callWSHandler` now
  recovers, mints a `*trace.StackError`, and routes it through the same path a
  returned error takes: `finish(500)`, a `request.op`/`request.end` event on the
  bus (dashboard "failed traces" + captured stack), an `error` envelope to the
  client, and a `[nexus] panic recovered in WS handler ...` line on stderr — the
  read loop survives. This closes the last execution context that lacked panic
  recovery (REST/GraphQL, workers, crons, and pubsub subscribers already had it).

### Changed

- **Config auto-load panics now carry `nexus:` context.** Bare `panic(err)` sites
  in `autoLoad` are wrapped (`nexus: failed to read config %q`, `nexus: malformed
  config (%s)`, `nexus: malformed [extensions.*] ...`) so a startup failure names
  the config source and cause instead of dropping a raw toml/IO stack.

### Added

- **Boot-time self-check in dev — foot-guns surface at startup, not at 2am.** A new
  `nexus.RegisterBootCheck(func() []manifest.Issue)` hook lets a package report
  live-topology problems at boot; nexus runs every registered check plus the
  existing `nexus.toml` config lint (addresses, CIDRs, CORS-credentials-wildcard,
  rate limits, unimported extensions) automatically under `nexus dev` / any
  `NEXUS_DEV` run, printing them to stderr. It's **advisory** (never aborts boot;
  genuine fatal misconfig still fails where it already did) and **dev-only** (zero
  cost in production). Covers both `nexus.Run` and `nexus.Boot`. The flagship
  check: **pubsub** now reports *"N topic(s) declared but no transport bound — add
  pubsub.UseInMemory()/UseRabbit(...)"* at boot instead of only at the first
  `Publish`. Runs last among boot invokes, after `BindTopics`, so a bound
  transport never false-positives.
- **`ERRORS.md`** documents nexus's three-layer error model (registration panics →
  `nexus:` prefix; handler-returned errors → transport; runtime panics → recover →
  `StackError` → dashboard + stderr) and the recover-invariant every execution
  context upholds — enforced by the new `TestUserHandlerPanicsAreRecovered`.

## [1.33.1] - 2026-07-08

### Added

- **Database SQL logging is quiet by default outside dev, with a config opt-out.**
  GORM's default logger prints slow-query / error lines to stdout in every
  environment. Now the `db` manager sets the logger explicitly: warn-level under
  `nexus dev` / a development environment (`runtime.environment = "development"`
  or `NEXUS_DEV`), and **silent otherwise**, so a production binary stays quiet.
  Override per connection with `[databases.<name>] log = "..."` (or
  `db.Config.LogLevel`): `"silent"`/`"false"`/`"off"`, `"error"`,
  `"warn"`/`"true"`/`"on"` (GORM's slow-query+error default), or `"info"`/`"all"`
  (every statement). Record-not-found is no longer logged as an error.

### Fixed

- **`nexus client` auto-dump: parse `tsconfig.json` / `jsconfig.json` as JSONC.**
  Merging the SDK path mappings failed with `invalid character '}' looking for
  beginning of object key string` when the config contained comments or trailing
  commas — both of which `tsc` accepts (the files are JSONC). The parser now
  strips `//` and `/* */` comments and trailing commas before the strict JSON
  decode (string literals preserved); the rewritten file is normalized to strict
  JSON as before.

## [1.33.0] - 2026-07-08

### Added

- **OAuth2 folds into a single `auth.Module` call — `auth.Config.Endpoints` +
  `oauth2.Backend`.** Previously a token server meant a standalone `NewServer`
  provide plus a separate `auth.Module` plus hand-wired `AsRest` lines for
  `/oauth/token`, login, and logout. Now:
  - `auth.Config.Endpoints{Login, Logout, Token, Revoke}` lets `auth.Module`
    mount its own HTTP front doors, each backed by a `Config.Backend`
    capability — so one `auth.Module(auth.Config{...})` owns the whole auth
    surface. Each path is off unless set; all are `Public`.
  - The cohesive backend gains three optional capabilities (discovered by type
    assertion, like `Resolve`/`Login`/`Authorize`): `Issue(ctx, *Identity)
    (any, error)` (login response / token pair), `RevokeToken(ctx, token)
    error` (logout), and `TokenHandler() httpx.HandlerFunc` (the raw grant
    endpoint).
  - `oauth2.Backend(oauth2.Config{...})` returns a ready `auth.BackendOption`
    implementing every capability, so an OAuth2 server drops straight into
    `auth.Config.Backend`. New `oauth2.Config` fields `LoginPath` / `LogoutPath`
    / `LoginClientID` / `LoginClientSecret` power the JSON login endpoint.

      auth.Module(auth.Config{
          Backend:   oauth2.Backend(oauth2.Config{Authenticator: authFn}),
          Endpoints: auth.Endpoints{Token: "/oauth/token", Login: "/api/auth/login"},
      })

  All additive: the `Config.Endpoints` zero value mounts nothing, so existing
  configs are unchanged. See `nexus docs auth`.

### Changed

- **`oauth2.Module` is now a thin wrapper over `auth.Module`** — it builds the
  server via `oauth2.Backend` and declares `auth.Endpoints` for the token/revoke
  paths, eliminating the internal `holder`/`atomic.Pointer` bridge that threaded
  the live `*Server` into the resolver closure during DI startup. Behavior is
  unchanged (the end-to-end password-grant test is untouched); `oauth2.Module`'s
  `RevokePath` now responds `200 {"ok":true}` instead of `204`.

### Deprecated

- **`auth.LoginEndpoint` / `auth.LogoutEndpoint`** — superseded by
  `auth.Config.Endpoints.Login` / `.Logout`, which mount the same handlers from
  inside `auth.Module` and source the issuer/revoker from the backend's `Issue`
  / `RevokeToken` capabilities. Both remain as thin wrappers and keep working.

## [1.32.3] - 2026-07-07

### Added

- **`auth.LoginHandler` / `auth.LogoutHandler` — exported handler builders.**
  `LoginEndpoint`/`LogoutEndpoint`'s `WithIssuer`/`WithRevoker` are static
  callbacks set at module-build time, so they can't reach DI-provided services
  (e.g. an OAuth2 token server). These builders return the same
  `httpx.HandlerFunc` the endpoints install, so an app can wire them inside its
  own `AsRestHandler` factory — where deps ARE injected — without a package
  global:

      nexus.AsRestHandler("POST", "/auth/login",
          func(m *auth.Manager, srv *TokenServer) httpx.HandlerFunc {
              return auth.LoginHandler(m, func(ctx, id *auth.Identity) (any, error) {
                  return srv.IssueToken(ctx, id.ID)
              })
          }, nexus.Public())

  `LoginEndpoint`/`LogoutEndpoint` now delegate to them, so behavior is
  unchanged; this only adds the DI-friendly wiring path. See `nexus docs auth`.

## [1.32.2] - 2026-07-07

### Added

- **`auth.LogoutEndpoint` — companion to `LoginEndpoint`.** A one-line helper
  that registers a `POST` logout endpoint (default `/auth/logout`): it extracts
  the presented token, drops it from the identity cache (`Manager.Invalidate`),
  and — with `auth.WithRevoker(func(ctx, token) error)` — invalidates it in the
  app's own store (an OAuth2 server, a DB session). Options: `auth.LogoutAt(path)`
  and `auth.LogoutExtractor(e)` (default `Bearer()`; use `auth.Cookie(...)` for
  cookie sessions). Public and idempotent — it authenticates by the very token
  it revokes, always returns `200 {"ok": true}`, and reveals nothing about
  whether a session existed. See `nexus docs auth`.

## [1.32.1] - 2026-07-07

### Added

- **`auth.LoginEndpoint` — HTTP front door for `Manager.Login`.** A one-line
  helper that registers a `POST` login endpoint (default `/auth/login`) which
  authenticates a `{username, password}` body through the login-capable
  `Config.Backend` and returns the result — so apps no longer hand-write a
  handler just to reach `Manager.Login`. Options: `auth.LoginAt(path)` and
  `auth.WithIssuer(func(ctx, *Identity) (any, error))` to shape the success body
  (e.g. mint a token); without an issuer it returns `{"identity": …}`. The
  endpoint is `Public` (you can't require a token to obtain one), returns 401 on
  invalid credentials with no user enumeration, and needs a `Config.Backend`
  that implements `Login`. See `nexus docs auth`.

## [1.32.0] - 2026-07-07

### Added

- **`auth.Config.Backend` — one cohesive, DI-constructed auth backend.**
  Previously the resolver (`Scheme.Resolve`) was a static func that couldn't see
  DI dependencies, so apps needing a resolver bound to app services (a DB, a
  token server) had to smuggle them in via package globals + a backfill
  `Invoke`, and authorization lived in a separate `Config.Authorization` block.
  `Config.Backend` collapses this: declare ONE backend, built from the container
  via `auth.UseBackend(func(deps...) *YourBackend { … })` (or `auth.StaticBackend(v)`
  for no deps). The framework discovers capabilities by type assertion — a
  backend implements any subset of `Resolve(ctx, token)` (fills any `Scheme`
  with a nil `Resolve`), `Login(ctx, Credentials)` (powers the new
  `Manager.Login`), and `Authorize(id, required) bool` (replaces the
  `Config.Authorization` permission check). A scheme-less `Config` with a backend
  gets a default bearer scheme. New `auth.Manager.Login`.

  Fully backward compatible: every new field/method is additive, the `Backend`
  zero value reproduces prior behavior exactly, and `Scheme.Resolve` /
  `Config.Authorization` / `auth.Authenticate` / `ModelBackend` are unchanged.
  Note `UseBackend` returns your concrete type, distinct from the existing
  `auth.Backend` login interface. See `nexus docs auth`.

## [1.31.0] - 2026-07-06

### Added

- **`extension/mail` — outbound email.** A Laravel-Mail / ActionMailer-style
  abstraction: app code composes a `mail.Message` and hands it to one `Mailer`
  interface; the transport is chosen by config, so log-in-dev / SMTP-in-prod is
  a `Config` change, not a code change. Wired like a cache or disk — a typed
  `mail.Bind[T]` whose `T` embeds `*mail.Manager`, injected into handlers and
  shown on the dashboard as a `resource.KindMail` resource. Two backends, both
  dependency-free (no third-party mail library):
  - `log` — the default (empty-driver) backend; prints each message and sends
    nothing, the safe default for dev/tests. Exposes `.Sent()` for assertions.
  - `smtp` — any SMTP server over stdlib `net/smtp`: STARTTLS (587), implicit
    TLS / SMTPS (465), and PLAIN auth. Builds a proper MIME message —
    `multipart/alternative` for text+HTML, `multipart/mixed` for attachments —
    with quoted-printable bodies and RFC 2047-encoded headers.

  `mail.Message` carries From (defaulting to `Config.FromAddress`), To/Cc/Bcc,
  ReplyTo, Subject, Text, HTML, Headers, and Attachments; recipients are
  validated before any transport round-trip. New `resource.KindMail` +
  `resource.NewMail`. See `nexus docs mail`.

## [1.30.0] - 2026-07-06

### Added

- **`nexus build` embeds `nexus.toml` into the binary.** The built artifact
  is now self-contained — no config file needs to ship alongside it. When a
  `nexus.toml` sits in the main package's directory, `nexus build` bakes it in
  via the linker (`-ldflags -X`, base64-encoded) and `Boot` uses it as a
  fallback when no config is found on disk. Resolution order is unchanged and
  disk still wins: `NEXUS_CONFIG` → `nexus.toml` in cwd → next to the
  executable → the embedded copy. So a deployed binary Just Works with no
  sidecar file, yet operators can still drop a `nexus.toml` next to it to
  override without a rebuild. The raw file is embedded with `${VAR}`
  placeholders intact, so secrets resolve from the runtime environment and are
  never baked into the binary. A pure-Go app with no `nexus.toml` embeds
  nothing.

## [1.29.2] - 2026-07-05

### Fixed

- **Deployed binaries now find `nexus.toml` beside the executable.** `Boot`
  previously looked for `nexus.toml` only in the current working directory,
  so a binary launched from a different directory (a common deploy layout —
  `./app` run from `/home/user` with the config in a project subdir) silently
  fell back to framework defaults, most visibly binding `:8080` instead of the
  configured `addr`. `resolveConfigPath` now resolves in priority order:
  `NEXUS_CONFIG` → `nexus.toml` in cwd → `nexus.toml` next to the executable.
  Ship the binary alongside its `nexus.toml` and the configured listen address
  is honored regardless of launch directory.
- **A missing `nexus.toml` warns instead of silently defaulting.** `Boot` still
  tolerates the file's absence (config-less apps boot), but now prints a clear
  stderr notice that framework defaults are in effect and the listen addr is
  falling back to `:8080`, rather than leaving the mystery port unexplained.

## [1.29.1] - 2026-07-04

### Fixed

- **CI lint job now runs.** `golangci-lint-action` downloaded the prebuilt
  golangci-lint binary (built with go1.24), which refuses to analyze the
  go1.26 modules ("the Go language version used to build golangci-lint is
  lower than the targeted Go version"). Switched to `install-mode:
  goinstall` so CI compiles it with the runner's Go 1.26 — matching how
  `make lint` runs locally.
- **`nexus version` honors release `-ldflags`.** `var Version =
  resolveVersion()` ran its initializer at startup and overwrote any
  linker-injected value, so a binary built with
  `-ldflags "-X main.Version=vX.Y.Z"` still printed `dev`. `Version` is now
  left uninitialized (so the `-X` value survives) with the BuildInfo/vcs/
  `dev` fallback filled in `init()`. Normal `go install …@vX.Y.Z` installs
  were unaffected (they resolve the tag via BuildInfo).
- **Scaffold Go directive.** `nexus new` generated a `go.mod` pinned to
  `go 1.25.1`; bumped to `go 1.26` to match the framework's requirement.
- Fixed stale internal `DatabaseFromConfig[T]` comments →
  `db.BindFromConfig[T]`.

## [1.29.0] - 2026-07-03

### Added — built-in web security: CSRF enforcement + security headers

- **Security response headers are now on by default** — the framework
  applies `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, and
  `Referrer-Policy: strict-origin-when-cross-origin` to every app with no
  code and no config, matching Django/Rails/Laravel/Phoenix. Opt-in HSTS,
  Content-Security-Policy, Permissions-Policy, and COOP. This is a
  behavior change (new response headers on existing apps) but the three
  defaults are safe; set `[runtime.middleware.security] headers = false`
  to turn them off, or `frame_options = "-"` to omit one.
- **CSRF enforcement** (double-submit cookie) available as an opt-in
  built-in — `[runtime.middleware.security] csrf = true` (or
  `Config.Middleware.Security.EnableCSRF`). Safe methods mint a random
  token in a non-HttpOnly `csrftoken` cookie; unsafe methods must echo it
  in the `X-CSRFToken` header (or a `csrf_token` form field). Those names
  match the generated client SDK, so an existing frontend needs no change.
  Bearer/token-auth requests (an `Authorization` header) are skipped —
  they aren't CSRF-vulnerable. The cookie's `Secure` flag auto-derives
  from the request scheme, so dev over http works without config. CSRF is
  **off by default** because a nexus app is usually a token-authenticated
  API where CSRF is moot; enable it when you serve cookie/session-
  authenticated, server-rendered HTML forms (a template engine, or
  Inertia backed by session cookies).
- **Config, secure-by-default and zero-code:** the new
  `Config.Middleware.Security` field, populated from
  `[runtime.middleware.security]` in nexus.toml. No Go, no import.
- **New `extension/security` package** for the pieces the core path can't
  offer: a dashboard "Security" tab (`security.Plugin()`) and per-route
  middleware bundles (`security.NewCSRFMiddleware` /
  `NewHeadersMiddleware`) for apps that mix cookie- and token-auth routes.
  Global enforcement stays in the core so the middleware is never applied
  twice.
- **New `middleware/secure` package** holds the transport-neutral header
  and CSRF implementations shared by the core and the extension (deps:
  `httpx` + stdlib only).
- `nexus docs security` documents it.

### Added — `extension/storage`: file/object storage (local + S3 disks)

- **A filesystem/object-storage abstraction, the Go equivalent of Laravel
  Storage / Rails ActiveStorage / Django file storages.** Application code
  talks to one `Disk` interface (`Put` / `Get` / `Exists` / `Delete` /
  `Stat` / `List` / `URL` / `SignedURL`); the backend is chosen by config,
  so local-in-dev and S3-in-prod differ only by a `Config`.
- **Two backends, both dependency-free:**
  - **Local** — the OS filesystem under a root dir. Atomic writes
    (temp-file + rename) and path-traversal rejection.
  - **S3** — any S3-compatible store (AWS S3, MinIO, Cloudflare R2,
    DigitalOcean Spaces) spoken directly over HTTPS with **hand-rolled
    SigV4 signing — no AWS SDK is linked** (go.mod is unchanged), keeping
    nexus's zero-heavy-dep ethos. `SignedURL` returns a presigned GET;
    virtual-hosted and path-style URLs both supported via `Endpoint`.
- **Wired like a cache or database** — `storage.Bind[T]("name", build,
  opts…)` where `T` embeds `*storage.Manager`; injected into handlers and
  registered as a dashboard resource (new `resource.KindStorage`).
- `PutOption`s: `WithContentType`, `WithSize` (stream without buffering),
  `Public`. `nexus docs storage` documents it. The SigV4 signer is
  verified against AWS's published example vector.

### Added — auth: password hashing + credential login backends (Django-style, phase 1)

- **Pluggable password hashing** (`auth.Hasher` / `auth.Hashers`), the
  Django `PASSWORD_HASHERS` analogue — encoded strings are self-describing
  (`<id>$<payload>`) so a set verifies any member algorithm and **rehashes
  on login** when the stored hash is stale. Three shippers, **zero new
  deps** (`golang.org/x/crypto` + stdlib `crypto/pbkdf2`):
  - `auth.BCrypt()` — the default (predictable memory, cost 12).
  - `auth.Argon2id()` — memory-hard alternative.
  - `auth.PBKDF2()` — PBKDF2-HMAC-SHA256 at 600k iterations (Django interop).
  - `auth.DefaultHashers()` = bcrypt default + argon2id/pbkdf2 for verify.
- **Pluggable password policy** (`auth.PasswordValidator`), the Django
  `AUTH_PASSWORD_VALIDATORS` analogue: `MinLength`, `NotNumericOnly`,
  `NotCommon`, `NotSimilarToUser`, run via `auth.ValidatePassword(...)`;
  `auth.DefaultValidators()` gives a sensible baseline.
- **Credential login backends** (`auth.Backend` + `auth.Authenticate`),
  the Django `AUTHENTICATION_BACKENDS` analogue — backends are tried in
  order; the built-in `auth.ModelBackend` authenticates a `Password`
  credential against a pluggable `auth.UserStore` with a `Hashers` set
  (constant-timing on the unknown-user path to avoid enumeration). Ships
  an in-memory `auth.MemoryUserStore` for dev/tests; swap in any store
  (GORM, external API) by implementing three methods.
- Non-breaking: this fills in the *login* half around the existing
  token-`Resolver`/`Scheme` surface, which is unchanged. (Phase 2:
  sessions + login/logout; phase 3: per-object policies.)

### Changed — CI now gates formatting and lint

- **`gofmt` is enforced in CI.** The whole tree was reformatted with the
  Go 1.26 toolchain (130 files — doc-comment reindentation + trailing
  newlines, no logic changes), and a new `gofmt` CI job + `make fmt-check`
  target fail the build if any file drifts. Run `make fmt` to fix.
- **`golangci-lint` gate added** (`.golangci.yml`, pinned `v1.64.8`, run
  per module). The enabled set — `gofmt`, `govet`, `ineffassign`,
  `durationcheck`, `makezero` — is a **ratchet** like the coverage floor:
  it passes clean today and only guards against regressions. Tighten
  `.golangci.yml` as the tree is cleaned up (errcheck / unused / staticcheck
  / bodyclose / errorlint are noted as next candidates); never loosen it to
  make a red build green. `make lint` runs the same locally.

## [1.20.4] - 2026-06-19

### Fixed — wildcard route params now match gin's convention on every backend

- **`c.Param("rest")` for a `*rest` route again returns a leading-slash suffix
  on the stdlib and chi backends.** gin exposes a `*filepath` capture as
  `/app.js` (leading slash); after the router-seam migration the stdlib backend
  returned `app.js` (ServeMux's `{rest...}` drops the slash) and the chi backend
  returned `""` (chi stores the capture under the key `*`, so the original name
  missed entirely). Handlers that build a path from the capture — notably the
  dashboard's `"assets" + c.Param("filepath")` — resolved to `assetsapp.js` /
  `assets`, 404'd, and served assets with an **empty MIME type**, so browsers
  blocked the dashboard's own JS module (`/__nexus/assets/index-*.js`). The
  seam now normalizes the wildcard capture to gin's leading-slash form via the
  new `httpx.WildcardName` helper, so `c.Param` behaves identically on gin,
  chi, and stdlib. Named (`:id`) params are unaffected.

## [1.20.3] - 2026-06-19

### Fixed — `stdrouter` treated `GET /` as a catch-all, swallowing assets

- **Trailing-slash routes are now exact matches.** gin treats a registered
  route as an exact path (its catch-all is the `*rest` wildcard), but
  `net/http.ServeMux` treats any pattern ending in `/` as a *subtree* match. So
  a home-page route like `GET /` (e.g. `inertia.Page("GET", "/", …)`) silently
  became a catch-all that shadowed every unmatched `GET` path — including
  `GET /assets/*` — so the SPA's JS/CSS never reached the `ServeFrontend`
  `NoRoute` fallback and the page loaded with **no assets**. `stdrouter` now
  appends ServeMux's `{$}` end-of-path marker to trailing-slash routes
  (`/` → `/{$}`, `/admin/` → `/admin/{$}`), restoring gin's exact-match
  semantics. Wildcard (`*rest`) routes keep their subtree behavior, and
  `NoRoute` / `Static` register their patterns directly, so the intended
  catch-alls are unaffected.

## [1.20.2] - 2026-06-19

### Fixed — `stdrouter.Static` no longer panics next to a catch-all route

- **`Static` is now scoped to `GET`.** It previously registered its prefix
  method-less (`/media/`), which Go 1.22's `ServeMux` treats as ambiguous
  against an app's catch-all `GET /` (the static pattern has a more specific
  path but matches *more* methods, so neither is a strict subset) and panics at
  boot — e.g. an SPA frontend plus a `Static("/media", …)` upload dir. A static
  file server only serves GET/HEAD, and `ServeMux` serves HEAD off a GET
  pattern, so registering `GET /media/` keeps full behavior while making the
  static route a strict path-refinement of `GET /` — no conflict. (gin's radix
  router tolerated the overlap; the stdlib default did not.)

## [1.20.1] - 2026-06-19

### Added — form accessors on `httpx.Ctx`

- **`httpx.Ctx` now carries gin-compatible form helpers**, closing a gap from the
  router-seam migration where low-level handlers that read POST bodies had no
  neutral equivalent for gin's form methods: `PostForm`, `DefaultPostForm`,
  `GetPostForm`, `PostFormArray`, `FormFile`, `MultipartForm`, and
  `SaveUploadedFile`. They read `*http.Request` directly, so they behave
  identically on the stdlib, chi, and gin backends — no adapter changes. Empty
  string for a missing key; `GetPostForm`/`DefaultPostForm` distinguish
  present-but-empty from absent (the latter falls back to the default).

## [1.20.0] - 2026-06-19

### Changed — `ginrouter` is now its own module (gin out of the main graph)

- **`github.com/paulmanoni/nexus/httpx/ginrouter` is a separate Go module.** gin
  (and its sonic / golang-asm / goccy / validator / json-iterator tree) is no
  longer a dependency of the main `github.com/paulmanoni/nexus` module at all —
  the module graph drops from 182 to 161 modules. The default build was already
  gin-free at link time (v1.19.0); now it's gin-free at the `go.mod`/`go.sum`
  level too, so `go get github.com/paulmanoni/nexus` pulls none of gin's tree.
- **The import path is unchanged** (`.../httpx/ginrouter`); it just versions
  independently. To use the Gin backend, add the module explicitly:

  ```bash
  go get github.com/paulmanoni/nexus/httpx/ginrouter
  ```
  ```go
  nexus.Boot(nexus.WithRouter(ginrouter.New()))
  ```
- `stdrouter` (default) and `chirouter` remain inside the main module — chi has
  no transitive dependencies, so it costs nothing to keep bundled.

## [1.19.0] - 2026-06-18

### Added — Pluggable HTTP router (`httpx` seam)

- **The HTTP router is now pluggable behind `github.com/paulmanoni/nexus/httpx`.**
  Handlers and middleware see a transport-neutral `*httpx.Ctx`; the concrete
  router is an adapter selected at boot via `nexus.WithRouter(...)` (or
  `Config.Router`). Three backends ship:
  - `httpx/stdrouter` — **the new default**, Go 1.22 `net/http.ServeMux`, with
    **zero third-party router dependencies**. The default binary no longer links
    gin (or its sonic/golang-asm/goccy/validator tree).
  - `httpx/chirouter` — opt-in (`go-chi`).
  - `httpx/ginrouter` — opt-in; the only package that imports gin now.
- Chain execution (`Next`/`Abort`, panic recovery, error accumulation) lives in
  `httpx.Ctx`, so every middleware runs identically on any backend; the router
  only matches paths and returns params. App-level middleware wraps the whole
  mux (runs even on 404/405, e.g. CORS preflight); per-op middleware runs inside
  the matched route. Route strings keep the canonical `:id` / `*rest` syntax on
  every backend.

### Changed (BREAKING) — gin no longer in the public surface

- `App.Engine() *gin.Engine` → **`App.Router() httpx.Router`**.
- Low-level handlers that took a `*gin.Context` parameter now take **`*httpx.Ctx`**.
- `gin.H` → **`httpx.H`**. `AsRestHandler` factories return `httpx.HandlerFunc`.
- To keep gin, add `nexus.WithRouter(ginrouter.New())` and blank-import the
  adapter — selecting gin/chi pulls their dependency trees back into the build;
  the stdlib default links none.

## [1.18.1] - 2026-06-18

### Security — Client SDK

- **SDK routes now sit behind the introspection gate.** An explicit
  `Config.Client{Enabled: true}` mount previously served `/__nexus/client/*`
  (the manifest — a full API map — and the `.d.ts` type surface) to anyone,
  with no `introspection_networks` enforcement. The mount now reuses the same
  gate as the dashboard: open under `nexus dev` / `Introspection`, 404 to
  non-allowed peers in a locked-down production binary. Opt back out with
  `Config.Client.Unguarded` when you deliberately serve the runtime SDK to the
  public (prefer vendoring `sdk/` at build time via `nexus client --out`).
- **Token store defaults to in-memory.** `NexusClient` previously defaulted to
  `localStorageTokenStore()`, leaving bearer tokens readable by any XSS and
  persistent across reloads. The default is now `memoryTokenStore()`;
  persistence is opt-in. The Vue/React `useNexus()` composables likewise default
  to in-memory and switch to `localStorage` only when `VITE_NEXUS_TOKEN` is
  explicitly set.
- **CSRF double-submit for cookie-based strategies.** Under `cookie` / `chain` /
  `custom` auth the SDK now, on state-changing requests, reads a non-HttpOnly
  CSRF cookie and echoes it in a header so a cross-site post is rejected. No
  cookie set → no header, so apps without CSRF cookies are unaffected.
- **Login token location is declarable, not just guessed.** The SDK reads the
  token from a configured dotted path before falling back to the heuristic walk,
  removing the risk of picking up an unrelated `token` field.

### Added

- `auth.Config` gains `LoginTokenField`, `CSRFCookie`, and `CSRFHeader`, bridged
  into the SDK manifest's auth section so the generated/runtime client reads the
  token from the declared location and uses the matching CSRF pair. Empty fields
  fall back to framework defaults: `data.token`, `csrftoken`, and `X-CSRFToken`
  (the Django/Laravel convention), exposed as `client.DefaultTokenField`,
  `client.DefaultCSRFCookie`, and `client.DefaultCSRFHeader`.
- `client.AuthMeta` (+ `WithDefaults`, `Empty`), `Handler.SetAuthMeta`, and
  `App.SetClientAuthMeta` — the additive bridge carrying the above without
  changing the `Mount` / `SetClientAuthInfo` signatures.
- `Config.Client.Unguarded` — escape hatch for serving the runtime SDK publicly
  from a locked-down binary.
- `Manifest.Projected` — marks the stripped (non-`Public`) manifest so the SDK
  surfaces a clear "the server is serving the stripped manifest" error on an op
  miss instead of a cryptic "no op named X".

### Changed

- **Breaking (runtime behavior):** apps relying on cross-reload token
  persistence must now pass `tokenStore: localStorageTokenStore()` explicitly
  (or set `VITE_NEXUS_TOKEN` for the composables).
- **Breaking (runtime behavior):** apps that intentionally serve the runtime SDK
  from a production binary with introspection off must set
  `Config.Client.Unguarded = true`.
- The SDK's default CSRF cookie/header changed from the Angular convention
  (`XSRF-TOKEN` / `X-XSRF-TOKEN`) to the Django/Laravel convention
  (`csrftoken` / `X-CSRFToken`). Override via `auth.Config` or the `NexusClient`
  constructor (`csrfCookie` / `csrfHeader`).
