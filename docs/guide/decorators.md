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
//@auth Requires("users:write")
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
| `//@auth Required` / `//@auth Requires("X")` | Modifier: an auth gate |
| `//@use <expr>` | Modifier: per-op middleware |
| `//@<pkg>.<Func> args…` | A custom decorator from an extension, for example `//@inertia.Page "GET" "/users" "Users/Index"` |

A custom decorator becomes `pkg.Func(args…, fn)`. The codegen resolves the `pkg` import
by looking, in order, at:

1. the annotated file's imports
2. the other files in the same package
3. a `[decorators.imports]` hint in `nexus.toml`
4. the module's import graph

## How the wiring is generated

- **`nexus dev` and `nexus build`** generate the registrations on the fly and pass them
  to `go build` as an overlay. Nothing is written to your source tree.
- **`nexus generate handlers ./...`** writes a `nexus_handlers_gen.go` per package, plus
  an import aggregator in the main package. Commit these files when you need a bare
  `go build`, `go test` or `go install` without the CLI, or when static tools such as
  gopls and linters should see the registrations.
- **`nexus generate handlers --check`** fails when the committed files are out of date.
  Use it as a CI drift gate.
