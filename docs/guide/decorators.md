# `//@` decorators

As an alternative to listing every handler in a `nexus.Module(...)`, you can annotate
handlers with `//@` comments and let nexus generate the registrations. The result is the
same `AsRest`/`AsQuery`/`Provide` options you would write by hand, so both styles can be
mixed.

```go
package users

import "github.com/paulmanoni/nexus"

//@provide
func NewUserService(app *nexus.App) *UserService {
    return &UserService{app.Service("users")}
}

type GetUserArgs struct {
    ID string `path:"id"`
}

//@rest GET /users/:id
func NewGetUser(s *UserService, p nexus.Params[GetUserArgs]) (*User, error) { ... }

//@mutation
//@auth Requires users:write
func NewCreateUser(s *UserService, p nexus.Params[CreateUserArgs]) (*User, error) { ... }
```

`main` needs no handler list:

```go
func main() { nexus.Boot() }
```

Each annotated package becomes a dashboard module named after the package. The `main`
package is named `app`.

## Annotations

One primary annotation per function, plus optional modifiers:

| Annotation | Registers |
|---|---|
| `//@provide` | A constructor (`nexus.Provide`) |
| `//@rest <METHOD> <PATH>` | A REST endpoint |
| `//@query` / `//@mutation` / `//@subscription` | A GraphQL field |
| `//@ws <PATH> <TYPE>` | A WebSocket message handler |
| `//@worker <NAME>` | A background worker |
| `//@job [queue] [timeout=D] [retry=N] [unique=D] [name=X]` | A [background job](./jobs) (`jobs.Define` on a method, `jobs.DefineFunc` on a function) |
| `//@page <METHOD> <PATH> [Component]` | An Inertia page (`inertia.Page`; on a controller method the component defaults to `<Folder>/<Method>`) |
| `//@controller <prefix> [trailing-slash]` | On a type: a [controller](./controllers) whose annotated methods are its actions |
| `//@auth Required` / `//@auth Requires PERM…` / `//@auth Public` | Modifier: an auth gate (bare tokens; legacy `Requires("X")` also accepted) |
| `//@session Required` | Modifier: flow-continuity gate — 428 unless the request arrived with an established session |
| `//@use <expr>` | Modifier: per-op middleware |
| `//@module <name>` / `//@path <prefix>` / `//@routeprefix <prefix>` | Package doc comment: name the module group, prefix its routes (`nexus.Path`/`nexus.RoutePrefix`) |
| `//@<pkg>.<Func> args…` | A custom decorator from an extension, for example `//@inertia.Page GET /users Users/Index` |

A custom decorator becomes `pkg.Func(args…, fn)`. The codegen resolves the `pkg` import
by looking, in order, at:

1. the annotated file's imports
2. the other files in the same package
3. a `[decorators.imports]` hint in `nexus.toml`
4. the module's import graph — where **your own module's packages outrank
   dependencies**: `//@use utils.Wrap(...)` means the project's `utils` even when
   three dependencies ship a package by that name, with no import and no hint.
   Only a tie inside the module, or between foreign packages with no local
   candidate, asks you to disambiguate.

Package selectors inside `//@use` (and type-level `//@use`) expressions resolve
through the same cascade, so the annotated file needs no import — not even a
blank one — for the packages its expressions name. The import lands only in the
generated file. An identifier the cascade can't place is left alone: it may be a
package-level value of the annotated package (`cfg.Timeout`), not a package.

## Errors are strict and positioned

Every mistake fails at the annotation with a `file:line` your editor can jump to —
never as a compile error inside the invisible generated file:

- A **typo'd keyword** (`//@quer`, `//@Rest`, `//@mutations`) errors with a
  did-you-mean suggestion. Genuinely foreign `//@` keywords from other tools are
  still ignored, so coexistence is preserved.
- `//@rest` validates the HTTP method (and normalises case, so `//@rest get /users`
  registers as `GET`) and requires the path to start with `/`. `//@ws` checks its
  path the same way.
- `//@query`/`//@mutation`/`//@subscription`/`//@provide` reject stray arguments —
  the op name derives from the function; override it with `//@use nexus.Op("name")`.
- `//@auth` and `//@use` expressions are parse-checked at the annotation.
- Known extension decorators are validated too: `//@inertia.Page` takes bare tokens
  (`//@inertia.Page get,post /login Login` — quoting optional, verbs case-normalised)
  and rejects a wrong arg count, a non-HTTP verb, or a bad path at the annotation.

## Auth and session gates

The `//@auth` modifier reads naturally — bare tokens, capability case-insensitive:

```go
//@auth Required                // auth.Required()
//@auth Requires ADMIN HR       // auth.Requires("ADMIN", "HR")
//@auth Public                  // nexus.Public() — the deny-by-default opt-out
```

`//@session Required` attaches `session.Required()`, the flow-continuity gate: 428
Precondition Required unless the request arrived with an established session. See
[Sessions](./sessions#requiring-a-session).

## Package-level directives

A package's registration group is configured on the **package doc comment**:

```go
// Package billing handles invoicing.
//
//@module billing
//@path /billing
package billing
```

- `//@module <name>` names the generated `nexus.Module` (default: the package name).
- `//@path <prefix>` prefixes the module's REST **and** GraphQL routes (`nexus.Path`).
- `//@routeprefix <prefix>` is the REST-only variant (`nexus.RoutePrefix`).

Scope is enforced both ways: a package directive on a function — or a function
directive on the package doc — is a positioned error, and two files declaring
conflicting values error naming both locations.

## Controllers

Methods can be annotated too, and the generated code calls them as method expressions
such as `(*UsersController).Show`, with the receiver supplied by DI. Without
`//@controller`, a type's annotated actions go to the `nexus.Controller` or
`nexus.Resource` your code declares for it. Your code keeps the module, path and
gates, and the annotations bring the routes
([annotated actions, declared in Go](./controllers#annotated-actions-declared-in-go)).
Put `//@controller <prefix>` on the type to declare the whole
[controller](./controllers) with annotations:

```go
//@controller /users trailing-slash
//@auth Required
type UsersController struct{ users *UserService }

//@page GET /
func (c *UsersController) Index(ctx context.Context) (IndexProps, error)

//@page GET /:id/view Admin/UserDetail
//@auth Requires view_user
func (c *UsersController) Show(ctx context.Context, id int64) (ShowProps, error)

//@mutation
func (c *UsersController) SaveUser(ctx context.Context, in SaveUser) (*User, error)
```

- **Type-level modifiers are shared.** `//@auth`, `//@session` and `//@use` on the type
  apply to every action.
- **Paths are relative to the prefix.** `/` or `""` is the prefix itself.
  `trailing-slash` registers each route at both `/x` and `/x/`.
- **An action may map to several routes** with more than one `//@page` or `//@rest`
  line. A plain function still registers exactly once.
- **Actions take** `//@page`, `//@rest`, `//@query` and `//@mutation`. `//@inertia.Page`
  on an action reads as `//@page`.
- **The controller is its own router,** so an action can't take `//@on`.
- **The constructor still needs `//@provide`** (or any other provider).
- **Either comment form works.** gofmt rewrites `//@x` in a doc comment as `// @x`,
  and both are read.

## Routers (FastAPI-style)

For grouping beyond one-module-per-package, declare **routers**: named groups
with stacking prefixes, shared gates, and cross-package membership.

```go
// Package api.
//
//@router v1 /api/v1
//@router billing /billing parent=v1 auth=Requires(ADMIN)
package api
```

The name is optional when the router *is* the package — the same default
`//@module` uses. `//@router <prefix>` names the router after the package, and
every op in that package joins it automatically:

```go
// Package billing.
//
//@router /billing parent=v1
package billing

//@rest GET /invoices
func NewListInvoices(...) (...)   // joins "billing" — no //@on needed
```

(One package-named router per package; it replaces `//@module`/`//@path`
there, and mixing them is an error. `//@on <other>` on an op still wins.)

For cross-package routers, declare with an explicit name; any handler in any
package then joins with `//@on`:

```go
//@rest GET /invoices
//@on billing
func NewListInvoices(...) (...)   // serves /api/v1/billing/invoices, ADMIN-gated
```

- Prefixes **stack** through `parent=`; shared `auth=` gates apply to every
  member op (parents' gates first), ahead of the op's own options.
- Each router is its own dashboard module; ops without `//@on` stay on the
  package module as before.
- Strictness as usual: an unknown router name in `//@on` errors with a
  did-you-mean over the declared names, conflicting re-declarations name both
  locations, unknown parents and parent cycles are positioned errors.

The same model is available as plain Go — `nexus.NewRouter(name, prefix,
shared...)`, `.Rest/.Query/.Mutation/.WS/.Worker/.Provide`, and
`.Include(child)`; pass only the root router to `Boot`. See
[Modules & services](./modules#routers).

## How the wiring is generated

- **`nexus dev` and `nexus build`** generate the registrations on the fly and pass them
  to `go build` as an overlay. Nothing is written to your source tree.
- **`nexus generate handlers ./...`** writes a `nexus_handlers_gen.go` per package, plus
  an import aggregator in the main package. Commit these files when you need a bare
  `go build`, `go test` or `go install` without the CLI, or when static tools such as
  gopls and linters should see the registrations.
- **`nexus generate handlers --check`** fails when the committed files are out of date.
  Use it as a CI drift gate.

## Testing annotated modules in isolation

`nexus generate handlers` (the eject path) is what makes annotated
registrations visible to a bare `go test` — but the registry they drain from is
process-global, so a test binary whose files link several annotated packages
hands **every** package's registrations to **every** boot. An `InProcess` boot
of one module then fails on the other packages' missing providers.

Scope the boot instead:

```go
app, stop, err := nexus.InProcess(nexus.Config{},
    nexus.DecoratedModules("adverts"),   // only adverts' //@ registrations
    adverts.Module,
    /* the module's own deps */)
```

Only drained modules named in the list participate (decorated modules are named
after their package; `main` registers as `"app"`). `nexus.DecoratedModules()`
with no names drops every decorated registration — a boot fully isolated from
annotations. Booting without the option keeps the old behaviour: everything
drained participates. Boots are repeatable within one binary — the drain is a
snapshot, so test order no longer decides which boot sees the registrations.
