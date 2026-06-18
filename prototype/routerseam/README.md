# routerseam — prototype of a router-agnostic seam for nexus

A self-contained proof that nexus's gin coupling can be reduced to a ~40-line
adapter, making the router swappable (gin → chi → stdlib) without rewriting
middleware or handler code.

## The idea

The earlier analysis flagged the **middleware model** as the HIGH-difficulty
part of a router swap: every middleware uses gin's `c.Next()` / `c.Abort()`
flow control, so swapping routers normally means rewriting all of them.

This prototype sidesteps that by **moving chain execution into the seam**:

- `httpx.Ctx` owns the handler slice, the index, and `Next()` / `Abort()` —
  byte-for-byte gin's loop semantics, but ours.
- A `httpx.Router` backend does only two things: **match a path** and **return
  path params**. It never touches the chain.
- Everything the framework calls — `JSON`, `Status`, `Header`, `Param`,
  `Query`, `Error`/`Errors`, `Set`/`Get`, `IsAborted`, `Written` — is on
  `*httpx.Ctx`. Framework and middleware code names no router type.

Canonical route syntax stays gin's (`:id`, `*rest`); the chi/std adapters
translate on registration, so **existing nexus route strings are reused
verbatim**.

## Layout

```
httpx/            the seam: Router, Ctx, HandlerFunc, chain execution
  ginrouter/      gin adapter      (~45 lines)
  chirouter/      chi adapter      (~55 lines, incl. path translation)
  stdrouter/      net/http adapter (~55 lines, zero third-party deps)
app/              demo "framework + handlers" — written ONCE against httpx
  app_test.go     same suite run against all 3 backends
cmd/gin, cmd/chi  binaries for size measurement
```

## What the demo exercises

The hard-to-port gin features, all on the neutral `Ctx`:

- middleware flow: `Next` / `Abort` / `AbortWithStatusJSON`
- recovery pattern (`defer recover(); c.Next()`) catching a downstream panic
- post-`Next` error reading (`c.Errors()`), as metrics/trace middleware do
- path params (`:id`), query, JSON bind, custom status, `NoRoute` (SPA fallback)

## Results

```
go test ./app/ -v      # 18/18 pass: 6 cases × {gin, chi, std}, identical behavior

binary size (seam + adapter + demo app):
  gin backend: 10.7 MB
  chi backend:  8.3 MB   (-2.3 MB, and drops ~10 transitive modules)
```

## How this maps back to real nexus

nexus already has the **middleware-facing** half of this seam:
`middleware.RequestCtx` + the unexported `carrier` interface (header / clientIP
/ path / setHeader / reject). `httpx.Ctx` here is the **routing-facing** sibling
that's still missing. A real integration would:

1. Merge `httpx.Ctx` with `middleware.RequestCtx` (or have the carrier wrap
   `*httpx.Ctx`).
2. Change `buildEndpointChain` to return `[]httpx.HandlerFunc` instead of
   `[]gin.HandlerFunc`.
3. Replace `App.engine *gin.Engine` with `App.router httpx.Router`; keep a
   `ginrouter` default so behavior is unchanged.
4. Port the ~6 built-in middlewares (trace, metrics, cors, recovery, auth gate,
   introspection gate) by swapping `*gin.Context` → `*httpx.Ctx` (mechanical).
5. Deprecate the two public leaks: `App.Engine()` and `*gin.Context` handler
   params (offer `*httpx.Ctx` instead).

Steps 1–4 are internal and non-breaking. Step 5 is the only breaking change,
and it's the same work whether or not you ever actually swap the router.
