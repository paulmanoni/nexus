# Modules & services

## Modules

A module is a named group of registrations. Its name is stamped on every endpoint inside
it, and the dashboard's architecture graph groups by module.

```go
var Module = nexus.Module("billing",
    nexus.Path("/billing"),                 // REST prefix + GraphQL mount, in one
    nexus.Provide(NewBillingService),
    nexus.AsRest("POST", "/charge", NewCharge),
    nexus.AsQuery(NewListInvoices),
)

func main() {
    nexus.Boot(billing.Module, users.Module)
}
```

| Option | Does |
|---|---|
| `nexus.Provide(fns...)` | Adds constructors to the DI graph; a service wrapper's dependencies become dashboard edges and a resource provider is registered automatically |
| `nexus.Supply(vals...)` | Adds ready-made values |
| `nexus.Setup(fns...)` | Work that must finish before the app serves — migrations, indexes, seeds. Runs after resources start, before the listeners open, in order; an error stops boot |
| `nexus.Middleware(entries...)` | App-wide middleware (values or DI constructors), placed by `middleware.Stage` |
| `nexus.Invoke(fn)` | Runs a function eagerly at startup with injected parameters |
| `nexus.Options(opts...)` | Bundles several options into one |
| `nexus.Path("/x")` | Prefixes the module's REST routes and mounts its service's GraphQL at `/x/graphql` |
| `nexus.RoutePrefix("/x")` | Prefixes REST routes only |

## Dependency injection

Constructors declare what they need as parameters, and the container supplies it:

```go
func NewOrderService(db *DB, mail *Mailer) *OrderService {
    return &OrderService{db: db, mail: mail}
}
```

- Constructors are **lazy singletons**: each runs once, and only when something needs its
  result.
- `Invoke` functions run eagerly, in registration order.
- A missing dependency or a cycle fails at boot with the full chain.

### Lifecycle hooks

Take a `nexus.Lifecycle` to run code on start and stop:

```go
func NewPoller(lc nexus.Lifecycle, db *DB) *Poller {
    p := &Poller{db: db}
    lc.Append(nexus.Hook{
        OnStart: func(ctx context.Context) error { return p.Start() },
        OnStop:  func(ctx context.Context) error { return p.Stop() },
    })
    return p
}
```

Databases, caches, workers, crons and the HTTP listeners all register their start and
stop this way. The listeners start last and stop first.

### Setup: work before the app serves

Migrations, roles, indexes and seeds go in `nexus.Setup`. Each function's parameters
come from DI (a `context.Context` gets the boot context), and it returns nothing or an
error:

```go
var Module = nexus.Module("resources",
    db.BindFromConfig[MainDB]("main", db.WithDefault()),
    nexus.Setup(EnsureIndexes, SeedRoles),
)

func EnsureIndexes(ctx context.Context, db *MainDB) error { … }
```

Setup functions run after every resource and worker has started and before the
listeners open, in declaration order. The first error stops boot, naming the function.

### App-wide middleware

`nexus.Middleware` registers middleware that runs on every request — REST, GraphQL,
WebSocket upgrades and unmatched paths. An entry is a `middleware.Middleware` or a
constructor returning one, with parameters from DI:

```go
var Compress = middleware.Middleware{Name: "compress", Stage: middleware.Edge, HTTP: compress}

func NewThemeHead(doc *nexus.Document) middleware.Middleware { … }

nexus.Boot(nexus.Middleware(Compress, NewThemeHead), usersModule)
```

`Stage` places it — `middleware.Edge` (outermost), then the framework's own (CORS,
security headers, rate limit), then `Session`, `Auth` and `App` (the default) — and
declaration order holds within a stage, across modules.

`nexus.FailBoot(err)` reports an error while options are being built, and boot fails with
it.

The container is built in and has no dependencies. `go.uber.org/fx` is available as an
alternative. See [Router & DI backends](./backends).

## Services

A service is a typed wrapper around `*nexus.Service`. The container routes by its type,
and the dashboard groups handlers under it:

```go
type BillingService struct{ *nexus.Service }

func NewBillingService(app *nexus.App) *BillingService {
    return &BillingService{app.Service("billing").Describe("Invoices and payments")}
}
```

A handler belongs to a service when the service is its first dependency:

```go
func NewCharge(svc *BillingService, p nexus.Params[ChargeArgs]) (*Receipt, error)
```

When a handler doesn't take the service, pin it with
`nexus.OnService[*BillingService]()`. Single-service apps can skip services entirely.

Services can declare the resources they use, which draws edges on the dashboard:

```go
app.Service("billing").Using("main", "cache")
```

## Routers

`nexus.Router` is a FastAPI-style registration group: a first-class value with
a URL prefix and shared per-op options, nestable through `Include`:

```go
billing := nexus.NewRouter("billing", "/billing", auth.Required())
billing.Rest("GET", "/invoices", NewListInvoices)
billing.Query(NewInvoiceStats)

v1 := nexus.NewRouter("v1", "/api/v1")
v1.Include(billing) // billing mounts at /api/v1/billing

nexus.Boot(v1) // a *Router is an Option; pass only the root
```

Prefixes stack under `Include`, and shared options — any `MiddlewareOption`:
`auth.Required()`, `auth.Requires(...)`, `session.Required()`, `nexus.Use(...)`
— inherit downward, running in declaration order ahead of each op's own
options. Each router appears as its own dashboard module. Passing an included
router (rather than the root) to `Boot` is a boot error, as is mounting the
same router twice.

The decorator form (`//nexus:router` + `//nexus:on`) builds on the same machinery — see
[`//nexus:` decorators](./decorators#routers-fastapi-style).

## Controllers

A controller groups related actions on one struct: its constructor brings the
dependencies, its methods are the actions, and the whole controller is one dashboard
module. `nexus.Controller` and `nexus.Resource` are routers bound to that type — see
[Controllers](./controllers).
