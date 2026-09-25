# Router & DI backends

The HTTP router and the DI container are both pluggable. Your code never names either
one: handlers see an `*httpx.Ctx`, and wiring goes through `nexus.Provide`, `Invoke` and
`Module`. The defaults add no third-party dependencies.

## HTTP router

The default is the standard library's `net/http.ServeMux`, so a default binary links no
gin or chi. Switch with one option:

```go
import "github.com/paulmanoni/nexus/httpx/chirouter" // or .../httpx/ginrouter

nexus.Boot(nexus.WithRouter(chirouter.New()))
```

| Backend | Package | Notes |
|---|---|---|
| stdlib (default) | `httpx/stdrouter` | No dependencies |
| chi | `httpx/chirouter` | Ships in the main module; chi itself has no dependencies |
| gin | `httpx/ginrouter` | A **separate module**, so gin stays out of your dependency graph until you `go get` it |

Route strings use `:id` and `*rest` on every backend. Middleware runs the same on every
backend, because the chain (`c.Next()`, `c.Abort()`, recovery) lives in `httpx.Ctx`, not
in the router. App-level middleware wraps the whole mux, so it runs even on a 404 or 405;
CORS preflight relies on that.

Raw handlers take an `*httpx.Ctx`, and `httpx.H` is a JSON map shorthand.
`App.Router()` exposes the live router.

## DI container

nexus has its own small container, `nexus/di`, and it is the default. A default binary
links no `go.uber.org/fx` or `dig`. To use fx:

```go
import "github.com/paulmanoni/nexus/di/fxcontainer" // a separate module

nexus.Boot(nexus.WithContainer(fxcontainer.New()))
```

Both backends pass the same parity test suite:

- constructors are lazy singletons
- invokes run eagerly, in order
- lifecycle hooks start in order and stop in reverse

The built-in container builds the graph about 60 times faster at startup.
