# Implementation Plan — Middleware Redesign, Steps 1–2

**Companion to:** [`middleware-redesign.md`](./middleware-redesign.md) §9.
**Covers:** Step 1 (new core types) + Step 2 (legacy shim).
**Property:** purely additive. No existing file's behavior changes; nothing in the
production execution path is rewired. The new types compile alongside the current ones
and are exercised only by their own unit tests. Step 3 flips the adapters over to them.

---

## 0. Two collisions that shape the plan

Before any code, two facts about the current package force specific choices. Both come
from the existing struct in `middleware/middleware.go:54`.

### 0.1 The name `Middleware` is already taken (by a struct)

The spec (§3) names the new interface `Middleware`. But `middleware.Middleware` is
already a **struct**, and that struct name is public API — it's the parameter type of
`nexus.Use(m middleware.Middleware)` (`app_use.go:25`) and the return type of factories
like `ratelimit.NewMiddleware`. Renaming it now breaks callers; you cannot have a struct
and an interface share a name in one package.

**Decision:** introduce the new interface under the transitional name **`Handler`** in
steps 1–4. The final rename (`Handler` → `Middleware`, struct `Middleware` → `Bundle`,
with one release of type aliases) is deferred to step 5, where the struct is already
internal-only. The struct's own doc comment already calls it "an executable BUNDLE", so
`Bundle` is the natural eventual name. This keeps steps 1–2 non-breaking.

### 0.2 The struct can't implement the interface directly (field vs method)

The struct has a **field** `Name string`. An interface requiring a **method** `Name()`
can never be satisfied by a type that has a field of the same name. So "make
`middleware.Middleware{Gin,Graph}` satisfy the new interface" (spec §9 step 2) is
implemented as a **wrapper**, not by hanging methods on the struct:

```go
type legacyBundle struct{ mw Middleware } // wraps the existing struct
```

`nexus.Use` will (in step 3) wrap the struct into a `legacyBundle` to obtain a `Handler`.
In steps 1–2 we only define the wrapper and its converter; we do not yet call it from the
registration path.

---

## 1. Step 1 — new core types

Four new files in `package middleware`. No edits to `middleware.go`.

### 1.1 `middleware/transport.go` (new)

```go
package middleware

// Transport identifies which wire protocol a request arrived on.
type Transport uint8

const (
	TransportREST Transport = iota
	TransportGraphQL
	TransportWebSocket
)

func (t Transport) String() string {
	switch t {
	case TransportREST:
		return "REST"
	case TransportGraphQL:
		return "GraphQL"
	case TransportWebSocket:
		return "WebSocket"
	default:
		return "unknown"
	}
}

// TransportSet is a bitset over Transport. The zero value is the empty set.
type TransportSet uint8

func bit(t Transport) TransportSet { return 1 << t }

// Transports builds a set from its members.
func Transports(ts ...Transport) TransportSet {
	var s TransportSet
	for _, t := range ts {
		s |= bit(t)
	}
	return s
}

// AllTransports is the common declaration for write-once middleware.
var AllTransports = Transports(TransportREST, TransportGraphQL, TransportWebSocket)

func (s TransportSet) Has(t Transport) bool { return s&bit(t) != 0 }

// String renders the set for fail-closed error messages, e.g. "{REST, GraphQL}".
func (s TransportSet) String() string {
	var parts []string
	for _, t := range []Transport{TransportREST, TransportGraphQL, TransportWebSocket} {
		if s.Has(t) {
			parts = append(parts, t.String())
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
```
(Add `import "strings"`.)

### 1.2 `middleware/phase.go` (new)

```go
package middleware

// Phase orders middleware uniformly across transports (redesign §7). Lower
// runs further out. Built-ins pick a phase; nexus.Use defaults to PhaseApp.
type Phase int

const (
	PhaseRecover   Phase = iota // outermost: panic boundary
	PhaseObserve                // trace, metrics, request-id
	PhaseCORS                   // (skipped by the WS frame chain)
	PhaseRateLimit
	PhaseAuth  // identity resolution
	PhaseAuthz // Requires(...) gates
	PhaseApp   // user nexus.Use(...), default
)
```
Phases are declared now so step 3's chain builder can sort by them; nothing reads `Phase`
in steps 1–2.

### 1.3 `middleware/handler.go` (new)

```go
package middleware

import "context"

// Handler is the unified middleware shape (redesign §3). Transitional name —
// becomes Middleware in step 5. A single implementation serves every transport
// it declares in Transports().
type Handler interface {
	Name() string
	Transports() TransportSet
	Handle(rc *RequestCtx, next Next) error
}

// Next advances the chain. Returning an error (or rc.Reject) short-circuits;
// the active carrier translates it per transport.
type Next func(*RequestCtx) error

// Func is the closure adapter — most middleware is one of these.
type Func struct {
	name string
	set  TransportSet
	fn   func(rc *RequestCtx, next Next) error
}

// NewFunc builds a Func Handler.
func NewFunc(name string, set TransportSet, fn func(rc *RequestCtx, next Next) error) Func {
	return Func{name: name, set: set, fn: fn}
}

func (f Func) Name() string                          { return f.name }
func (f Func) Transports() TransportSet              { return f.set }
func (f Func) Handle(rc *RequestCtx, n Next) error   { return f.fn(rc, n) }

// carrier is the per-transport backing behind RequestCtx. Each adapter (step 3+)
// implements it over *gin.Context, graph.ResolveParams, or a WS frame.
type carrier interface {
	header(key string) string
	clientIP() string
	path() string
	reject(status int, err error) error
}

// RequestCtx is the transport-neutral request handle middleware sees (§3.2).
// Middleware never touches gin.Context or graph.ResolveParams.
type RequestCtx struct {
	Context   context.Context
	Transport Transport

	carrier carrier
	bag     map[any]any
}

// newRequestCtx is the constructor adapters call. Unexported — only adapters
// (and tests via an in-package fake) build a RequestCtx.
func newRequestCtx(ctx context.Context, t Transport, c carrier) *RequestCtx {
	return &RequestCtx{Context: ctx, Transport: t, carrier: c}
}

func (rc *RequestCtx) Header(key string) string { return rc.carrier.header(key) }
func (rc *RequestCtx) ClientIP() string         { return rc.carrier.clientIP() }
func (rc *RequestCtx) Path() string             { return rc.carrier.path() }

// WithContext replaces the underlying context — the functional equivalent of
// stashing a value on gin.Context for downstream middleware/handlers.
func (rc *RequestCtx) WithContext(ctx context.Context) { rc.Context = ctx }

// Set/Get share typed values across the chain without internal-package coupling.
func (rc *RequestCtx) Set(key, val any) {
	if rc.bag == nil {
		rc.bag = make(map[any]any, 4)
	}
	rc.bag[key] = val
}

func (rc *RequestCtx) Get(key any) (any, bool) {
	v, ok := rc.bag[key]
	return v, ok
}

// Reject short-circuits the chain. Returns the error so callers can write
// `return rc.Reject(401, ErrUnauthenticated)`.
func (rc *RequestCtx) Reject(status int, err error) error {
	return rc.carrier.reject(status, err)
}
```

**Note on `newRequestCtx` being unexported:** adapters live in `package nexus`, not
`package middleware`, so they cannot call an unexported constructor. Resolve this in
step 3 by either (a) moving the carrier interface + constructor to an exported
`middleware.NewRequestCtx(ctx, t, Carrier)` with an exported `Carrier` interface, or
(b) keeping adapters in a `middleware/internal` sub-package. **Decision deferred to
step 3** — for steps 1–2 the unexported form is correct because only in-package tests
construct a `RequestCtx`. Flagged here so it isn't a surprise.

---

## 2. Step 2 — legacy shim

One new file. Still no edits to the registration path.

### 2.1 `middleware/legacy.go` (new)

```go
package middleware

import "fmt"

// legacyBundle adapts the existing Middleware struct to the Handler interface.
// It's a wrapper (not methods on the struct) because the struct's Name FIELD
// would collide with a Name() METHOD (see impl plan §0.2).
type legacyBundle struct{ mw Middleware }

// AsHandler wraps a legacy bundle as a Handler so existing nexus.Use(...) bundles
// flow through the new pipeline and participate in fail-closed (step 3).
func AsHandler(mw Middleware) Handler { return legacyBundle{mw: mw} }

func (b legacyBundle) Name() string { return b.mw.Name }

// Transports infers the set from which realizations are present: Gin backs
// REST + the WS upgrade route; Graph backs GraphQL (redesign §3.1, §9 step 2).
func (b legacyBundle) Transports() TransportSet {
	var s TransportSet
	if b.mw.Gin != nil {
		s |= bit(TransportREST) | bit(TransportWebSocket)
	}
	if b.mw.Graph != nil {
		s |= bit(TransportGraphQL)
	}
	return s
}

// Handle bridges to the legacy realization for the active transport.
//
//   - GraphQL: b.mw.Graph is already func(next) next — bridged here directly.
//   - REST / WS: b.mw.Gin is a gin.HandlerFunc driven by gin's own c.Next();
//     the REST/WS adapter (step 3) unwraps legacyBundle and runs b.mw.Gin
//     natively in its gin chain rather than routing through Handle. So this
//     path is intentionally a guard until step 3 wires the unwrap.
func (b legacyBundle) Handle(rc *RequestCtx, next Next) error {
	switch rc.Transport {
	case TransportGraphQL:
		if b.mw.Graph == nil {
			return next(rc)
		}
		return bridgeGraph(rc, b.mw.Graph, next)
	default:
		return fmt.Errorf("nexus: legacyBundle %q has no functional realization for %s; "+
			"the gin path is run natively by the REST/WS adapter (step 3)", b.mw.Name, rc.Transport)
	}
}
```

`bridgeGraph` (the Graph→Handler bridge) is the one piece of real bridging logic worth
landing now, since it's transport-clean and unit-testable without gin. Its exact body
depends on the GraphQL carrier shape (how `graph.ResolveParams` is reached from a
`RequestCtx`), which is a step-3 artifact. **For steps 1–2, stub it** so the package
compiles and the GraphQL bridge has a home:

```go
// bridgeGraph runs a legacy graph.FieldMiddleware inside the functional chain.
// Full body lands with the GraphQL carrier in step 3; stubbed so legacy.go
// compiles and is unit-testable via a fake carrier.
func bridgeGraph(rc *RequestCtx, _ graph.FieldMiddleware, next Next) error {
	return next(rc) // TODO(step 3): drive the FieldMiddleware via the GraphQL carrier
}
```
(Import `github.com/paulmanoni/nexus/graph`.)

This keeps step 2 honest: `Name()` and `Transports()` — the two methods step 3's
fail-closed assertion actually depends on — are **fully implemented and tested now**;
the execution bridges are stubbed and explicitly owned by step 3.

---

## 3. Tests (new)

One new file: `middleware/handler_test.go`. These are the acceptance gate for steps 1–2;
no integration wiring exists to test yet.

1. **`TransportSet` bit math** — `Transports(REST, GraphQL).Has(...)` truth table; empty
   set; `AllTransports.Has` all three; `String()` renders `{REST, GraphQL}` in canonical
   order.
2. **`Func`** — `NewFunc(...).Name/Transports` round-trip; `Handle` invokes the closure
   and threads `next`.
3. **`legacyBundle.Transports` inference** — Gin-only → `{REST, WebSocket}`; Graph-only →
   `{GraphQL}`; both → all but `WebSocket`-via-Graph stays off (assert exact bits);
   neither → empty set.
4. **`RequestCtx` over a fake carrier** — an in-package `type fakeCarrier struct{...}`
   implementing `carrier`; assert `Header/ClientIP/Path` delegate, `Set/Get` round-trips,
   `Reject` returns the carrier's error, `WithContext` swaps the context.
5. **`legacyBundle.Handle`** — GraphQL transport with a nil `Graph` calls `next`
   (pass-through); non-GraphQL transport returns the guard error mentioning the transport.

---

## 4. Explicitly NOT in steps 1–2 (owned by step 3)

To keep the PR boundary crisp, these are out of scope and must not be touched:

- `app_use.go` — `MiddlewareOption.applyToRest/applyToGql/applyToWS` stay as-is. `Use`
  still stores raw bundles on `cfg.bundles`.
- `routing_endpoint_chain.go` — `buildEndpointChain` still extracts `.Gin`. Not rewired.
- `transport_rest.go`, `transport_graph.go`, `transport_ws.go` — no changes. The
  registration-time `Transports()` **assertion** (fail-closed) lands in step 3 at each
  transport's option loop, where `applyToX` is called.
- The carrier **implementations** (gin / graph / ws) — step 3.
- `bridgeGraph` body and the REST/WS legacy-unwrap — step 3.
- The exported-constructor decision (§1.3 note) — step 3.
- Built-in ports (`ratelimit`, `auth`, `cors`, `metrics`, `trace`) — step 3.

---

## 5. Acceptance criteria

- `go build ./...` and `go vet ./...` clean.
- `go test ./middleware/...` passes, covering the five test groups in §3.
- No diff outside `middleware/` (four new files + one test file). `git diff --stat`
  shows only additions under `middleware/`.
- `ratelimit.NewMiddleware(...)` and every existing `nexus.Use(...)` call site still
  compile unchanged (sanity: `go build ./examples/...`).

---

## 6. Risks & notes

- **Naming churn later.** The `Handler`→`Middleware` / struct→`Bundle` rename (step 5)
  is mechanical but touches public API; plan to ship it behind type aliases for one
  release. Calling it out now (§0.1) so reviewers expect the transitional name.
- **`bridgeGraph` shape risk.** Its real body depends on how the GraphQL carrier exposes
  `graph.ResolveParams`. If step 3 finds the functional bridge awkward, the fallback is
  to have the GraphQL adapter unwrap `legacyBundle` and use `.Graph` directly (mirroring
  the REST unwrap), making `bridgeGraph` unnecessary. Either way, steps 1–2 are
  unaffected — the stub just gets deleted or filled.
- **Allocation.** `RequestCtx.bag` is lazily allocated, so middleware that never calls
  `Set` pays nothing. The per-request `RequestCtx` alloc itself is a step-3 hot-path
  concern (redesign §11) — not relevant while nothing constructs one in production.

---

## 7. Checklist

- [ ] `middleware/transport.go` — Transport, TransportSet, Transports, AllTransports, Has, String
- [ ] `middleware/phase.go` — Phase enum
- [ ] `middleware/handler.go` — Handler, Next, Func/NewFunc, RequestCtx, carrier
- [ ] `middleware/legacy.go` — legacyBundle, AsHandler, Transports inference, Handle guard, bridgeGraph stub
- [ ] `middleware/handler_test.go` — five test groups (§3)
- [ ] `go build ./... && go vet ./... && go test ./middleware/...` green
- [ ] `git diff --stat` shows additions under `middleware/` only
