# `//nexus:` decorators

As an alternative to listing every handler in a `nexus.Module(...)`, you can annotate
handlers with `//nexus:` comments and let nexus generate the registrations. The result is the
same `AsRest`/`AsQuery`/`Provide` options you would write by hand, so both styles can be
mixed.

```go
package users

import "github.com/paulmanoni/nexus/v2"

//nexus:provide
func NewUserService(app *nexus.App) *UserService {
    return &UserService{app.Service("users")}
}

type GetUserArgs struct {
    ID string `path:"id"`
}

//nexus:rest GET /users/:id
func NewGetUser(s *UserService, p nexus.Params[GetUserArgs]) (*User, error) { ... }

//nexus:mutation
//nexus:auth Requires users:write
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
| `//nexus:provide` | A constructor (`nexus.Provide`) |
| `//nexus:rest <METHOD> <PATH>` | A REST endpoint |
| `//nexus:query` / `//nexus:mutation` / `//nexus:subscription` | A GraphQL field |
| `//nexus:ws <PATH> <TYPE>` | A WebSocket message handler |
| `//nexus:worker <NAME>` | A background worker |
| `//nexus:job [queue] [timeout=D] [retry=N] [unique=D] [name=X]` | A [background job](./jobs) (`jobs.Define` on a method, `jobs.DefineFunc` on a function) |
| `//nexus:page <METHOD> <PATH> [Component]` | An Inertia page (`inertia.Page`; on a controller method the component defaults to `<Folder>/<Method>`) |
| `//nexus:controller <prefix> [trailing-slash]` | On a type: a [controller](./controllers) whose annotated methods are its actions |
| `//nexus:auth Required` / `//nexus:auth Requires PERM…` / `//nexus:auth Public` | Modifier: an auth gate (bare tokens; legacy `Requires("X")` also accepted) |
| `//nexus:session Required` | Modifier: flow-continuity gate — 428 unless the request arrived with an established session |
| `//nexus:use <expr>` | Modifier: per-op middleware |
| `//nexus:module <name>` / `//nexus:path <prefix>` / `//nexus:routeprefix <prefix>` | Package doc comment: name the module group, prefix its routes (`nexus.Path`/`nexus.RoutePrefix`) |
| `//nexus:<pkg>.<Func> args…` | A custom decorator from an extension, for example `//nexus:inertia.Page GET /users Users/Index` |

A custom decorator becomes `pkg.Func(args…, fn)`. The codegen resolves the `pkg` import
by looking, in order, at:

1. the annotated file's imports
2. the other files in the same package
3. a `[decorators.imports]` hint in `nexus.toml`
4. the module's import graph — where **your own module's packages outrank
   dependencies**: `//nexus:use utils.Wrap(...)` means the project's `utils` even when
   three dependencies ship a package by that name, with no import and no hint.
   Only a tie inside the module, or between foreign packages with no local
   candidate, asks you to disambiguate.

Package selectors inside `//nexus:use` (and type-level `//nexus:use`) expressions resolve
through the same cascade, so the annotated file needs no import — not even a
blank one — for the packages its expressions name. The import lands only in the
generated file. An identifier that names a top-level declaration of the annotated package
(`cfg.Timeout`) is recognised as a value and skipped outright — it never
reaches the module graph, and no shadowing import is synthesized for it;
anything else the cascade cannot place is left alone.

## Errors are strict and positioned

Every mistake fails at the annotation with a `file:line` your editor can jump to —
never as a compile error inside the invisible generated file:

- An **unknown keyword** is an error — the `nexus:` namespace belongs to nexus, so
  there is nothing to coexist with. A typo (`//nexus:quer`, `//nexus:Rest`,
  `//nexus:mutations`) gets a did-you-mean suggestion.
- The **v1 spelling** (`//@rest`, or gofmt's `// @rest`) is rejected with its `file:line`
  and a pointer to `nexus migrate v2`, which rewrites it. Other tools' `@`-annotations
  (swag's `// @Summary`, for one) are left alone.
- `// nexus:rest` (with a space) is not a Go directive — gofmt and go/doc treat it as
  prose — so it is rejected too; write `//nexus:rest`.
- `//nexus:rest` validates the HTTP method (and normalises case, so `//nexus:rest get /users`
  registers as `GET`) and requires the path to start with `/`. `//nexus:ws` checks its
  path the same way.
- `//nexus:query`/`//nexus:mutation`/`//nexus:subscription`/`//nexus:provide` reject stray arguments —
  the op name derives from the function; override it with `//nexus:use nexus.Op("name")`.
- `//nexus:auth` and `//nexus:use` expressions are parse-checked at the annotation.
- Known extension decorators are validated too: `//nexus:inertia.Page` takes bare tokens
  (`//nexus:inertia.Page get,post /login Login` — quoting optional, verbs case-normalised)
  and rejects a wrong arg count, a non-HTTP verb, or a bad path at the annotation.

## Auth and session gates

The `//nexus:auth` modifier reads naturally — bare tokens, capability case-insensitive:

```go
//nexus:auth Required                // auth.Required()
//nexus:auth Requires ADMIN HR       // auth.Requires("ADMIN", "HR")
//nexus:auth Public                  // nexus.Public() — the deny-by-default opt-out
```

`//nexus:session Required` attaches `session.Required()`, the flow-continuity gate: 428
Precondition Required unless the request arrived with an established session. See
[Sessions](./sessions#requiring-a-session).

## Package-level directives

A package's registration group is configured on the **package doc comment**:

```go
// Package billing handles invoicing.
//
//nexus:module billing
//nexus:path /billing
package billing
```

- `//nexus:module <name>` names the generated `nexus.Module` (default: the package name).
- `//nexus:path <prefix>` prefixes the module's REST **and** GraphQL routes (`nexus.Path`).
- `//nexus:routeprefix <prefix>` is the REST-only variant (`nexus.RoutePrefix`).

Scope is enforced both ways: a package directive on a function — or a function
directive on the package doc — is a positioned error, and two files declaring
conflicting values error naming both locations.

## Controllers

Methods can be annotated too, and the generated code calls them as method expressions
such as `(*UsersController).Show`, with the receiver supplied by DI. Without
`//nexus:controller`, a type's annotated actions go to the `nexus.Controller` or
`nexus.Resource` your code declares for it. Your code keeps the module, path and
gates, and the annotations bring the routes
([annotated actions, declared in Go](./controllers#annotated-actions-declared-in-go)).
Put `//nexus:controller <prefix>` on the type to declare the whole
[controller](./controllers) with annotations:

```go
//nexus:controller /users trailing-slash
//nexus:auth Required
type UsersController struct{ users *UserService }

//nexus:page GET /
func (c *UsersController) Index(ctx context.Context) (IndexProps, error)

//nexus:page GET /:id/view Admin/UserDetail
//nexus:auth Requires view_user
func (c *UsersController) Show(ctx context.Context, id int64) (ShowProps, error)

//nexus:mutation
func (c *UsersController) SaveUser(ctx context.Context, in SaveUser) (*User, error)
```

- **Type-level modifiers are shared.** `//nexus:auth`, `//nexus:session` and `//nexus:use` on the type
  apply to every action.
- **Paths are relative to the prefix.** `/` or `""` is the prefix itself.
  `trailing-slash` registers each route at both `/x` and `/x/`.
- **An action may map to several routes** with more than one `//nexus:page` or `//nexus:rest`
  line. A plain function still registers exactly once.
- **Actions take** `//nexus:page`, `//nexus:rest`, `//nexus:query` and `//nexus:mutation`. `//nexus:inertia.Page`
  on an action reads as `//nexus:page`.
- **The controller is its own router,** so an action can't take `//nexus:on`.
- **The constructor still needs `//nexus:provide`** (or any other provider).
- **Directives may sit anywhere in the doc comment.** gofmt moves `//nexus:` lines
  below the prose; every line is read.

## Routers (FastAPI-style)

For grouping beyond one-module-per-package, declare **routers**: named groups
with stacking prefixes, shared gates, and cross-package membership.

```go
// Package api.
//
//nexus:router v1 /api/v1
//nexus:router billing /billing parent=v1 auth=Requires(ADMIN)
package api
```

The name is optional when the router *is* the package — the same default
`//nexus:module` uses. `//nexus:router <prefix>` names the router after the package, and
every op in that package joins it automatically:

```go
// Package billing.
//
//nexus:router /billing parent=v1
package billing

//nexus:rest GET /invoices
func NewListInvoices(...) (...)   // joins "billing" — no //nexus:on needed
```

(One package-named router per package; it replaces `//nexus:module`/`//nexus:path`
there, and mixing them is an error. `//nexus:on <other>` on an op still wins.)

For cross-package routers, declare with an explicit name; any handler in any
package then joins with `//nexus:on`:

```go
//nexus:rest GET /invoices
//nexus:on billing
func NewListInvoices(...) (...)   // serves /api/v1/billing/invoices, ADMIN-gated
```

- Prefixes **stack** through `parent=`; shared `auth=` gates apply to every
  member op (parents' gates first), ahead of the op's own options.
- Each router is its own dashboard module; ops without `//nexus:on` stay on the
  package module as before.
- Strictness as usual: an unknown router name in `//nexus:on` errors with a
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
app, stop, err := nexus.InProcess(config.Runtime{},
    nexus.DecoratedModules("adverts"),   // only adverts' //nexus: registrations
    adverts.Module,
    /* the module's own deps */)
```

Only drained modules named in the list participate (decorated modules are named
after their package; `main` registers as `"app"`). `nexus.DecoratedModules()`
with no names drops every decorated registration — a boot fully isolated from
