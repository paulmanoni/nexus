# Controllers

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

## Resources

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
- **Pages and forms instead of JSON:** [`inertia.Resource`](./inertia#resources) adds
  `New` and `Edit` pages and turns writes into redirects, custom actions included.

## Custom actions

Any method of the controller can be an action, conventional name or not. `Member`
mounts it under a record, `Collection` under the resource, and the verb methods
anywhere below the prefix:

```go
func (c *UsersController) Suspend(ctx context.Context, id int64, in SuspendInput) error
func (c *UsersController) Search(ctx context.Context, q SearchArgs) ([]User, error)
func (c *UsersController) Export(ctx context.Context) (*Report, error)

nexus.Resource[*UsersController]("/users").
    Provide(NewUsersController).
    Member("POST", "suspend", (*UsersController).Suspend).  // POST /users/:id/suspend
    Collection("GET", "search", (*UsersController).Search). // GET  /users/search
    Get("/reports/export", (*UsersController).Export)       // GET  /users/reports/export
```

Custom actions bind parameters, run gates and call `Authorize` (with the method
name, `"Suspend"`) exactly like the conventional ones.

### Defaults for every action

`ActionDefaults` sets the options each REST action starts from, chosen per action from
its verb, path and method name. It covers actions registered before and after the
call. Options passed to the action itself still win, and `nexus.NoActionDefaults()`
exempts one action entirely:

```go
nexus.Controller[*ReportsController]("/reports").
    ActionDefaults(func(method, path, action string) []nexus.RestOption {
        if method == "GET" {
            return []nexus.RestOption{nexus.WithIcon("file-text")}
        }
        return []nexus.RestOption{auth.Requires("reports:write")}
    }).
    Get("", (*ReportsController).Index).
    Post("/rebuild", (*ReportsController).Rebuild).
    Get("/health", (*ReportsController).Health, nexus.NoActionDefaults())
```

This hook is how [`inertia.Resource`](./inertia#resources) makes a custom GET render
the page `<Folder>/<Method>` and a custom write redirect back, so its custom actions
behave like its conventional ones.

## Authorizing actions

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
