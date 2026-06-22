# nexus decorator-form registration — design

Goal: let handler/DI registration be expressed as `//@` annotations on the
functions themselves, eliminating hand-maintained `nexus.Module(...)` wiring
lists — **while staying purely additive**. Decorator-form and explicit
registration produce identical `nexus.Option`s, share one DI container, and
either can be opted out of at any granularity.

This mirrors the seams already in the framework (the `httpx` router seam, the
`di` container seam): a generic engine (`deco`) feeds a nexus-specific adapter,
and the default `go build`/`go install` binary stays plain Go.

## Components (who owns what)

```
github.com/paulmanoni/deco            generic //@ transpiler/scanner
  • parser for //@ comments             ← ADD: deco.Scan() (structured hits)
  • Generate / Overlay / Transform      (existing wrap/codegen modes)
  • decorators.Func/FuncValues          (runtime wrappers — optional, unused here)
        │ deco.Scan(dir) → []Hit{Func, Keyword, Args, Pos}
        ▼
github.com/paulmanoni/nexus/internal/handlergen   nexus-specific emitter
  • Hit[] → Go source: func init(){ decorate.Rest(...); decorate.Provide(...) }
        │ emits init() calls
        ▼
github.com/paulmanoni/nexus/decorate  runtime contract  (IMPLEMENTED)
  • decorate.Rest/Query/Provide/WS/Worker/… → record → nexus.As*/Provide
  • decorate.Module(name) drains → one nexus.Option
  • ZERO build-time deps; plain Go the generated init() calls invoke
        ▲
github.com/paulmanoni/nexus/cmd/nexus  CLI (toolchain only)
  • nexus generate handlers → scan + emit COMMITTED *_gen.go
  • nexus dev               → scan + emit OVERLAY (temp, no churn)
  • nexus build             → nexus generate, then go build
```

**Boundary that keeps it clean:** `deco` never learns about nexus (it just
surfaces annotations); nexus owns the `//@rest → decorate.Rest` mapping. `deco`
is a dependency of the **CLI only** — the compiled app links just `decorate`
(plain Go), never deco. Same shape as keeping fx out of the default build.

## The pipeline — one scanner, one emitter, two sinks

```
  source.go (//@rest, //@provide on funcs)
        │  deco.Scan(pkgDir)
        ▼
   []Hit{Func, Keyword, Args, Pos}
        │  handlergen.Emit(hits)
        ▼
   generated Go:  func init(){ decorate.Rest("GET","/x", NewX); decorate.Provide(NewSvc) }
        ├── nexus dev   → OVERLAY temp dir + `go run -overlay`   (no committed file, source-mapped)
        └── nexus build / generate → COMMITTED handlers_gen.go   (go build / go install see it)
```

### Why two sinks — the invariant that drives the design

`go install pkg@v` and bare `go build` **never** run a generate step or an
overlay; they compile committed source as-is. So the committed `*_gen.go` is the
**source of truth** that keeps nexus `go install`-able — the property the v1.x
line has deliberately preserved. The overlay is a **dev-only accelerator** (zero
file churn while iterating); it is never the sole path, because an overlay-only
design makes `go build` silently drop every annotated route.

CI guards drift with `nexus generate handlers --check` (regenerate, fail if the
committed file changed) — the same discipline protobuf/sqlc/templ/mocks use.

## Auto-wiring — the app writes nothing but annotations

The app never calls a drain function or imports handler packages. `main` is just:

```go
func main() { nexus.Boot() }   // or nexus.Run(cfg, …explicit opts…)
```

Three pieces make that work:

1. **Auto-drain.** `decorate.Register("pkg", …opts)` (emitted into each annotated
   package's `init()`) buffers a per-package group. `decorate.init()` registers
   its `Drain` with nexus core's deferred-options hook (`RegisterDeferredOptions`,
   decorate→nexus only — no cycle); `nexus.Boot`/`Run` auto-includes the drained
   `nexus.Module("pkg", …)` groups. No `decorate.Module(...)` call.
2. **Import aggregator.** Go only compiles imported packages, so `nexus generate`
   emits `<mainpkg>/nexus_imports_gen.go` blank-importing every annotated package
   — pulling them into the build (and running their `init()`) with no import the
   app author writes. In dev it's overlay-injected (no committed file); for
   `go build`/`go install` it's committed (those run no overlay).
3. **Per-package modules.** The emitter groups each package's registrations under
   its own package name (`decorate.Register("notes", …)`), so the dashboard's
   module grouping is correct automatically. Handlers in the `main` package (or
   an empty/odd name) fall back to the default module `"app"` so they still group
   under a sensible dashboard node rather than a `"main"` one.

So: dev = zero files/imports/churn (overlay injects gen files + aggregator);
`go build`/`go install` = the same committed artifacts. Proven end-to-end in
`examples/notes` and `examples/inertia` (import-free mains; `nexus dev` serves
with no committed files).

## User-app layout

```
myapp/
├── main.go            nexus.Boot(decorate.Module("app"), …)
├── handlers/
│   ├── users.go       YOU write: //@rest GET /users/:id  func NewGetUser(...)
│   └── handlers_gen.go GENERATED + COMMITTED: init(){ decorate.Rest(...); … }
└── web/ …             (frontend, unchanged)
```

```go
// handlers/users.go — authored
//@provide
func NewUsersService(app *nexus.App) *UsersService { ... }

//@rest GET /users/:id
func NewGetUser(s *UsersService, p nexus.Params[GetArgs]) (*User, error) { ... }

//@mutation
//@auth Requires("ADMIN")
func NewCreateUser(s *UsersService, p nexus.Params[NewUser]) (*User, error) { ... }
```
```go
// handlers/handlers_gen.go — generated, committed, reviewable
// Code generated by `nexus generate handlers`. DO NOT EDIT.
package handlers

import (
	"github.com/paulmanoni/nexus/decorate"
	"github.com/paulmanoni/nexus/extension/auth"
)

func init() {
	decorate.Provide(NewUsersService)
	decorate.Rest("GET", "/users/:id", NewGetUser)
	decorate.Mutation(NewCreateUser, auth.Requires("ADMIN"))
}
```

## Custom decorators for extensions

An extension ships its OWN `//@` decorator with NO new code — it reuses its
existing `Option`-returning registrar (the universal nexus pattern):

- A QUALIFIED keyword `//@pkg.Func args…` generates
  `decorate.Record(pkg.Func(args…, Fn))`. Because every nexus registrar returns
  a `nexus.Option`, `inertia.Page`, `nexus.AsRest`, etc. work directly — the
  codegen records the returned option into the same `decorate.Module(...)` drain
  as the built-ins.
  ```go
  //@inertia.Page "GET" "/users" "Users/Index"
  //   → decorate.Record(inertia.Page("GET", "/users", "Users/Index", NewUsers))
  ```
- `pkg` is imported from the annotated file (handlers using the extension —
  e.g. `inertia.Redirect`/props — import it naturally). Each whitespace-
  separated annotation token is a distinct argument (comma-joined); an argument
  can't contain a space.
- Custom decorators don't take `//@auth`/`//@use` modifiers (the registrar
  defines its own surface).

**Branding with the extension's icon.** `nexus.WithIcon(name)` is a cross-
transport per-op option (mirrors `HideFromDashboard`) that stamps
`registry.IconTag` on the endpoint; the dashboard renders it (Inspector endpoint
rows + the per-endpoint default glyph fallback). An extension bakes its icon
into its registrar so every endpoint it registers is branded:
`inertia.Page` adds `nexus.WithIcon(inertia.Icon)` ("app-window"). Plugins
themselves carry an icon too (`extension.Plugin.Icon` → `PluginInfo.Icon`,
default Puzzle).

`decorate.Record(o nexus.Option)` is the public hook; `transpiler.Scan` returns
qualified keywords and the CLI keeps built-ins ∪ any dotted keyword.

Proven end-to-end: `examples/inertia` (`//@inertia.Page` → Inertia-protocol
pages, branded `app-window`) and `examples/notes` `widgets.Panel`
(`//@widgets.Panel "/stats"` → `GET /widgets/stats`, branded `layout-panel-top`).

## Annotation catalog → decorate.*

```
//@provide                       → decorate.Provide(fn)
//@supply                        → decorate.Supply(v)
//@rest <METHOD> <PATH>          → decorate.Rest(method, path, fn, opts…)
//@query   /  //@mutation        → decorate.Query/Mutation(fn, opts…)
//@subscription                  → decorate.Subscription(fn, opts…)
//@ws <PATH> <TYPE>              → decorate.WS(path, type, fn, opts…)
//@worker <NAME>                 → decorate.Worker(name, fn)
//@auth Required | Requires("X") → appended as an opt: auth.Required()/auth.Requires("X")
//@use <expr>                    → appended as an opt
```
Auth/middleware annotations compile to existing option values — no new runtime
concept, only placement next to the handler.

## deco addition (small, generic)

```go
// in github.com/paulmanoni/deco — stays nexus-agnostic
type Hit struct {
	Pkg, File string
	FuncName  string         // the annotated function
	Keyword   string         // "rest", "provide", "auth", …
	Args      []string       // raw tokens after the keyword
	Pos       token.Position
}
func Scan(dir string, keywords ...string) ([]Hit, error)
```

## CLI surface (additions)

```
nexus generate handlers [./...]   scan + (re)write *_gen.go;  --check fails if stale (CI)
nexus dev                         scan + overlay so annotations take effect live, no commit
nexus build                       runs `nexus generate handlers` before go build
```
Slots next to the existing `nexus client` SDK codegen.

## Coexistence & opt-out

- Generated `decorate.*` and hand-written `nexus.Module(nexus.AsRest(...))` emit
  identical options and share one container.
- Opt out per-handler (don't annotate → register by hand) or entirely (don't run
  `nexus generate` → framework behaves exactly as today).
- The app binary links `decorate` only; deco is build-time.

## Phasing

1. **DONE** — `nexus/decorate` runtime contract + example + tests.
2. **DONE** — `internal/handlergen` emitter: `Annotation[] → *_gen.go`, golden tests.
3. **DONE** — `transpiler.Scan(dir, keywords…) []Hit` in the deco repo (read-only
   annotation front-end; reuses deco's parser/packageDirs).
4. **DONE** — `nexus generate handlers` (cmd/nexus): `Scan → Site → handlergen.Generate
   → write`, with `--check` CI gate and byte-equal no-op writes. `handlergen.Generate`
   groups scan results per package, one file each; proven end-to-end (annotations →
   generated file → serving, coexisting with an explicit module).
5. **DONE** — `nexus dev` injects the registrations via a `go run -overlay`
   (regenerated each restart, zero source-tree churn); `nexus build` refreshes
   the committed `*_gen.go` before compiling. Proven end-to-end: dev serves a
   `//@rest` route with no committed file on disk; build regenerates it.
6. **DONE** — `//@use <expr>` middleware annotations (imports resolved from the
   annotated file's own import block); fixed the `path:`→`uri:` REST path-param
   doc bug in CLAUDE.md and `nexus docs`. Real end-to-end example at
   `examples/notes` (provide + REST×3 + GraphQL + explicit coexisting endpoint;
   dashboard lists all five identically, grouped under the `notes` service).

### Dependency wiring (resolved)

cmd/nexus imports `github.com/paulmanoni/deco/transpiler` (the scanner), pinned
at `v0.12.0` in go.mod — no replace. The app runtime links NO deco; only the CLI
does (verified via `go list -deps .`), the same shape as the viteless CLI
dependency.

## Prototype caveat

`decorate` accumulates registrations in a process-global registry (the natural
target for init-time decorators, matching deco's router-as-decorator model);
`Module()` drains+resets it. A later revision may scope the registry per-package
to drop the global.
