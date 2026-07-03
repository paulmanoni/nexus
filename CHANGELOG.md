# Changelog

All notable changes to nexus are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
