# frontend/deps — node-free frontend dependencies

A pure-Go (mostly) frontend dependency manager + bundler that turns
`import 'vue'` in a `.jsx` / `.tsx` / `.vue` file into a working
ES module bundle, without Node.js, npm, or `node_modules`.

```
$ nexus add vue
$ nexus add @vue-flow/core
$ nexus build
  → islands/Counter.js  (esbuild bundle, dep resolution via nexus.lock)
```

What's underneath:

```
your-project/
├── nexus.lock                         pin file (sha256-verified)
├── islands.src/                       JS/TS/JSX/Vue source
│   └── Counter.tsx
└── islands/                           emitted bundles (embedded in your binary)
    └── Counter.js

~/.nexus/cache/                        shared across every project on the machine
├── cas/<aa>/<bbbb...>                 content-addressed blobs
├── url/<urlhash>.meta                 URL → blob index
└── sfc-vue/<version>/                 cached @vue/compiler-sfc bundles
    └── compiler.bundle.js
```

## Architecture

Five layers, each independently testable, ordered from leaf to root:

| Package | What it owns |
|---|---|
| `store/` | Content-addressed disk cache. atomic writes, flock per-key, sha256-verified. `Get(url) → blob path + meta`, `Put(url, content, hash, meta)`. |
| `lockfile/` | `nexus.lock` reader/writer. Deterministic JSON output (sorted keys, stable formatting). |
| `fetcher/` | esm.sh HTTP client. Recursive — JS `import` + CSS `@import`/`url()` references chase transitively. sha256-verified storage. |
| `resolver/` | esbuild plugin (`OnResolve` + `OnLoad`). Bare imports → lockfile lookup → cached blob. Falls through for relative + unknown paths. Two-attempt query-strip lookup for esm.sh's mixed-query stub conventions. |
| `bundler/` | esbuild Build API wrapper. Wires the resolver + Vue SFC plugins, applies sensible defaults. |
| `sfc/vue/` (cgo+vue tag) | Vue SFC compiler via QuickJS hosting real `@vue/compiler-sfc`. Bootstrap fetches the compiler bundle once and caches it. |

Each package has its own tests; the bundler exercises the full
chain via an e2e test that loads a real package from a fake
registry through esbuild. There are also network-gated tests
(`-tags network`) against real esm.sh for: full Vue SFC compile
(`@vue/compiler-sfc@3.4.21`), `@vue-flow/core` bundle, and the
`lucide-vue-next` namespace-import size envelope.

## Trust model

esm.sh + integrity hashes, in this order:

**1. URL-pinning at fetch.** `nexus add vue` GETs `https://esm.sh/vue`. esm.sh redirects to `https://esm.sh/vue@3.4.21` — the exact version the registry serves at fetch-time. The resolved URL is what lands in `nexus.lock`. A later `nexus install` hits that resolved URL directly, never re-resolving (no version drift between machines or between fetches).

**2. sha256 integrity.** `Put` computes sha256 of the bytes; the caller may supply an expected hash and `Put` rejects mismatches. `nexus.lock` records `"integrity": "sha256-<hex>"` per entry. On install, the fetcher verifies the bytes match the lockfile's hash before they touch the store.

**3. esm.sh itself.** We trust esm.sh to serve the bytes we asked for (this is the same trust npm/pnpm/yarn place in the npm registry, plus or minus). The integrity hash defends against tampering between fetch and use; it does NOT prove the original publisher signed the package. If you don't trust esm.sh, run your own mirror — set `NEXUS_REGISTRY=https://your-mirror.example.com` and the fetcher uses it.

**4. The Vue compiler bundle.** First `.vue` build per machine fetches `@vue/compiler-sfc` from your registry, runs it through esbuild + our resolver, and caches the result at `~/.nexus/cache/sfc-vue/<version>/compiler.bundle.js`. The bundle's bytes are NOT separately integrity-checked beyond the fact that every input blob's hash was verified at fetch time. Bump the pinned Vue compiler version in `frontend/deps/sfc/vue/bootstrap.go` to invalidate the cache.

What we do NOT do:
- Cross-platform reproducibility checks (the lockfile pins bytes, not build environments)
- Signature verification (no GPG/sigstore on packages — esm.sh doesn't sign and there's no upstream of-record)
- Network sandboxing during `nexus install` (it CAN go to the registry; offline operation requires `nexus vendor` first)

## Cache layout

```
~/.nexus/cache/                        (or wherever NEXUS_CACHE points)
├── cas/                               content-addressed blob storage
│   ├── ab/cdef0123…                   one sha256, two-level shard
│   └── 7e/89abcdef…
├── url/                               URL-keyed metadata
│   ├── <urlhash>.meta                 JSON: {url, resolved_url, content_sha256, content_type, etag}
│   └── …
├── lock/                              per-URL flock sentinels (advisory)
│   └── <urlhash>.lock
└── sfc-vue/                           Vue compiler bundle cache
    └── 3.4.21/
        └── compiler.bundle.js         ~850 KB IIFE
```

**Two-level shard on `cas/`** keeps directory size manageable
even at 100K+ blobs; ext4 / HFS+ / APFS all handle a few thousand
entries per directory gracefully.

**Atomic writes everywhere.** Blobs and metadata both use the
temp-file-then-rename pattern within the same directory, so the
rename is one inode update guaranteed atomic by POSIX. A crash
mid-write leaves a `.tmp` file the next operation reaps.

**Per-URL flock.** Two concurrent processes (`nexus add` running
in one shell, `nexus install` in another) serialize on a
sentinel file under `cache/lock/`. Unrelated URLs don't block
each other. Shared locks for `Get`, exclusive for `Put`.

**Shared across projects.** Two projects on the same machine
depending on `vue@3.4.21` pay one download + one set of bytes
on disk. Cross-project reachability for `nexus gc` is opt-in
via `--keep <lockfile-path>`.

## CLI reference

| Command | What it does |
|---|---|
| `nexus add <spec>` | Fetch a package, write to `nexus.lock` + cache. Spec accepts `vue`, `vue@3.4.21`, `@vue-flow/core`, etc. |
| `nexus install` | Sync `~/.nexus/cache` to whatever `nexus.lock` pins. Fresh clones + CI use this. No-op when warm. |
| `nexus remove <spec>` | Drop entry from `nexus.lock`. Cache untouched until `gc`. |
| `nexus update [spec]` | Re-resolve specs against the registry, update `nexus.lock` with new resolved versions. No args = update everything. |
| `nexus vendor [--out ./vendor/nexus]` | Copy every blob the lockfile references into a project-local dir for air-gapped builds. Combine with `NEXUS_CACHE=./vendor/nexus`. |
| `nexus gc [--keep <lockfile>...]` | Mark-and-sweep over the cas tree. Removes blobs no URL mapping references (or, with `--keep`, no supplied lockfile references). |

### Environment variables

| | |
|---|---|
| `NEXUS_CACHE` | Cache root (default `~/.nexus/cache`) |
| `NEXUS_REGISTRY` | Base registry URL (default `https://esm.sh`) |

## Vue SFC support (opt-in, CGo)

`.vue` files require the QuickJS-backed Vue SFC compiler. Default
builds skip it entirely (pure Go, no CGo dependency). Opt in:

```bash
CGO_ENABLED=1 go install -tags vue github.com/paulmanoni/nexus/cmd/nexus@latest
```

With the `vue` tag, `nexus build` (or `nexus dev`) loads
`@vue/compiler-sfc` into an in-process QuickJS runtime and
compiles `.vue` files transparently. Without the tag, `.vue`
sources are rejected with a clear message at build time.

Why this works: QuickJS supports async generators natively, which
means `@babel/parser`'s feature-detection (a transitive dep of
`@vue/compiler-sfc`) evaluates correctly. Goja, the pure-Go JS
interpreter we tried first, doesn't run async generators, and
esbuild can't down-level the introspection expression cleanly.
The QuickJS path is the only one we've found that actually
compiles real Vue source against the real compiler.

## How the framework uses this for its own dashboard

`extension/dashboard/cmd/build-ui/main.go` is the framework's
internal build script. Reads `extension/dashboard/ui/package.json`
for the dep set, programmatically calls the fetcher for each,
bootstraps the Vue compiler, runs the bundler, writes
`index.html`. Output lands at `extension/dashboard/ui/dist/`
where the framework's `//go:embed` ships it inside every nexus
binary.

The script is build-tagged `cgo && vue` — only framework
maintainers + CI rebuild the dashboard, so a CGo requirement at
that level is fine. Users of the framework get a pre-built
dashboard via the embedded FS without any toolchain themselves.

Run it from the repo root:

```bash
CGO_ENABLED=1 go run -tags='cgo vue' ./extension/dashboard/cmd/build-ui
```

## What this is NOT

- **A general JS runtime.** We host enough of one to compile Vue
  SFC sources, nothing else. SSR is not a goal.
- **A vite replacement.** vite is a dev server + HMR + plugin
  ecosystem; this is just enough to bundle JS/TS/JSX/Vue at
  build time. Dev mode uses esbuild's native watcher (no HMR).
- **A tree-shaking miracle.** Where the upstream package's
  output shape blocks tree-shaking (e.g. `lucide-vue-next`'s
  top-level `Object.defineProperty` namespace object), we ship
  the whole namespace. Per-icon imports work; auto-rewriting
  bare-named imports to deep imports is a follow-up.
- **An npm-protocol implementation.** We talk esm.sh's URL
  shape, not the npm registry protocol. Self-hosted alternatives
  must speak esm.sh's URL conventions (esm.sh itself, or a
  reverse-proxied mirror).

## Limitations + known gaps

| | |
|---|---|
| `.vue` requires CGo | by design — see above |
| Tree-shaking through namespace imports | upstream output-shape issue (lucide etc.) |
| Sub-path entries in `nexus.lock` | collapses to one entry per `<spec>@<version>`. Multi-sub-path packages need each sub-path declared explicitly today. |
| Source-URL inheritance vs query | resolver tries with-query first, falls back to query-stripped (fixed in 23cf30e; vue-flow exposed the case) |
| `nexus dev` watch on a Vue project | works for JS/TS; Vue SFC re-compiles incur the full Goja worker dance each time |

## Source layout reference

```
frontend/deps/
├── store/
│   ├── store.go              content-addressed cache primitives
│   ├── store_test.go
│   └── lock.go               flock sentinel helpers
├── lockfile/
│   ├── lockfile.go           nexus.lock read/write
│   └── lockfile_test.go
├── fetcher/
│   ├── fetcher.go            HTTP client, recursion, dedup
│   ├── fetcher_test.go
│   ├── imports.go            ExtractImports (JS)
│   ├── imports_test.go
│   ├── css_imports.go        ExtractCSSImports (@import + url())
│   └── css_imports_test.go
├── resolver/
│   ├── resolver.go           esbuild plugin
│   └── resolver_test.go
├── bundler/
│   ├── bundler.go            Build API wrapper
│   ├── bundler_test.go       e2e
│   └── lucide_test.go        network-tagged size-envelope pin
└── sfc/vue/                  (build-tagged cgo)
    ├── compile.go            QuickJS worker
    ├── adapter.js            @vue/compiler-sfc bridge
    ├── bootstrap.go          one-time bundle fetch + esbuild
    ├── plugin.go             esbuild plugin
    ├── vueflow_test.go       network-tagged dashboard-shape proof
    └── testdata/
        └── fake-adapter.js   hand-rolled stub for unit tests
```
