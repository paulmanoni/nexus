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

The decorator form (`//@router` + `//@on`) builds on the same machinery — see
[`//@` decorators](./decorators#routers-fastapi-style).

## Controllers

A controller groups related actions on one struct. Its dependencies come through its
constructor, its methods are the actions, and the whole controller is one dashboard
module. `nexus.Controller` is a router bound to that type:

```go
type UsersController struct{ users *UserService }

func NewUsersController(users *UserService) *UsersController { … }

func (c *UsersController) Show(ctx context.Context, id int64) (*User, error)
func (c *UsersController) Suspend(ctx context.Context, id int64, in SuspendInput) error

nexus.Controller[*UsersController]("/users", auth.Required()).
    Provide(NewUsersController).
    Get("/:id", (*UsersController).Show).
    Post("/:id/suspend", (*UsersController).Suspend, auth.Requires("users:suspend"))
```

- **Actions are method expressions** on the controller type; anything else is a boot
  error.
- **Path parameters bind by position.** `Show(ctx, id int64)` gets the route's `:id`
  with no `nexus.Arg`, and a trailing struct is the body. Passing an explicit
  `nexus.Arg` overrides this.
- **Verb methods:** `Get`, `Post`, `Put`, `Patch`, `Delete` and `Rest`. GraphQL uses
  `Query` and `Mutation`; these are named after the method and need an explicit
  `nexus.Arg` for scalars. `Provide` or `Supply` adds the controller itself.

### Resources

`nexus.Resource` registers the conventional actions the controller defines:

| Method | Route |
|---|---|
| `Index` | `GET /users` |
| `Show` | `GET /users/:id` |
| `Create` | `POST /users` |
| `Update` | `PUT` and `PATCH /users/:id` |
| `Destroy` | `DELETE /users/:id` |

```go
nexus.Resource[*UsersController]("/users", auth.Required()).
    Provide(NewUsersController).
    Member("POST", "suspend", (*UsersController).Suspend).  // POST /users/:id/suspend
    Collection("GET", "search", (*UsersController).Search)  // GET  /users/search
```

- **Missing methods mean missing routes**, so a read-only resource defines only
  `Index` and `Show`.
- **Nesting works too:** a resource at `/posts/:postId/comments`, or one included in a
  router with that prefix, gives its actions both parameters, as in
  `Show(ctx, postID, id int64)`.

### Authorizing actions

A controller that implements `nexus.ActionAuthorizer` has `Authorize` called before
every action. It runs after the router's gates, with the request context and the
method name:

```go
func (c *UsersController) Authorize(ctx context.Context, action string) error {
    if action == "Destroy" && !auth.Can(ctx, "users:delete") {
        return errors.New("you cannot delete users")
    }
    return nil
}
```

- **Refusal is a normal action error.** A refused action ends through the action's
  usual error path: a REST 403 (`nexus.ErrForbidden`), a GraphQL error, or the Inertia
  [error page](./inertia#error-pages).
- **Errors that already mean something keep that meaning.** `nexus.ErrCRUDNotFound`
  stays a 404, and `nexus.Errors` stays a validation response.
- **Actions must return an error.** If an action takes no `context.Context`, the hook
  still gets one.
