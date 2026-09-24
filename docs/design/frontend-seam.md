# Seamless nexus ↔ Vite

Status: **in progress** (Stages 1–3 built) — decision taken 2026-09-24: Vite is the only frontend
engine; viteless is retired from nexus (the repo lives on independently).
Node/npm are dev- and build-time requirements; the runtime stays one Go binary
with `web/dist` embedded.

## Diagnosis

A full audit (this doc's evidence) found **27 distinct facts** shared between
the Go side and the frontend — the project dir, the dist root, the HMR origin,
the app origin, the entry module, the manifest, the shell, page names, props
types, the SDK location, reload signals, readiness — and almost every one
travels by **scanning, probing, or parsing output** rather than by being told:

- `nexus dev` AST-scans the app's source for the `ServeFrontend(...)` literal
  (cmd/nexus/dev_detect.go:63) and decides Inertia mode by running
  `go list -deps` and grepping for the import (dev.go:229).
- The app's port is regex-scraped from its own stdout (dev.go:1107); the HMR
  origin is scraped from Vite's `Local:` line with a hardcoded 5173 fallback
  after 30s (viteless realvite.go:77-96); when 5173 is taken the native engine
  silently binds a random port (viteless dev.go:267-279).
- The dev entry is guessed as `src/main.ts` (extension/inertia/inertia.go:214);
  the production entry is guessed by globbing top-level `*.ts` — which is the
  blank-SPA bug (docs/design/blank-spa-build-bug.md).
- Under real Vite the proxy resolver and `[env]` bridge are silently dropped
  (realvite.go:48-58), which is why the gateway hardcodes `:8080` in
  vite.config.ts.
- **Inertia in production depends on a Vite manifest that neither default
  toolchain writes.** The native build writes none; the vite scaffold omits
  `build.manifest: true`; the engine renders an empty head with no warning
  (inertia.go:160-162). Both scaffold paths ship blank pages.
- The Inertia shell ignores `index.html` entirely (shell.go:16-35), so the
  gateway lost its title and stylesheet links and patched them back with
  response-rewriting middleware (ThemeHead). Three uncoordinated reload
  systems fire on one `.vue` save.
- Pages appear in the SDK as bogus REST calls; `*PageProps` interfaces are
  generated but nothing links them to components, so all 114 of the gateway's
  `defineProps` are hand-typed; component names are free strings with a silent
  NotFound fallback.
- The running binary writes `web/sdk` into the working directory **on every
  boot, production included** (obs_integration.go:166-172).

One sentence of diagnosis: **the integration is a set of cooperating guessers;
nothing owns the contract.**

## The design: the plugin tells, the app reads

Adopt the model Laravel proved at scale: a first-party Vite plugin owns the
handshake, and the backend never guesses.

```
        dev                                   prod
┌──────────────┐  writes  ┌─────────┐   ┌────────────┐  emits   ┌──────────────────┐
│ vite dev     │ ───────► │ hot file│   │ vite build │ ───────► │ .vite/manifest.json │
│ (nexus plugin)│          └────┬────┘   │ (nexus plugin)│        └────────┬─────────┘
└──────────────┘               │        └────────────┘                 │
                        Go app reads it                        embedded, Go reads it
                 → asset tags point at the real                → hashed asset tags,
                   HMR origin, whatever port                     version = manifest hash
```

### 1. The hot file (dev)

`nexus-vite-plugin` (already shipped in `web/sdk`) gains a `configureServer`
hook: when `vite dev` starts it writes `<outDir>/.vite/nexus-hot.json`:

```json
{ "version": 1, "origin": "http://127.0.0.1:5173", "base": "/",
  "entries": ["src/main.ts"], "pid": 47487 }
```

and removes it on shutdown. It sits beside the manifest `vite build` writes
because the build output directory is the **one path both sides already
share** — Vite's `build.outDir` and the root passed to `ServeFrontend` — so
locating it needs no new convention and no guess about which directory is the
Vite root. A build into the same outDir restores a live dev server's file after `emptyOutDir` wipes it; a stale one is removed. The Go side reads it only
from disk, never from an embed, and follows it only while the dev server it
names is **live** — its pid is running, or, when the pid is dead or unknown
(Vite in a container sharing the volume), its origin answers
`@vite/client`. A file left by a killed dev server, which is routine because
`nexus dev` stops Vite with SIGKILL, therefore reads as absent and is logged
once; it neither errors nor redirects pages. Liveness is what protects a
deployment: `nexus new` writes `environment = "development"` into nexus.toml
and deployments ship it, so the environment check that also gates the file
cannot be the safety on its own.

Contract implementation: `internal/vitehot` (schema, reader, the enable rule),
one instance per app via `App.ViteHot()`, created by `ServeFrontend`. `ServeFrontend` and the Inertia engine stat the
file (cached, revalidated cheaply): present → emit
`<script type="module" src="<origin>/@vite/client">` and
`<origin>/<entry>`; absent → serve the embedded build via the manifest.

What this kills outright: `NEXUS_VITE_DEV`, the `Local:` stdout scrape, the
5173 folklore (the port in the hot file is whatever Vite actually bound), the
`go list -deps` Inertia detection (mode no longer changes the dev topology),
and — the real seamlessness win — **the orchestrator requirement**. `npm run
dev` in one terminal and `go run .` in another is a fully working setup;
`nexus dev` becomes a convenience that supervises both, not the glue that
makes them find each other.

### 2. One origin for the browser

The browser always opens the Go app. The Vite dev server serves assets only
(the plugin sets `server.cors` and `server.origin` so cross-origin module
loading works). SPA mode stops living on the Vite port: in dev, `ServeFrontend`
serves `index.html` transformed to reference the hot origin. Consequences:

- The vite.config proxy block for `/__nexus`, `/graphql`, `/oauth`, `/ws` is
  deleted — there is nothing to proxy, the browser is already on the Go origin.
- viteless's reverse proxy, the app-port stdout regex, and the partial toml
  parse that fed it all go with it.
- "Which URL do I open?" has one answer in every mode: the app's.

### 3. The manifest is non-optional (prod)

The plugin's `config` hook forces `build.manifest: true` and registers the
declared entry, exactly as laravel-vite-plugin does. The engine reads
`.vite/manifest.json` (current code already does, version.go:38-73) — but an
Inertia render finding **no manifest becomes a loud error page in dev and a
logged error in prod**, never a silent empty head. The entry is declared once,
in `vite.config.ts`:

```ts
nexus({ input: 'src/main.ts' })
```

and flows to the hot file, the manifest, and the build. The Go-side `Entry`
guess and the top-level `*.ts` glob both die. Asset URLs honour Vite `base` /
`FrontendAt` instead of hardcoded `/`.

### 4. index.html is the one shell

The Inertia engine stops synthesising its own document. `index.html` (disk in
dev, embed in prod) is the template: the engine injects the asset tags and
replaces the app mount with `<div id="app" data-page=…>`. The same file serves
SPA NoRoute. `Config.Head` remains as an additive escape hatch. The gateway's
ThemeHead response-rewriting middleware and its `devassets` module (serving
`web/public` in dev — the plugin/dev origin handles that) both become
deletable.

### 5. Typed pages and shares

`inertia.Page("GET", path, "Admin/Employers", handler)` already carries the
component name and the props type into the registry. Generate, alongside the
SDK:

```ts
// web/sdk/pages.d.ts
interface NexusPageProps {
  'Admin/Employers': { employers: EmployerRow[]; filters: Filters }
  ...
}
// usage in a page component
const props = definePage<'Admin/Employers'>()   // fully typed
```

plus typed shared props from `Share`/`ShareScoped`'s value types (the `can` /
`perms` / `features` casts in the gateway become checked). The plugin, which
already globs `Pages/**/*.vue`, validates registered component names against
existing files at dev time — a typo becomes a build-time error instead of a
silent NotFound render. Pages stop being emitted as fake REST calls in the SDK.

### 6. One SDK generator, one location, dev-only writes

`web/sdk` is the location; the plugin's default matches (today Go writes
`web/sdk` while the plugin defaults to `src/sdk`). The boot-time dump runs
**only under NEXUS_DEV** — a production binary writing into its working
directory on every start is a bug, not a feature. `frontend.Plugin`'s parallel
`src/__nexus` codegen and `nexus generate frontend`'s overlap fold into this
one path.

### 7. nexus dev / build / new after the change

- `nexus dev`: supervises `vite` as a child when `web/package.json` exists;
  reads the hot file like everyone else; the AST scan, import scan, embed-stub
  special cases for the manifest, and `--dist` all shrink or disappear. Go
  rebuilds stay build-then-swap, untouched.
- `nexus build`: `npm ci`(when needed) + `vite build` + `go build`. The
  vestigial second embed in `embed_gen.go` goes.
- `nexus new`: scaffolds `package.json` + `vite.config.ts` with the plugin;
  `--tooling` is deprecated (one answer). `npm install` returns to the README
  as a real, correct step.
- Retired: the viteless dependency from cmd/nexus, `--frontend-cmd` (already
  ignored), the dead `islands.src` layer and `loadViteEnv`, the half-alive
  `NEXUS_FRONTEND_DIR` (honoured everywhere or removed).

## Staging — the gateway stays green at every step

1. **Handshake**: plugin hot file + manifest enforcement + engine/ServeFrontend
   reading them; `NEXUS_VITE_DEV` kept as fallback. Immediate wins: real port
   truth, loud missing-manifest, gateway deletes its proxy block.
2. **Shell**: index.html as the single template; base-aware asset URLs.
   Gateway deletes ThemeHead's rewriting and devassets.
3. **Types**: pages.d.ts + typed shares; SDK consolidation; dev-only dump.
4. **Vite-only**: build via npm/vite, dev drops viteless, scaffold rewrite,
   legacy removal, CLAUDE.md/docs rewrite.

## Recorded but out of scope here

- viteless bug root causes (entry glob, silent `:0` fallback, sidecar EOF) are
  in docs/design/blank-spa-build-bug.md; fixes are HELD per decision — the
  repo has uncommitted SSR work, and nexus is leaving the engine anyway.
- Gateway hygiene found during the audit: the committed `web/dist/index.html`
  references assets that do not exist; AGENTS.md points at `web/src/sdk/`
  (actual: `web/sdk/`); `sessionGuard.ts` and `vite.config.ts` contradict each
  other about `filter:'usage'`; the Dockerfile installs `nexus@latest` while
  go.mod pins a version.

## Stage 1 — as built

Verified end to end against real Vite 6.4.3: a scripted run (hot-file port,
page tags, CSS asset origin, SPA fallback, hot-file 404, restart on a new port,
clean and hard shutdown, production build and caching) and a real browser
showing the page rendered on the Go origin with cross-origin HMR updating a
component in place. Three decisions changed on contact with a real machine:

- **The origin is the bound address, not `localhost`.** On the development
  machine another project's Vite held `127.0.0.1:5173`; this one bound
  `[::1]:5173` and Vite reported both as `localhost:5173` — a name that reaches
  either server depending on how the client resolves it. The plugin now writes
  the socket's literal address (`http://[::1]:5173`); `resolvedUrls` is only a
  fallback.
- **A manifest is proof of a build.** `nexus({ input })` makes `vite build`
  emit no `index.html`, and `emptyOutDir` removes any stub, so "index.html
  missing" can no longer mean "never built". Boot rule: manifest → boot
  (unknown routes 404); nothing built in development → placeholder; nothing
  built in production → fail fast. Development must not fail fast: it is
  exactly the `npm run dev` + `go run .` setup this design promises.
- **Caching is derived from the build.** `immutable` requires both a manifest
  entry and a content-hashed name (a config such as `entryFileNames:
  '[name].js'` produces unhashed output); everything else revalidates with an
  ETag, and the production shell is `no-cache` + ETag rather than `no-store`.
  The manifest parser moved to `internal/vitemanifest`, shared by inertia and
  `ServeFrontend`.

Carried into later stages: the three reload systems still coexist (fixed in
Stage 2); an app that passes its bundle only through `inertia.Config.Frontend`
gets no hot-file support because the reader is created by `ServeFrontend`.

## Stage 2 — as built

Built on branch `stage2-shell` together with the fixes from an adversarial
review of Stage 1 (one high, five medium, all reproduced), and verified with a
27-check scripted run against real Vite 6.4.3 (local install) and a real
browser under `nexus dev`: a `.vue` edit hot-updated in place with no reload
and JS state kept; a `.go` edit reloaded the page once, ~4s later, after the
rebuilt binary was serving; a `public/` file loaded through the Go origin; the
page kept `index.html`'s title and stylesheet.

- **Pages render into `index.html`.** `App.FrontendDocument` hands the engine
  Vite's transformed page in dev and the built page in production; the engine
  puts `data-page` on the mount (replacing a hard-coded one), keeps everything
  else byte-for-byte, and adds no asset tags — the document already carries
  them. A module-only build (`nexus({ input })`, no `index.html`) is the one
  case that still gets a synthesised document, now with tags under
  `App.FrontendMount()` rather than `/`. The mount is found by a small tag
  scanner that is not fooled by ids in comments, scripts or attributes. With a
  CSP nonce, the template's own script/link/style tags are stamped too, or a
  strict-CSP app would lose its scripts.
- **The reload shim reloads for a new process, not for file writes.** Every
  process has a boot ID; a reconnect that sees a different one reloads — after
  the new binary serves. While a dev server is live, file changes never reload
  (Vite owns them); `.go` saves never reload on the file event. The shim also
  reloads once when the dev server starts, stops, or comes back on a different
  origin — pages would otherwise keep loading modules from a dead port. SPA
  pages now carry the shim too.
- **Review fixes.** `public/` files are proxied to Vite for loopback clients
  (never the LAN: that would expose Vite's source and `/@fs`) that name a
  loopback host and come through no proxy (a local tunnel makes every visitor
  loopback; a rebinding page arrives under its own name), and never for Vite's
  own routes (`/@…`, `/__…`, `node_modules`); a stale hot file
  reads as absent after a liveness probe (pid, else the origin answering), and
  liveness — not `environment = "development"`, which scaffolds ship — is what
  protects a deployment; boot leniency needs `nexus dev` or a live dev server;
  `immutable` needs an exact 8-character Vite hash with a digit or capital,
  settled by the manifest's own names; a build restores a live dev server's hot
  file after `emptyOutDir`; CORS allows loopback, `*.localhost`, `*.test`, this
  machine's addresses and `nexus({ appOrigin })`, and a `--host` bind names the
  network address; `/.vite/` is never served; hot-derived values are validated
  and escaped.

Carried into later stages: `nexus dev` stops real Vite with SIGKILL (inside
viteless), so the plugin cannot clean up — harmless now, but Stage 4's
Vite-driving `nexus dev` should send SIGTERM. ThemeHead-style per-request head
content (a theme only for some routes) still needs a per-request `Config.Head`.
The root-package flake was an older worker test reading a status before it was
recorded; it now polls (2,000 clean runs under `-cpu=4`).

## Stage 3 — as built

Verified end to end on a scratch app: a Go binary with three `inertia.Page`
routes and two typed shares, run with `environment = "development"`, wrote
`web/sdk` and merged `web/tsconfig.json`; against those generated files
`vue-tsc --noEmit` passed, and failed (exit 2) on a page prop misused and on a
shared prop read with the wrong type; `vite build` emitted the right runtime
props (`users: { type: Array, required: true }`, an `inertia.Prop` field as
`{ type: null, required: false }`); a page registered in Go with no `.vue` file
failed the build naming it; the same binary with `environment = "production"`
wrote nothing and left `tsconfig.json` untouched.

- **Pages and shares are typed from Go.** `inertia.Page` stamps
  `registry.PageTag` (via the new exported `nexus.Tag`); the manifest carries
  `endpoints[].page` and `sharedProps`; `client.d.ts` declares
  `NexusPageProps` and `NexusSharedProps`, and pages no longer appear as REST
  calls in either generator. The documented form is
  `defineProps<NexusPageProps['Users/Index']>()` — Vue's SFC compiler
  resolves an indexed access through imports and re-exports, but not a
  generic helper, so no `definePage<'X'>()` exists. An untyped page is
  `{ [key: string]: unknown }` (Vue cannot resolve `Record<…>` there).
  Typed shares are `ShareScoped[T]` and the new `ShareTyped[T]`; `Share`
  stays untyped and rides an index signature.
- **`usePage().props` is typed** through `inertia.d.ts`, a global
  augmentation of `@inertiajs/core`'s `InertiaConfig.sharedPageProps`,
  referenced from `client.d.ts` and added to an existing tsconfig `include`
  (a component that only calls `usePage()` imports nothing that would load
  it). Written only when there are pages or typed shares, and removed when
  there no longer are.
- **`nexus-client` resolves.** The tsconfig merge maps it to `client.d.ts`
  (TypeScript will not swap declarations in for a mapped `.js`), the plugin
  aliases it to `client.js` for Vite, and no `baseUrl` is added any more.
- **One location, development only.** The dump runs under `nexus dev` or
  `environment = "development"` (the hot-file rule) and never in a
  production binary; `client.Off` is an explicit "no dump" the frontend
  defaults no longer overwrite; the plugin reads `web/sdk` by default. The
  frontend dir is found by any Vite config extension, not only `.ts`.
- **Component names are checked.** `nexus({ pages })` (default
  `src/Pages`) warns in `vite dev` and fails `vite build` for every
  registered component with no file, matched case-exactly.

Deferred to Stage 4: folding `frontend.Plugin`'s `src/__nexus` codegen and
`nexus generate frontend` into `web/sdk` (its dead in-process driver is gone;
the CLI paths remain), and pinning `typescript ~6.0` in scaffolds (vue-tsc 3.3
crashes on TypeScript 7). A component rendered by routes with different props
types gets a union, which Vue merges into all-required props — a dev-only
"missing required prop" warning on the routes lacking a field. Not verified:
the augmentation under pnpm's strict layout, where `@inertiajs/core` may not
resolve from `web/sdk`.
