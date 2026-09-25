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
| `nexus.Provide(fns...)` | Adds constructors to the DI graph |
| `nexus.ProvideService(fn)` | Provide, plus dashboard edges drawn from the constructor's parameters |
| `nexus.ProvideResources(fns...)` | Provide, plus automatic registration of resource providers |
| `nexus.Supply(vals...)` | Adds ready-made values |
| `nexus.Invoke(fn)` | Runs a startup side effect with injected parameters |
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
stop this way.

`nexus.Error(err)` reports an error while options are being built, and boot fails with
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
