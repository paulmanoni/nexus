# How nexus fails — the error model

One page so anyone can reason about *where* a failure surfaces and *why*, without
reading the whole framework. nexus fails in exactly three layers. Match the
symptom to a layer and you know where to look.

## Layer 1 — Registration errors (boot time) → **panic**

Wiring that can't possibly work: a bad route, a missing `[databases.*]` block, a
malformed `nexus.toml`, an unresolved DI dependency, a decorator that can't be
resolved. These **panic at boot**, on purpose — a misconfigured binary must fail
loudly at startup, never limp into serving traffic.

Contract:
- Message is prefixed **`nexus: ...`** and, where possible, states the fix inline
  (e.g. *"no [databases.uaa] block found — declare it in nexus.toml"*).
- Deferred build errors go through `nexus.Raw(di.Error(...))` / `nexus.Error(err)`
  so they surface at boot with the same prefix.
- `MustLoadConfig` / `MustLoadExtensions` panic by documented contract.

Rule when adding one: wrap with context — `panic(fmt.Errorf("nexus: <what> %q: %w", x, err))`.
A bare `panic(err)` that drops a junior into a raw toml/parse stack with no
`nexus:` breadcrumb is a bug.

## Layer 2 — Handler-returned errors (request time) → **the transport**

A handler returning `(T, error)` with a non-nil error is normal control flow, not
a crash. The framework renders it per transport:

| Transport | Rendering |
|-----------|-----------|
| REST / WS | JSON error body / `error` envelope (customizable via `auth.OnError` etc.) |
| GraphQL   | an entry in the response `errors` array |

This is where *expected* failures live — validation, not-found, unauthorized.
Return an error; don't panic.

## Layer 3 — Runtime panics (request time) → **recover → StackError → dashboard + stderr**

A bug in user code — nil map write, slice out of range, nil deref. These must
**never crash the process**. Every execution context that calls a user-supplied
function recovers, mints a `*trace.StackError` (panic value + cleaned stack), and
routes it to the dashboard (red badge with the stack) *and* mirrors it to stderr.

The recover sites — **one per context, and this list must stay complete**:

| Context        | Recover site                                    |
|----------------|-------------------------------------------------|
| REST / GraphQL | `recoveryMiddleware` (`app_recovery.go`, global — `/graphql` is an HTTP route) |
| WebSocket      | `callWSHandler` (`transport_ws.go`)             |
| Workers        | `runWorker` (`app_workers.go`)                  |
| Crons          | cron dispatch (`extension/cron/cron.go`)        |
| Pubsub subs    | subscriber dispatch (`extension/pubsub`)        |

**The invariant:** *any context that invokes a user function recovers → mints a
`StackError` → surfaces it (500/error-envelope + bus + stderr).* WebSocket handlers
run in a per-connection read-loop goroutine, so a missing recover there would
crash the whole server — the reason `callWSHandler` exists.

Adding a new transport (or any new place that calls user code)? You **must**:
1. Add its recover site, mirroring `recoveryMiddleware`.
2. Add a case to `TestUserHandlerPanicsAreRecovered` — the test that enforces this
   invariant so the framework teaches the rule instead of a person having to.

## Bonus — boot-time self-check (dev)

Some failures are legal at boot but only *bite* later (e.g. a pubsub topic with no
transport bound fails at the first `Publish`). Under `nexus dev` / `NEXUS_DEV`,
nexus runs a **boot self-check** — the `nexus.toml` config lint plus every
`nexus.RegisterBootCheck(...)` topology check — and prints findings to stderr at
startup. Advisory only (never aborts) and dev-only (zero cost in prod). It's how a
junior sees the problem *before* deploy. A package with a deferred-until-runtime
failure mode should register a `BootCheck` from its `init()` (see `pubsub/boot_check.go`).
