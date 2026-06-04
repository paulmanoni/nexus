# Middleware Redesign

**Status:** Proposed
**Scope:** `middleware/`, `app_use.go`, `routing_endpoint_chain.go`, `transport_rest.go`,
`transport_graph.go`, `transport_ws.go`, and the built-in middleware under
`extension/{auth,cors,ratelimit,metrics}` + `trace/`.

This document specifies a redesign of nexus's cross-transport middleware system.
It replaces the dual-realization bundle with a single functional middleware over a
transport-neutral request context, makes silent transport degradation a boot-time
error, and promotes WebSocket middleware to a first-class per-frame model.

---

## 1. Motivation

### 1.1 Today's model

Middleware is a bundle carrying one realization per transport
(`middleware/middleware.go:54`):

```go
type Middleware struct {
    Name, Description string
    Kind  Kind
    Gin   gin.HandlerFunc        // REST + WS upgrade
    Graph graph.FieldMiddleware  // func(next) next
}
```

Attachment is via `nexus.Use(m)` (`app_use.go:25`), which each transport applies by
picking the field it understands and ignoring the rest. Global middleware lives in
`Config.Middleware.Global`; GraphQL has a second path via `nexus.GraphMiddleware`; rate
limiting is special-cased into `transport_graph.go`.

### 1.2 Problems

1. **Two incompatible authoring styles.** Gin is imperative (`c.Next()`, write to `c`);
   Graph is functional (`func(next) next`). One logical concern is written twice and
   kept in sync by hand. `ratelimit` is the proof: `ginEnforcer` and `graphEnforcer`
   duplicate the PerIP/scope logic and extract the client IP two different ways
   (`c.ClientIP()` vs `ratelimit.ClientIPFromCtx`).

2. **Silent degradation is a security footgun.** `MiddlewareOption.applyToGql`
   (`app_use.go:50`) records a bundle's *name* even when its `Graph` field is nil, so the
   dashboard lists it as present while nothing executes. Attaching a Gin-only
   `auth.Required()` to a GraphQL op looks protected and is wide open.

3. **WebSocket is second-class.** `applyToWS` (`app_use.go:69`) and `transport_ws.go`
   install middleware only on the *first* `AsWS` call for a path, only at HTTP-upgrade
   time. Later registrations are dropped with a warning. There is no per-frame or
   per-message-type gating.

4. **Four attachment APIs, no targeting.** `nexus.Use`, `nexus.GraphMiddleware`,
   `Config.Middleware.Global`, and the rate-limit special case. There is no way to
   deliberately scope a middleware to a single transport.

5. **Implicit, divergent ordering.** REST chains trace → metrics → user → handler
   (`routing_endpoint_chain.go`); GraphQL reverse-wraps validators → user → resolver.
   There is no shared notion of ordering or phases.

---

## 2. Design overview

Three changes, decided together:

- **Unify, keep raw escape hatches.** A single `Middleware` interface over a
  transport-neutral `RequestCtx` is the default authoring path. Raw single-transport
  helpers (`UseOnRest`/`UseOnGraph`/`UseOnWS`) remain first-class for genuine edge cases.
- **Fail closed at registration.** A middleware attached to a transport it cannot honor
  is a boot-time error, not a runtime no-op.
- **Per-frame + per-type WebSocket.** WS middleware gains a per-message hook and the
  per-path chain is the union of every `AsWS` registration, dispatched per message type.

---

## 3. Core types

```go
// Middleware is the one shape authors write. A single implementation serves every
// transport it declares in Transports().
type Middleware interface {
    Name() string
    Transports() TransportSet
    Handle(rc *RequestCtx, next Next) error
}

// Next advances the chain. Returning an error (or calling rc.Reject) short-circuits;
// the active transport translates it to an HTTP status, a GraphQL error, or a WS close.
type Next func(*RequestCtx) error

// Func is the closure adapter — most middleware is one of these.
type Func struct {
    name string
    set  TransportSet
    fn   func(rc *RequestCtx, next Next) error
}
func (f Func) Name() string             { return f.name }
func (f Func) Transports() TransportSet { return f.set }
func (f Func) Handle(rc *RequestCtx, next Next) error { return f.fn(rc, next) }
```

### 3.1 TransportSet

```go
type Transport uint8

const (
    TransportREST Transport = iota
    TransportGraphQL
    TransportWebSocket
)

type TransportSet uint8 // bitset over Transport

func Transports(ts ...Transport) TransportSet
func (s TransportSet) Has(t Transport) bool
func (s TransportSet) String() string // for error messages, e.g. "{REST, GraphQL}"

// AllTransports is the common case for write-once middleware.
var AllTransports = Transports(TransportREST, TransportGraphQL, TransportWebSocket)
```

`Transports()` is the enforcement hook for fail-closed (§5). Unified middleware returns
`AllTransports`; raw `UseOnX` helpers return a single-transport set.

### 3.2 RequestCtx

`RequestCtx` is the neutral abstraction both Gin and GraphQL can produce. Middleware
never touches `gin.Context` or `graph.ResolveParams` — those become adapter internals.

```go
type RequestCtx struct {
    Context   context.Context // the real ctx; replace via WithContext to inject values
    Transport Transport

    carrier carrier // transport-backed, unexported
}

// Request-side reads — one implementation per concern, no more dual extraction.
func (rc *RequestCtx) Header(key string) string
func (rc *RequestCtx) ClientIP() string
func (rc *RequestCtx) Path() string // REST route or GraphQL field path

// Cross-middleware sharing — exported, typed keys, no internal-package coupling.
func (rc *RequestCtx) Set(key, val any)
func (rc *RequestCtx) Get(key any) (any, bool)

// Context injection — the functional equivalent of stashing on gin.Context.
func (rc *RequestCtx) WithContext(ctx context.Context)

// Short-circuit — translated per transport by the carrier.
func (rc *RequestCtx) Reject(status int, err error) error
```

```go
// carrier is the per-transport backing implemented by each adapter.
type carrier interface {
    header(key string) string
    clientIP() string
    path() string
    reject(status int, err error) error
}
```

`Reject` returns an `error` so middleware can `return rc.Reject(401, ErrUnauthenticated)`
in one line; the chain unwinds and the carrier renders the right wire response.

---

## 4. Transport adapters

Adapters are framework-internal. Each builds a `RequestCtx`, runs the chain, and
translates `Reject`.

### 4.1 REST

The chain compiles to a single `gin.HandlerFunc`. The carrier wraps `*gin.Context`:
`header` → `c.GetHeader`, `clientIP` → `c.ClientIP`, `reject` →
`c.AbortWithStatusJSON`. `WithContext` re-points `c.Request`.

### 4.2 GraphQL

Smallest change — already functional. The chain compiles to a `graph.FieldMiddleware`.
The carrier wraps `graph.ResolveParams`: `header` reads from the request stashed on the
resolve context, `reject` returns the error. This replaces the per-op `namedMw` wiring
in `transport_graph.go` and the rate-limit special case.

### 4.3 WebSocket

Two hook points (see §6).

---

## 5. Fail-closed registration

The behavioral centerpiece. With the unified model, every middleware honors every
transport it declares, so degradation mostly disappears. For deliberately
single-transport middleware, attaching it where it cannot run is a **registration
error**, surfaced at boot:

```
nexus: middleware "auth:required" attached to GraphQL op `searchUsers`
       but declares Transports = {REST}. Use nexus.UseOnRest(...) to scope it,
       or give it a GraphQL realization.
```

Each transport asserts at registration:

```go
if !mw.Transports().Has(thisTransport) {
    return fmt.Errorf("nexus: middleware %q attached to %s %s but declares Transports = %s",
        mw.Name(), thisTransport, opName, mw.Transports())
}
```

- Built-in gates (`auth.Required`, `auth.Requires`) declare `AllTransports`, so they are
  never the thing that fails.
- Raw `UseOnRest(...)` carries `Transports(TransportREST)`; attaching it to a query is
  caught at boot, not in production.
- The legacy bundle shim (§7) reports the set implied by its non-nil fields, so old
  code participates in the same check — a previously-silent Gin-only-on-GraphQL
  attachment now fails loudly.

---

## 6. WebSocket: per-frame + per-type

`transport_ws.go` moves from "first registration wins, upgrade-time only" to a union of
all `AsWS` registrations on a path, dispatched per message type.

```go
// WSMiddleware adds an optional per-message hook to the base interface.
type WSMiddleware interface {
    Middleware                                  // Handle(rc, next) — runs once at upgrade
    OnFrame(fc *FrameCtx, next FrameNext) error // runs per message; optional
}

type FrameCtx struct {
    Session *WSSession
    Type    string          // envelope "type" — enables per-message-type gating
    Data    json.RawMessage
    Context context.Context
}
type FrameNext func(*FrameCtx) error
```

Dispatch model:

1. **Upgrade time:** run each registration's `Handle` once for connection-wide concerns
   (origin checks, connection-level rate limit, auth handshake).
2. **Per message:** the hub builds a frame chain **per message type** from the union of
   all `AsWS` registrations on the path, then dispatches: resolve envelope `type` → run
   that type's frame chain → handler.

This kills first-registration-wins and lets `chat.send` require auth while
`presence.ping` does not, over one connection. Middleware that only cares about the
connection implements `Middleware`; middleware that gates individual messages implements
`WSMiddleware`.

---

## 7. Phases

A single ordering applied across all transports, replacing the per-transport implicit
order:

```
PhaseRecover → PhaseObserve → PhaseCORS → PhaseRateLimit → PhaseAuth → PhaseAuthz → PhaseApp
```

```go
type Phase int

const (
    PhaseRecover Phase = iota // outermost: panic boundary
    PhaseObserve              // trace, metrics, request-id
    PhaseCORS
    PhaseRateLimit
    PhaseAuth                 // identity resolution
    PhaseAuthz                // Requires(...) gates
    PhaseApp                  // user nexus.Use(...), default
)
```

Built-ins register into a phase. `nexus.Use` defaults to `PhaseApp`. Within a phase,
attachment order is preserved. The WS frame chain reuses this ordering minus
`PhaseCORS`.

---

## 8. Attachment API

```go
// Unified — honors every transport it's attached to (the default).
nexus.Use(mw)

// Raw single-transport escape hatches — Transports() is the corresponding singleton,
// so misattachment fails at boot (§5).
nexus.UseOnRest(h gin.HandlerFunc)
nexus.UseOnGraph(m graph.FieldMiddleware)
nexus.UseOnWS(f ws.FrameFunc)

// App-wide — every REST endpoint + GraphQL op + WS upgrade + dashboard.
Config.Middleware.Global // []Middleware, unchanged surface
```

`nexus.GraphMiddleware` and the rate-limit special case are deprecated in favor of
`Use` / `UseOnGraph` and a normal `PhaseRateLimit` middleware.

---

## 9. Migration

Non-breaking through step 3; only step 4 touches the WS dispatch loop.

1. **Add the core** — `Middleware` interface, `RequestCtx`, `TransportSet`, `Func`,
   phases — alongside the existing struct.
2. **Legacy shim** — make `middleware.Middleware{Gin, Graph}` satisfy the new interface;
   `Transports()` returns the set implied by its non-nil fields. Existing `nexus.Use(...)`
   calls keep compiling and now participate in fail-closed.
3. **REST + GraphQL adapters** — wire the registration-time `Transports()` assertion.
   This delivers fail-closed immediately. Port `ratelimit`, `auth`, `cors`, `metrics`,
   `trace` to the unified form, deleting the duplicated Gin/Graph pairs and the dual IP
   extraction.
4. **WS rework** — union frame chains + per-type dispatch + `OnFrame`.
5. **Deprecate** `GraphMiddleware` and the rate-limit special case once parity lands.

Steps 1–3 are independently shippable. User-facing surface is unchanged for the common
case: `nexus.Use(rl)` still compiles — the difference is that one `rl` is now genuinely
correct on GraphQL and WS instead of silently inert.

---

## 10. Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Unification depth | Unify, keep raw escape hatches | One authoring path by default; `UseOnX` for the rare transport-specific need |
| Degradation | Fail closed at registration | Eliminates the silent auth-bypass footgun; misattachment caught at boot |
| WebSocket scope | Per-frame + per-type | Fixes first-registration-wins and enables per-message gating on a shared connection |

## 11. Open questions

- **`RequestCtx` allocation cost** on the REST hot path — pool `RequestCtx` instances
  per request, or accept one alloc per request? Benchmark before deciding.
- **Streaming GraphQL** (subscriptions) — does `RequestCtx` need a per-event hook
  analogous to WS `OnFrame`, or is upgrade-time `Handle` sufficient for now?
- **Phase customization** — do users need to register middleware into a non-default
  phase, or is `PhaseApp` + ordering enough? Keep the phase enum internal until there's
  demand.