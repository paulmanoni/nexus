# Changelog

All notable changes to nexus are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
