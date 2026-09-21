# `nexus build` ships a blank SPA

Status: **open, reproduced at `v1.59.1` + `main`**. Not fixed — reported only.
Severity: high. Only reachable in production; `nexus dev` is unaffected.

## Reproduction

```
nexus new spacheck --frontend vue --yes
cd spacheck && go mod tidy
nexus build
```

Build reports success (`built 1 entry`, `built spacheck`, exit 0). The bundle is:

```
web/dist/index.html          297 B
web/dist/viteless-env.d.js   236 B
web/dist/viteless-env.d.js.map  93 B
```

`web/src/main.ts` and `web/src/App.vue` are never bundled. The whole
production "app" is the compiled ambient type-declaration file:

```js
// web/dist/viteless-env.d.js
if(typeof globalThis.__VUE_PROD_DEVTOOLS__==="undefined"){…}
```

## The mechanism

The source HTML is correct and names the real entry:

```html
<!-- web/index.html -->
<script type="module" src="/src/main.ts"></script>
```

The emitted HTML has been rewritten to point at the declaration file:

```html
<!-- web/dist/index.html -->
<script type="module" src="/viteless-env.d.js"></script>
```

So entry resolution is not following `index.html`'s `<script src>`. It is
picking up `web/viteless-env.d.ts` — a sibling of `index.html`, matching
`*.ts`, and containing no runtime code — and treating it as *the* entry. The
rewrite of `index.html` is downstream of that choice, which is why the output
is self-consistent and the build sees nothing wrong.

Vue never mounts. The served page is blank.

## Why it escapes notice

`nexus dev` serves through the HMR server, which resolves `/src/main.ts`
correctly, so the app works throughout development. The bundle is only exercised
by `nexus build`, and the build exits 0. The first symptom is a blank page in
production.

## Where the fix belongs

Entry resolution is viteless's, not nexus's — most likely its entry discovery
globs `web/*.ts` instead of parsing `index.html`. Two candidate fixes, not
mutually exclusive:

1. **viteless** (`~/Documents/personal/viteless`): derive the entry from
   `index.html`'s `<script type="module" src>`, which is Vite's own contract, and
   exclude `*.d.ts` from entry candidates in any case — a declaration file is
   never an entry.
2. **nexus**, as defence in depth: `nexus build` should fail rather than report
   success when the emitted HTML references no module built from `src/`. A build
   that produces a bundle with no application code in it is not a green build.

Moving or renaming the scaffold's `viteless-env.d.ts` would mask this specific
case, but any user `.d.ts` beside `index.html` would trip it again, so it is not
a fix on its own.

## Two adjacent findings, same session

**The HMR port is random and contradicts every document.** Six consecutive
`nexus dev` runs bound 63924, 64001, 64144, 64223, 64309, 64420. The scaffold's
README, `nexus new`'s own next-step output, `nexus docs`, and CLAUDE.md all
promise `5173`. Either the port selection regressed or all four documents are
wrong; the intent appears to be 5173.

**Every clean run warns.** A pristine scaffold with Node on PATH prints
`viteless: node config eval failed (sidecar handshake: EOF); falling back to the
zero-Node evaluator` on both `dev` and `build`. It is unactionable, and it trains
a newcomer to ignore `[web]` output — including the real errors above.

**`nexus build` dumps ~2.5 KB of base64 to the terminal.** It echoes the full
`go build` line, including `-ldflags -X …embeddedConfigB64=<the entire nexus.toml,
base64>`. Worth truncating the echoed command.
