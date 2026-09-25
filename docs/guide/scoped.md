# Request-scoped values

Some facts are derived from the request, such as the current tenant, a permission set or
a feature evaluation. `nexus.NewScoped` computes such a fact **at most once per
request, on first use**, and shares it with every handler, service and prop that asks.

```go
type Tenant struct {
    ID   int64
    Plan string
}

var CurrentTenant = nexus.NewScoped[Tenant](func(svc *TenantService) nexus.Compute[Tenant] {
    return func(ctx context.Context) (Tenant, error) {
        return svc.ForUser(ctx)
    }
})

nexus.Boot(CurrentTenant, ordersModule) // the handle is an Option
```

Read it anywhere, on any transport:

```go
t, err := CurrentTenant.Get(ctx)
```

Or declare it as a handler dependency:

```go
func NewListOrders(ctx context.Context, tenant *nexus.Scoped[Tenant], ...) ([]Order, error)
```

## Semantics

- **Lazy.** A value nobody asks for is never computed. An app with no scoped values
  pays nothing.
- **Once per request.** Concurrent `Get` calls, such as parallel GraphQL resolvers, share
  one computation. Errors are memoized too.
- **Per request only.** There is no TTL and no sharing between requests. Values that can
  tolerate staleness belong on the identity; values that outlive requests belong in a
  cache.

Two handles of the same type in one app conflict in DI. Mark the extras `.NoProvide()`,
or give each fact its own named type.

Don't call `Get` on a handle from inside its own compute function; that deadlocks.

## Sharing with pages

`inertia.ShareScoped("tenant", CurrentTenant)` sends the value to every Inertia page as
a shared prop. Handlers and pages then share one computation per request.

## Tests

Inject a value without booting the app:

```go
ctx = nexus.WithScopedValue(ctx, CurrentTenant, Tenant{ID: 1})
```
