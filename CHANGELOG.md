# Changelog

All notable changes to nexus are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
