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

## Inside a module

A controller placed in a `nexus.Module` mounts under the module's `Path`, and its
GraphQL actions serve on the module's endpoint:

```go
nexus.Module("admin", nexus.Path("/admin"),
    nexus.Controller[*UsersController]("/users").     // GET /admin/users/:id
        Provide(NewUsersController).
        Get("/:id", (*UsersController).Show).
        Query((*UsersController).UserRows),            // POST /admin/graphql
)
```

- **The prefix is a REST prefix.** GraphQL actions never move to `<prefix>/graphql`.
  They serve on the enclosing module's endpoint, or on the app's `/graphql` outside
  a module, so the frontend's GraphQL calls don't depend on how actions are grouped.
- **Trailing slashes.** `.TrailingSlash()` registers every action at both `/users/:id`
  and `/users/:id/`. This helps when existing links use both forms, as in a Django port.

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
Calls add up: each function's options apply after the previous one's, so defaults you
add to an `inertia.Resource` extend its page rendering instead of replacing it.

## Pages from any action

`inertia.Component` renders one action as an Inertia page with any component name.
It needs no resource conventions and works on a plain `nexus.Controller`:

```go
nexus.Controller[*ListsController]("").
    Provide(NewListsController).
    Get("/longlist/:pk/view", (*ListsController).Longlist, inertia.Component("Admin/AdvertLonglist"))
```

On an `inertia.Resource` it overrides the conventional `<Folder>/<Method>` component.

`inertia.AsPage()` names the page after the action instead: `<Folder>/<Method>`, with
the folder taken from the controller type. It's the Go form of a `//@page` line
without a component:

```go
nexus.Controller[*UsersController]("/users").
    Get("/:id", (*UsersController).Show, inertia.AsPage())   // page Users/Show
```

It only works on a controller action. On a plain `AsRest` the boot fails, because
there's no action to name the page after.

## Decorator form

Controllers can be declared with [`//@` annotations](./decorators#controllers)
instead of a registration chain. Put `//@controller` on the type and annotate its
methods:

```go
// UsersController serves the users pages.
//
//@controller /users trailing-slash
//@auth Required
type UsersController struct{ users *UserService }

//@provide
func NewUsersController(users *UserService) *UsersController { … }

//@page GET /
func (c *UsersController) Index(ctx context.Context) (IndexProps, error)       // page Users/Index

//@page GET /:id/view Admin/UserDetail
//@auth Requires view_user
func (c *UsersController) Show(ctx context.Context, id int64) (ShowProps, error)

//@query
func (c *UsersController) UserRows(ctx context.Context, in RowsArgs) ([]UserRow, error)
```

The generator emits exactly the `nexus.Controller[*UsersController](…)` chain you
would write by hand.

### Annotated actions, declared in Go

You can annotate the actions and still declare the controller in Go. Leave
`//@controller` off the type and annotate its methods:

```go
type DashboardController struct{ stats *StatsService }

//@page GET / Admin/Dashboard
//@auth Required
func (c *DashboardController) Index(ctx context.Context) (IndexProps, error)

//@page GET /forbidden
//@use nexus.Public()
func (c *DashboardController) Forbidden(ctx context.Context) (ForbiddenProps, error)   // Dashboard/Forbidden

//@query
//@auth Required
func (c *DashboardController) DashboardStats(ctx context.Context) (*Stats, error)
```

A `nexus.Controller` or `nexus.Resource` for that type then serves those actions.
The module, path and gates stay in code:

```go
var Module = nexus.Module("admin",
    nexus.Path("/admin"),
    nexus.Resource[*DashboardController]("/").
        Provide(NewDashboardController),
)
```

This serves `GET /admin/`, `GET /admin/forbidden`, and `dashboardStats` on
`/admin/graphql`.

- **Paths are written as-is.** Without a `//@controller` prefix, `/` means `/`, and
  the Go controller's prefix (here `/`) goes in front of each one.
- **Actions from both places add up.** A Resource's conventional methods, the chain's
  own `Get`/`Member`/… calls and the annotated actions are all registered.
- **Without a Go declaration** the annotated actions register on their own, as a
  controller in their package's module.
- **The generated code** is one `nexus.ControllerActions` call per type. You can write
  that call by hand too:

```go
nexus.ControllerActions(func(c *nexus.ControllerRouter[*DashboardController]) {
    c.Rest("GET", "/", (*DashboardController).Index, inertia.Component("Admin/Dashboard"), auth.Required())
})
```

Only pointer-receiver methods are collected this way. A method with a value receiver
still registers on its own, as `T.M`.

### Every annotation has a Go form

| Annotation | Go |
| --- | --- |
| `//@controller /users` on the type | `nexus.Controller[*UsersController]("/users")` |
| `//@controller /users trailing-slash` | `….TrailingSlash()` |
| `//@auth` / `//@session` / `//@use` on the type | `nexus.Controller[*T]("/users", auth.Required(), …)` |
| `//@page GET /:id Admin/User` | `.Get("/:id", (*T).Show, inertia.Component("Admin/User"))` |
| `//@page GET /:id` (no component) | `.Get("/:id", (*T).Show, inertia.AsPage())` |
| `//@page GET,POST /form` | `.Get(…)` and `.Post(…)` |
| `//@rest GET /export` | `.Get("/export", (*T).Export)` |
| `//@query` / `//@mutation` | `.Query((*T).Rows)` / `.Mutation((*T).Save)` |
| `//@auth` / `//@use` on a method | options on that action: `.Get(…, auth.Requires("x"))` |
| `//@provide` on the constructor | `.Provide(NewT)` |
| annotated methods, no `//@controller` | `nexus.ControllerActions[*T](func(c) {…})` |
| `//@path /admin` on the package | `nexus.Module("admin", nexus.Path("/admin"), …)` |

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
