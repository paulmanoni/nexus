# Implementation Plan — Middleware Redesign, Step 3

**Companion to:** [`middleware-redesign.md`](./middleware-redesign.md) §9 and
[`middleware-impl-steps-1-2.md`](./middleware-impl-steps-1-2.md).

Step 3 in the spec bundles two changes with very different risk profiles, so it is
split:

- **3a — Fail-closed registration.** Small, self-contained, the headline safety win.
  No execution-path change. **Status: DONE.**
- **3b — Adapters + built-in ports.** The unified authoring layer
  (`FromHandler` + carriers + IP centralization). **Status: authoring layer DONE
  (§2); built-in ports BLOCKED on a reject-fidelity decision (§3).**

---

## 1. Step 3a — fail-closed registration (DONE)

The bundle execution path is unchanged. The only addition is a registration-time guard:
a middleware that declares some transports but is attached to a transport it doesn't
honor now errors at boot instead of silently no-opping. This is the auth-bypass footgun
from redesign §1.2 / §5.

### What shipped

- **`app_use.go`** — new pure helper `checkBundleTransports(bundles, t, opID) error`.
  For each attached bundle it computes `middleware.AsHandler(b).Transports()` (the
  step-1/2 inference) and errors when the set is non-empty and lacks `t`. The error
  names the middleware, the transport, the op, and points at `UseOnRest/UseOnGraph/UseOnWS`.
- **Wiring** at all four registration entry points, after the option loop where the op
  identifier is known:
  - `transport_rest.go` — `AsRest` (opID `"<METHOD> <path>"`) and `AsRestHandler`.
  - `transport_graph.go` — `asGqlField` (opID `cfg.opName`), serving `AsQuery`/`AsMutation`.
  - `transport_ws.go` — `AsWS` (opID `"WS <path> <type>"`).
  Each returns `rawOption{o: fx.Error(err)}` on failure — the existing fail-fast idiom,
  so misattachment surfaces through fx at startup.
- **`app_use_test.go`** — table test over the helper (gin/graph/both/empty bundles ×
  each transport) plus a message-content assertion.

### Design note: empty bundles are allowed

A bundle with neither `Gin` nor `Graph` (`Transports() == 0`) is a pure dashboard label
that enforces nothing. It is **not** rejected — it protects nothing, so attaching it
anywhere is harmless, and rejecting it would break the existing name-only labelling
pattern. The guard targets exactly the dangerous case: a bundle that *does* enforce
something on transport X, attached to transport Y where it would silently not run.

### Why this didn't break existing middleware

The built-ins that get cross-attached — `auth.Required`, `auth.Requires`, `auth.Optional`,
`ratelimit.NewMiddleware` — all set **both** `Gin` and `Graph`, so they declare every
transport and pass everywhere. `metrics`/`trace` are added only by `buildEndpointChain`
(REST/WS) and never attached to GraphQL ops; `cors` is engine-global, not per-op. Full
suite + examples build green.

### Verification

`go build ./...`, `go vet ./. ./middleware/...`, `go test ./...`, `go build ./examples/...`
— all clean.

---

## 2. Step 3b — unified authoring layer (DONE)

Rather than the full execution-path rewrite the spec first imagined (replace the
gin-native chain with a `Handler` runner), 3b landed a lower-risk realization that
delivers the same headline win — **write-once middleware** — without touching the hot
path. The `Middleware` bundle stays the transport-boundary object; a new `FromHandler`
authoring layer fills its `.Gin`/`.Graph` realizations from a single `Handler`.

### What shipped

- **`middleware/clientip.go`** — the canonical `WithClientIP`/`ClientIPFromCtx` + key,
  moved into the neutral `middleware` package so every carrier and the ratelimit
  extension share one key without an import cycle.
- **`extension/ratelimit/middleware.go`** — `WithClientIP`/`ClientIPFromCtx` now delegate
  to `middleware`'s (back-compat wrappers; the old `clientIPCtxKey` is deleted). Existing
  transport stashing (`ratelimit.WithClientIP`) now writes the neutral key, so the
  GraphQL carrier reads it.
- **`middleware/adapter.go`** — `ginCarrier` (over `*gin.Context`) and `graphCarrier`
  (over `graph.ResolveParams`) implementing the unexported `carrier`, plus `ginAdapter`
  / `graphAdapter` that run a `Handler` over a `RequestCtx`, plus **`FromHandler(h)
  Middleware`** — generates exactly the realizations `h.Transports()` declares.
- **`middleware/adapter_test.go`** — realization generation per transport set;
  gin pass-through + reject (via a real `gin.Engine`); graph pass-through + reject;
  client-IP round-trip.

### Why this over the chain-runner rewrite

- **Zero hot-path change.** `buildEndpointChain` and the resolver wrap are untouched, so
  no allocation regression and no ordering-parity risk. The redesign §11 `RequestCtx`
  allocation question is deferred until a ported middleware actually runs on the hot
  path.
- **Incremental.** New middleware can be authored once today; built-ins migrate one at a
  time behind the unchanged bundle boundary.
- **The deferred constructor decision (impl-1-2 §1.3) is moot** for now: the carriers
  live in `package middleware`, so nothing in `package nexus` needs to build a
  `RequestCtx`. `carrier`/`newRequestCtx` stay unexported.

---

## 3. Built-in ports — reject fidelity (option A chosen)

Porting `auth` and `ratelimit` to a single `FromHandler` implementation required closing
a real gap: the generic `RequestCtx.Reject(status, err)` couldn't express their
transport-specific reject semantics.

- **`auth`** (`extension/auth/middleware.go`) — `rejectUnauthenticated`/`rejectForbidden`
  call `emitReject` (dashboard/trace events), invoke app-supplied
  `OnUnauthenticated`/`OnForbidden` hooks that take a `*gin.Context`, and use
  `wrapGraphErr` for the GraphQL error shape. A generic `Reject` can do none of these.
- **`ratelimit`** (`extension/ratelimit/gin.go`) — denial sets a `Retry-After` response
  header alongside the 429. `Reject(status, err)` has no way to set response headers.

### Decision: option A (richer RequestCtx), scoped lean

Chosen: add `RequestCtx.SetHeader(key, val)` — sets a response header on REST/WS,
no-ops on GraphQL — backed by `carrier.setHeader`. This covers ratelimit's `Retry-After`
without exposing the raw transport object (option B) or leaving the duplication in place
(option C). The neutral abstraction grows by exactly one HTTP-leaning method that
degrades safely on GraphQL.

### `ratelimit` — PORTED

`NewMiddleware` is now a single `middleware.FromHandler(NewFunc(...))`:

- One implementation, one IP source (`rc.ClientIP()`) — `ginEnforcer`/`graphEnforcer`
  and the `c.ClientIP()` vs `ClientIPFromCtx` split are deleted.
- Denial: `rc.SetHeader("Retry-After", secs)` (no-op on GraphQL) then `rc.Reject(429,
  …)`. REST gets 429 + `Retry-After`; GraphQL gets the `rate limit exceeded — retry
  after X` error. Verified in `extension/ratelimit/port_test.go` (REST, GraphQL, PerIP).
- **Documented behavior change:** the REST 429 JSON body unifies from
  `{error, retryAfter, key}` to `{error: "rate limit exceeded — retry after X"}`. The
  actionable retry timing is preserved in the `Retry-After` header and the message; the
  redundant `retryAfter`/`key` fields are dropped. Note this in the release notes.
- `GinMiddleware` (public, used for the engine-root global limit) is untouched.

### `auth` — PORTED

`auth` needed more than `SetHeader`: `rejectUnauthenticated`/`rejectForbidden` run
app-supplied `OnUnauthenticated`/`OnForbidden` hooks that take a `*gin.Context`. Solved
with a neutral reject-hook (`middleware/rejecthook.go`):

- **`middleware.WithRejectHook(ctx, fn)`** where `fn(c *gin.Context, status, err) bool`,
  plus `ginCarrier.reject` invoking it before the default abort. The hook closure lives
  in `auth`; only the plumbing lives in `middleware` — no `auth` dependency leaks in.
- `Required`/`Requires`/`Optional` are now single `middleware.FromHandler(NewFunc(...))`
  Handlers via an `auth.builtin(...)` helper. `ginRequired`/`graphRequired`/`ginRequires`/
  `graphRequires` and `rejectUnauthenticated`/`rejectForbidden` are **deleted**.
- `rejectAuth(rc, err)` branches on `rc.Transport`: GraphQL → `wrapGraphErr` (unchanged);
  REST/WS → `rc.Reject(status, err)`, which hands off to `authRejectHook` (a faithful
  merge of the old reject funcs — `emitReject`, app hook, status fallback). The hook owns
  only 401/403 and returns false otherwise, so rate-limit's 429 keeps the default abort.
- `ginAuthMiddleware` installs the hook per request via `WithRejectHook`.
- **Coverage:** existing end-to-end tests exercise the reworked path —
  `TestOnUnauthenticated_CustomEnvelope` (custom 401 envelope proves the app hook runs
  through the gin carrier) and `TestRejectEvent_FiresOnUnauthenticated` (the `auth.reject`
  event still fires). Full suite + examples green.

### `cors` / `trace` / `metrics` — NOT PORTED (by design)

On inspection these are categorically different from `auth`/`ratelimit`: they aren't one
*decision* rendered two ways, they're observability/HTTP concerns that legitimately read
transport-specific response signals. Porting them is pointless or lossy:

- **`cors`** (`extension/cors/middleware.go`) — a bare `gin.HandlerFunc` registered
  globally at the engine root; not a bundle. CORS is a browser/HTTP concept with no
  GraphQL counterpart, so there is nothing to unify. Porting yields zero dedup and would
  need a new no-body-204 abort primitive. Stays a `gin.HandlerFunc`.
- **`trace`** (`trace/middleware.go`) — also a bare `gin.HandlerFunc`, REST/WS-only, with
  no dual realization (GraphQL tracing is emitted on a separate path). It reads
  `c.Writer.Status()` / `c.Errors` after `next`. Porting would mean inventing a graph
  realization it doesn't have plus exposing response status/errors on `RequestCtx` — net
  new complexity, no dedup.
- **`metrics`** (`extension/metrics/middleware.go`) — has real dual realizations, but they
  read different signals: the gin recorder does panic-recover-and-re-panic with
  `debug.Stack()`, `c.Writer.Status()` (4xx vs 5xx), and `c.Errors`; the graph recorder
  reads only the error from `next`'s return. The shared logic (`wrapErrtrace`,
  `publishOpEventWithStatus`, `splitKey`) is already factored into helpers both call; what
  remains is irreducibly transport-specific.

Routing these through `FromHandler` would require expanding `RequestCtx` with
response-introspection (`ResponseStatus`, `ResponseError`, panic handling) — a large
HTTP-leaning expansion of the neutral abstraction that benefits only framework-internal
code and adds regression risk around gin panic/status semantics. **Decision: leave them
as-is.** The `FromHandler` win is fully realized for the user-facing enforcement
middleware (`auth`, `ratelimit`, and future custom middleware) that the redesign targeted.

**Small cleanup done:** `metrics` now reads the caller IP via `middleware.ClientIPFromCtx`
instead of `ratelimit.ClientIPFromCtx`, dropping its `ratelimit` import — finishing the
IP-key centralization started with the ratelimit port.

### Out of scope (step 4)

Per-frame / per-type WebSocket (`WSMiddleware.OnFrame`, union frame chains) stays in
step 4.

---

## 4. Checklist

**3a (done)**
- [x] `checkBundleTransports` helper + tests
- [x] Wired into `AsRest`, `AsRestHandler`, `asGqlField`, `AsWS`
- [x] `go build/vet/test ./...` + examples green

**3b authoring layer (done)**
- [x] `middleware/clientip.go` — neutral IP key; ratelimit delegates
- [x] `middleware/adapter.go` — `ginCarrier`, `graphCarrier`, `ginAdapter`, `graphAdapter`, `FromHandler`
- [x] `middleware/adapter_test.go` — generation + gin/graph pass-through & reject
- [x] `go test ./...` green

**Built-in ports**
- [x] Decide reject-fidelity option → A (§3)
- [x] `RequestCtx.SetHeader` + carrier `setHeader` + tests
- [x] Port `ratelimit` (`Retry-After` preserved; body-shape change documented)
- [x] Port `auth` (`middleware.WithRejectHook` for `OnUnauthenticated`/`OnForbidden`)
- [x] `cors`/`trace`/`metrics`: decided NOT to port (transport-specific by nature); metrics IP read centralized
- [ ] Benchmark hot path; pool `RequestCtx` if warranted
