# nexus/di — built-in DI container (fx replacement)

Goal: drop `go.uber.org/fx` (+ `dig`, `multierr`, `atomic`) from the **default**
nexus build, keeping fx available as an opt-in adapter — the same shape as the
`httpx` router seam (stdlib default, gin/chi opt-in modules).

## Why not godecorate

`godecorate` is a decorator/AOP transpiler (comment-driven code-gen that wraps
function calls). It has no constructor injection, lifecycle, or value groups, so
it cannot replace fx's role in nexus. A purpose-built container can — and is
small, because nexus only uses a thin slice of fx.

## What this package provides (the slice nexus uses)

| fx | di equivalent | status |
|----|---------------|--------|
| `fx.Provide` | `di.Provide` | ✅ lazy, singleton, multi-return, trailing `error` |
| `fx.Supply` | `di.Supply` | ✅ |
| `fx.Invoke` | `di.Invoke` | ✅ eager, registration-order |
| `fx.Options` / `fx.Module` | `di.Options` / `di.Module` | ✅ (Module name = diagnostics only) |
| `fx.In` + `optional:"true"` | `di.In` + `optional:"true"` | ✅ |
| `fx.In` + `group:"x"` | `di.In` + `group:"x"` | ✅ |
| `fx.Out` + `group:"x"` | `di.Out` + `group:"x"` | ✅ |
| `fx.Annotate(.., fx.ResultTags(..))` | `di.Annotate(.., di.ResultTags(..))` | ✅ (GraphQL field group) |
| `fx.ParamTags` | `di.ParamTags` | ✅ (parity; nexus unused) |
| `fx.Lifecycle` / `fx.Hook` | `di.Lifecycle` / `di.Hook` | ✅ ordered start, reverse stop, start-rollback |
| `fx.Error` | `di.Error` | ✅ |
| `fx.New(..).Run()` | `di.New(..)` + `App.Run/Start/Stop/Err` | ✅ signal-driven Run |
| `fx.NopLogger` / `fxevent` | — | n/a (nexus's quiet-mode logging is fx-specific; replace with plain stderr) |
| `fx.Decorate` / `fx.Replace` | — | not used by core; `nexus.Raw` escape hatch goes away or routes to adapter |

Semantics intentionally match fx: constructors are lazy and run once; invokes
run eagerly at `New()` in tree order while Provide order is irrelevant;
duplicate typed providers and dependency cycles error.

## Migration plan (the seam)

1. **Seam type.** Change `nexus.Option`'s hidden method from
   `nexusOption() fx.Option` to a di-level intent (`nexusOption() di.Option`),
   and rewrite the wrappers in `app_options.go`:
   `Provide/Supply/Invoke/Options/Module/Raw` → the `di.*` equivalents.
   The reflective `reflect.MakeFunc` invoke synthesis in `transport_rest.go`,
   `transport_graph.go`, `app_workers.go` stays — it just emits `di.Invoke` /
   `di.Provide(di.Annotate(..))` instead of the fx forms.
2. **In-structs.** `manifest_automount.go` (`autoMountIn`) and
   `routing_default_gate.go` swap `fx.In` → `di.In` (tags unchanged).
3. **Lifecycle.** `obs_integration.go`, `db/bind.go`, `extension/cache/bind.go`,
   `app_workers.go`, `pubsub/broker.go` swap `fx.Lifecycle`/`fx.Hook` →
   `di.Lifecycle`/`di.Hook`.
4. **Run.** `Run()` builds `di.New(all...)` and calls `.Run()`; replace the
   `fxevent` quiet-logger paths with plain stderr handling (di already prints
   build/start errors to stderr).
5. **fx fate (decide after prototyping — currently open):**
   - Keep fx as `nexus/di/fxcontainer` opt-in adapter (separate module), OR
   - Remove fx entirely. The container covers every current call site, so
     removal is viable; the adapter is insurance for unusual graphs.

## Status — INTEGRATED

The migration is complete on branch `feat/di-seam`:

- `nexus.Option` is now `nexusOption() di.Option`; `app_options.go` Run path
  selects a `di.Backend` (default `di.Builtin()`), collects the option tree to a
  `di.Spec`, and runs it.
- The two `fx.In` consumers (`autoMountGraphQL`, `applyDefaultGate`) are now
  annotated invokes using `di.ParamTags` (`group:"…"` / `optional:"true"`) — no
  marker structs, so both backends wire them identically.
- All lifecycle sites (`obs_integration`, `db`, `cache`, `workers`, `pubsub`)
  use `di.Lifecycle`/`di.Hook`.
- Extensions use `nexus.Error` / `nexus.Lifecycle` / `nexus.Raw(di.…)` — no
  extension imports fx.
- `go.mod` no longer requires `go.uber.org/fx` or `dig`; `go list -deps .`
  confirms the default build links neither.
- **fx kept as opt-in:** `nexus/di/fxcontainer` is a SEPARATE module (own
  `go.mod`, like `httpx/ginrouter`) implementing `di.Backend` by translating the
  `di.Spec` onto fx (raw tag strings pass straight to `fx.ResultTags`/
  `fx.ParamTags`; `di.Lifecycle` is bridged to `fx.Lifecycle`). Select it with
  `nexus.WithContainer(fxcontainer.New())`.

Full `go build ./...`, `go vet ./...`, and `go test ./...` pass in both modules.

### Test helper note

`nexus`'s white-box tests used `fxtest.New`; they now use a small `diTestApp`
shim (`ditest_shim_test.go`) with the same `RequireStart`/`RequireStop` surface.
The `newApp` helper was fixed to mirror Run's invoke ordering (early → user →
late) so `autoMountGraphQL` runs after user options register virtual fields.
