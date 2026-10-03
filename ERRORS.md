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
- Deferred build errors go through `nexus.Raw(di.Error(...))` / `nexus.FailBoot(err)`
  so they surface at boot with the same prefix.
- `config.MustLoad` / `MustLoadExtensions` panic by documented contract.

Rule when adding one: wrap with context — `panic(fmt.Errorf("nexus: <what> %q: %w", x, err))`.
A bare `panic(err)` that drops a junior into a raw toml/parse stack with no
`nexus:` breadcrumb is a bug.

## Layer 2 — Request errors → **one model, one table per transport**

A handler returning `(T, error)` with a non-nil error is normal control flow, not
a crash. Every failed request — a handler's error, a binding or `validate:` tag
failure, an auth gate, a rate limit — is a `*nexus.Error`:

```go
type Error struct {
    Code    Code                // one of the eight below
    Message string              // shown to the client
    Fields  map[string][]string // per-field messages (InvalidInput); global under "_global"
    Cause   error               // errors.Is / errors.As see through it
}

return nil, nexus.Err(nexus.NotFound, "user not found")
return nil, nexus.Errf(nexus.Conflict, "email taken: %w", err)
return nil, nexus.Invalid().Field("email", "already taken").Global("try again")
return nil, nexus.Forbidden // a bare Code is an error too
```

`nexus.ErrorOf(err)` maps any error onto the model, once per failed request, and
each transport renders the result through one table:

| Code | REST | GraphQL `extensions.code` |
|---|---|---|
| `InvalidInput` | 422 | `INVALID_INPUT` (+ `errors`) |
| `Unauthenticated` | 401 | `UNAUTHENTICATED` |
| `Forbidden` | 403 | `FORBIDDEN` |
| `NotFound` | 404 | `NOT_FOUND` |
| `Conflict` | 409 | `CONFLICT` |
| `TooMany` | 429 | `TOO_MANY_REQUESTS` |
| `Unavailable` | 503 | `UNAVAILABLE` |
| `Internal` | 500 | `INTERNAL` |

| Transport | Rendering | Where |
|-----------|-----------|-------|
| REST      | the code's status, body `{"code", "message", "errors"}` | `WriteError` (`errors_transport.go`) |
| GraphQL   | an `errors` entry with `extensions.code` (+ `errors`) | `graphqlError`, the field's outermost error mapper |
| WebSocket | an `error` event `{type, code, message, errors}`; the connection stays open | `wsErrorEvent` |
| Middleware | `rc.Reject(status, err)`: the error's code, else the status's | `middleware.ErrorBody`, `middleware.Rejection` |
| Inertia   | `InvalidInput` → flash + 303 back; anything else → `ErrorPage` at the code's status | `extension/inertia` `RenderError` |
| Views     | `InvalidInput` → re-render, `view.Errors(ctx)`; else the event's error | `view` `event` |

Rules:
- **An error without a code is `Internal`.** Its message is shown under `nexus dev`
  (`dev.Enabled()`) and replaced by `internal error` otherwise, so a driver or
  SQL error never reaches a client in production. The original stays on `Cause`
  and on the dashboard trace.
- **Validation runs on every transport.** Arguments are bound, then their
  `validate:` tags checked (`nexus.Validate`); a decode failure or a failing tag
  is an `InvalidInput` with a message per field, before the handler runs.
- **`Envelope` receives the mapped `*nexus.Error`**, so the wrap's `err.Error()`
  is safe to put on the wire.
- An extension answering a request itself renders through the same table:
  `nexus.WriteError(c, err)`, or `rc.Reject(status, err)` in middleware.

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
| REST / GraphQL | `recoveryMiddleware` (`middleware.go`, global — `/graphql` is an HTTP route) |
| WebSocket      | `callWSHandler` (`websocket.go`)             |
| Workers        | `runWorker` (`workers.go`)                  |
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
failure mode should register a `BootCheck` from its `init()` (see `pubsub/boot.go`).
