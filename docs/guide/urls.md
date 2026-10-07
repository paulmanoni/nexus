# Named routes & URLs

Every REST route has a name, and nexus builds its URL back from it — so a link,
a redirect or a form action never spells out a path that a module `Path`, a
router prefix or `route_prefix` can move.

```go
var ShowUser = nexus.AsRest("GET", "/users/:id", (*Users).Show, nexus.Arg("id"))

var Module = nexus.Module("users", nexus.Path("/admin"), ShowUser /* , … */)

ShowUser.URL(ctx, 7)               // "/admin/users/7"
nexus.URL(ctx, "users:show", 7)    // the same route, by name
```

## Names

A route's name is its handler's name with the first letter lowered —
`(*Users).Show` → `show`, `ListPets` → `listPets` — in the namespace of the
module it is declared in: `users:show`. Controller actions take the method's
name (`(*UsersController).Update` → `update`, for its `PUT` and `PATCH` alike).

Routers namespace their routes with their own name, stacked through `Include`
the way prefixes stack:

```go
billing := nexus.NewRouter("billing", "/billing")
billing.Rest("GET", "/invoices/:id", (*Billing).Invoice)    // v1:billing:invoice
v1 := nexus.NewRouter("v1", "/api/v1").Include(billing)
```

`nexus.Name("…")` overrides the default; `nexus.Name("")` names a route after
its namespace. Two routes given the same name with `Name` fail the boot. Two
routes whose *default* names coincide on different paths leave the name
ambiguous instead — building it says so and asks for a `Name`. A route served
with and without a trailing slash, or under several methods, is one name; it
builds the path without the slash.

A handler without a name of its own (a closure) gives its route no name unless
`Name` gives one. Extensions name what they register with `nexus.DefaultName`:

| registration | name |
|---|---|
| `view.Page("GET", "/about", About)` | `about` (the component) |
| `view.Live[*UserShow]("/users/:id")` | `userShow` (its router's namespace) |
| `auth` endpoints | `login`, `logout`, `me`, `token`, `revoke`, … |

`nexus.NoName()` keeps plumbing (an upload endpoint, a socket) out of the names.

## Building a URL

Handles build their own route:

```go
ShowUser.URL(ctx, 7)                                 // *nexus.Route (AsRest, inertia.Page, view.Page)
UserPage.URL(ctx, u.ID)                              // view.Live[*UserShow](…)
users.URL(ctx, (*UsersController).Show, 7)           // a controller and one of its actions
billing.URL(ctx, "invoice", 3)                       // a router, a name relative to it
nexus.URL(ctx, "v1:billing:invoice", 3)              // anywhere, by full name
```

Parameters fill the path and the query:

- a scalar fills the next path parameter, in path order;
- `nexus.P{"id": 7}` fills them by name;
- a struct fills them from its `path:"…"` fields and adds its non-zero
  `query:"…"` fields — the handler's own args type works;
- `nexus.Query{"tab": "roles", "tag": []string{"a", "b"}}` adds a query.

```go
nexus.URL(ctx, "users:index", nexus.Query{"q": "ada"})   // /admin/users?q=ada
ShowUser.URL(ctx, GetUser{ID: 7, Tab: "roles"})          // /admin/users/7?tab=roles
```

Values are escaped; a `*rest` parameter keeps its slashes. With
[maskid](./maskid) on, id parameters are masked the way responses mask them.

`ctx` says which app builds the URL: a request's context, on every transport
(REST, GraphQL, WebSocket, live views). Without one — a job, a startup task —
the one running app does; in a process running several, `nexus.WithApp(ctx,
app)` says which.

A route that can't be built — an unknown name, a missing parameter — panics
under `nexus dev` and in tests, with a did-you-mean for a mistyped name, and in
production logs the error and returns `"#"`. `nexus.Reverse(ctx, name, …)` and
each handle's `Reverse` return the error instead.

`Route.Method()` is the route's method, for a form:

```templ
<form method="post" action={ ChangePassword.URL(ctx) }>
```

## A page that links to its own route

A handler that renders a page linking to its own route — a form posting back,
a pager — refers to the handle its own registration builds, and Go rejects the
package variable as an initialization cycle. Declare the handles and assign
them in `init`:

```go
var LoginPage, SignIn *nexus.Route

func init() {
	LoginPage = nexus.AsRest("GET", "/login", showLogin, nexus.Public())
	SignIn = nexus.AsRest("POST", "/login", signIn, nexus.Public()) // signIn renders the form: action={ SignIn.URL(ctx) }
}
```

## Listing routes

`App.Routes()` lists the named routes. `nexus routes` shows a NAME column and
filters with `--name users:`; the dashboard's endpoint page and
`GET /__nexus/routes` show each route's name.
