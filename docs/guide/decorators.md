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
4. the module's import graph

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

## How the wiring is generated

- **`nexus dev` and `nexus build`** generate the registrations on the fly and pass them
  to `go build` as an overlay. Nothing is written to your source tree.
- **`nexus generate handlers ./...`** writes a `nexus_handlers_gen.go` per package, plus
  an import aggregator in the main package. Commit these files when you need a bare
  `go build`, `go test` or `go install` without the CLI, or when static tools such as
  gopls and linters should see the registrations.
- **`nexus generate handlers --check`** fails when the committed files are out of date.
  Use it as a CI drift gate.
