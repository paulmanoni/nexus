# Migrating to v2

nexus 2.0 carries every breaking change in one major version. Most of the move is
mechanical and done by `nexus migrate v2`; what it can't rewrite it marks in your code
with a `// TODO(nexus v2): …` comment that says what replaces it. This page walks the
migration in order, then covers each area: what the codemod does, what it flags, and how
to finish by hand.

The full list of changes is in the [CHANGELOG](https://github.com/paulmanoni/nexus/blob/main/CHANGELOG.md);
the design and its decisions are in [docs/design/v2.md](https://github.com/paulmanoni/nexus/blob/main/docs/design/v2.md).

## Before you start

- **Go 1.26 or newer.** The v2 module requires Go 1.26.2; `go mod tidy` raises your
  `go` line to match. The CLI module requires Go 1.27.1 — with the default
  `GOTOOLCHAIN=auto`, `go install` fetches that toolchain for you.
- **The v2 CLI.** The v1 CLI doesn't know v2 projects; install the new one:

  ```bash
  go install github.com/paulmanoni/nexus/cmd/nexus/v2@latest
  nexus version
  ```

- **Optionally, v1.80 first.** nexus v1.80 is the bridge release: under `nexus dev` it
  lists at boot every v1 API your app uses that 2.0 removes or renames, with the file:line
  and the replacement, and `nexus lint --v2` reports the same from source. It also reads
  the `//nexus:x` annotation spelling, so you can switch annotations while still on v1.
- **A clean working tree.** The codemod rewrites files in place. Commit first so the
  diff is the migration and nothing else.

## Step by step

```bash
nexus migrate v2 --dry-run     # every change it would make, file by file
nexus migrate v2               # rewrite the tree (re-running is a no-op)
go mod tidy                    # resolve github.com/paulmanoni/nexus/v2 and drop v1
nexus doctor                   # Go, the module on v2, nexus.toml, Node/Vite, Tailwind
nexus config check             # nexus.toml under the strict rules (exit 1 on a problem)
go build ./... && nexus test ./...
```

Then search for what's left:

```bash
grep -rn "TODO(nexus v2)" --include='*.go' .
```

Each TODO names its replacement; the sections below show the before and after.
Everything the compiler still rejects after that is a removed API — see
[Removed APIs](#removed-apis).

`nexus migrate v2` skips `vendor`, `node_modules`, `testdata` and hidden directories,
gofmt's every `.go` file it changes, and `--help` prints its whole symbol table.

## Module path and imports

The module is `github.com/paulmanoni/nexus/v2`. The separate modules take `/v2` at the
end of their own path, and `view` is now a package of the root module.

```go
// v1
import (
    "github.com/paulmanoni/nexus"
    "github.com/paulmanoni/nexus/extension/auth"
    "github.com/paulmanoni/nexus/view"
    "github.com/paulmanoni/nexus/httpx/ginrouter"
)

// v2
import (
    "github.com/paulmanoni/nexus/v2"
    "github.com/paulmanoni/nexus/v2/extension/auth"
    "github.com/paulmanoni/nexus/v2/view"
    "github.com/paulmanoni/nexus/httpx/ginrouter/v2"
)
```

**Codemod:** rewrites import specs in `.go` and `.templ` files, and the `require` /
`replace` lines in `go.mod` (to `v2.0.0`; a `…/nexus/view` requirement is dropped).
The modules that keep their own path are `cmd/nexus`, `di/fxcontainer`,
`httpx/ginrouter`, `extension/cache/redis`, `extension/jobs/jobsredis` and
`extension/jobs/jobsamqp`. Run `go mod tidy` afterwards.

## `//nexus:` directives

Annotations are Go directives. The grammar after the prefix is unchanged.

```go
// v1
//@rest GET /users/:id
//@auth Requires view_user
func (s *UserService) GetUser(ctx context.Context, id int64) (*User, error)

// v2
// GetUser returns one user.
//
//nexus:rest GET /users/:id
//nexus:auth Requires view_user
func (s *UserService) GetUser(ctx context.Context, id int64) (*User, error)
```

**Codemod:** rewrites every `//@x` and `// @x` nexus annotation in `.go` and `.templ`
files — function, type (`//nexus:controller`), package (`//nexus:module`, `//nexus:path`,
`//nexus:routeprefix`) and custom `//nexus:pkg.Func` decorators. Other tools'
`@`-annotations (swag's `// @Summary`) are left alone.

In v2 the old spelling is a `file:line` error pointing at `nexus migrate v2`, and an
unknown `//nexus:` keyword is always an error. gofmt moves directives below the doc
prose; that is fine — every line of the comment is read. See [decorators](./decorators).

## Config: package, strict nexus.toml, sections

### The `config` package

The runtime config types, the loader and `Get` left the root package.

```go
// v1
cfg := nexus.MustLoadConfig()
port := nexus.Get[int]("db.port", 5432)
var srv nexus.ServerConfig

// v2
cfg := config.MustLoad()
port := config.Get[int]("db.port", 5432)
var srv config.Server
```

| v1 | v2 |
|---|---|
| `nexus.Config`, `ServerConfig`, `WebSocketConfig`, `DashboardConfig`, `GraphQLConfig`, `MiddlewareConfig`, `SecurityConfig`, `CORSConfig`, `StoreConfig` | `config.Runtime`, `config.Server`, `config.WebSocket`, `config.Dashboard`, `config.GraphQL`, `config.Middleware`, `config.Security`, `config.CORS`, `config.Stores` |
| `LoadConfig`, `MustLoadConfig`, `DefaultConfigPath` | `config.Load`, `config.MustLoad`, `config.DefaultPath` |
| `Get`, `MustGet`, `HasConfig`, `OnConfigChange`, `BindConfig`, `ConfigVersion` | `config.Get`, `config.MustGet`, `config.Has`, `config.OnChange`, `config.Bind`, `config.Version` |
| `ConfigError`, `LintRuntimeFile` | `config.Error`, `config.LintFile` |
| `nexus.Cache` | `resource.Cache` |

**Codemod:** rewrites each selector on a nexus import (aliases followed, shadowing
locals left alone), adds the `config` import — as `nexusconfig` when `config` is already
taken in the file — and drops an import left unused.

::: tip extension/config
`github.com/paulmanoni/nexus/v2/extension/config` (the config-server extension) is
also named `config`. Where a file needs both, import the extension under another name
(`configext`).
:::

### Strict nexus.toml

v1 ignored a key nothing read; v2 fails boot on it with the line and a did-you-mean:
an unknown key, a key in the wrong table, a section nobody declared, or an
`[extensions.x]` block without a registered decoder.

```toml
# v1 — silently ignored
environment = "production"   # top level
[runtime]
addr = ":9090"               # belongs under [runtime.server]

# v2
[runtime]
environment = "production"
[runtime.server]
addr = ":9090"
```

**Codemod:** moves every single-line key the strict check pins to one table into that
table, keeping its text and comment. Typos and multi-line entries are left for
`nexus config check`, which prints each problem with its fix (`--json` for
CI). Add the schema line at the top of the file for editor completion:

```toml
#:schema https://paulmanoni.github.io/nexus/nexus.toml.schema.json
```

### App sections: `config.Section`

A table of your own must be declared, or boot fails on it:

```go
// v1: [shop] read with nexus.Get[string]("shop.currency")

// v2
type ShopConfig struct {
    Currency string  `toml:"currency"`
    TaxRate  float64 `toml:"tax_rate"`
}

var Shop = config.Section[ShopConfig]("shop", ShopConfig{Currency: "USD"})

func price(p int) string { return format(p, Shop.Get().Currency) }
```

**Codemod:** every top-level section of nexus.toml that nothing declares is declared
free-form in `main.go` — and in each package that reads it, so its tests boot too — with
a TODO:

```go
var (
	// TODO(nexus v2): give [shop] a struct: config.Section[ShopConfig]("shop"), read with .Get()
	_ = config.Section[map[string]any]("shop")
)
```

The app boots unchanged. Resolve the TODO by replacing `map[string]any` with a struct as
above; declaring the same table again with the same type shares the declaration.

`config.Get("shop.currency")` still works for a declared table. `nexus config check`
reads `config.Section` calls from your source, so it knows your tables too.

### Dotenv is loaded by nexus.toml

```go
// v1
nexus.Boot(nexus.MustLoadDotenv(), usersModule)

// v2 — .env beside nexus.toml is loaded before ${VAR}s expand
nexus.Boot(usersModule)
```

```toml
[runtime]
dotenv = ["!.env", ".env.local"]   # default [".env"]; a leading ! makes a file required
```

**Codemod:** drops `MustLoadDotenv()` / `LoadDotenvIfPresent()` from the option list
with a TODO. Resolve it by deleting the TODO (the default `.env` is loaded), or by listing
the files in `[runtime] dotenv`; mark a file that must exist with `!`. Real environment
variables still win. Outside `Boot`, `config.LoadDotenv(paths…)` /
`config.RequireDotenv(paths…)` return an error instead of being options.

### `MustLoadExtensions` returns one Option

```go
// v1
opts = append(opts, nexus.MustLoadExtensions()...)

// v2
opts = append(opts, nexus.MustLoadExtensions())
```

`LoadExtensionOptions` is `nexus.LoadExtensions`, returning `(nexus.Option, error)`.
**Codemod:** drops the `...` after `MustLoadExtensions()`; `LoadExtensionOptions` is
left to the compiler. Under `nexus.Boot` you need neither:
`Boot` loads extensions itself.

## dev, notify and resource

```go
// v1
nexus.PreserveDev("notes", store)
if nexus.IsDev() { … }
n := nexus.NewNotifier()

// v2
dev.Preserve("notes", store)
if dev.Enabled() { … }
n := notify.New()
```

| v1 | v2 |
|---|---|
| `PreserveDev`, `PreserveDevJSON`, `DevStateDir`, `DevState` | `dev.Preserve`, `dev.PreserveJSON`, `dev.StateDir`, `dev.State` |
| `IsDev`, `NexusDevEnv`, `NexusDevRootEnv` | `dev.Enabled`, `dev.Env`, `dev.RootEnv` |
| `Notifier`, `NewNotifier`, `Bus` | `notify.Notifier`, `notify.New`, `notify.Bus` |
| `Cache` | `resource.Cache` |

**Codemod:** all of these. The packages are `github.com/paulmanoni/nexus/v2/dev`,
`…/notify` and `…/resource`. `nexus.IfDev` / `nexus.IfNotDev` stay in the root.

## Op names and `nexus.Op`

v1 named an op after its handler with a `New` prefix dropped (`NewListPets` →
`listPets`). v2 uses the name as written (`NewListPets` → `newListPets`), or
`nexus.Op`. On GraphQL the op name is the field name, so this is a wire change there;
a REST op is named `METHOD /path` either way.

```go
// v1
nexus.AsQuery(NewListPets)                 // field: listPets

// v2 — what the codemod writes, keeping the wire name
nexus.AsQuery(NewListPets, nexus.Op("listPets"))

// v2 — the documented shape: a method, named after itself
func (s *PetService) ListPets(ctx context.Context, in ListArgs) ([]Pet, error)
nexus.AsQuery((*PetService).ListPets)      // field: listPets
```

**Codemod:** adds `nexus.Op("xxx")` to `AsQuery` / `AsMutation` / `AsSubscription`
registrations whose handler is `NewXxx`, and `//nexus:use nexus.Op("xxx")` to
`NewXxx` functions annotated `//nexus:query`, `//nexus:mutation` or
`//nexus:subscription`. Registrations that already call `nexus.Op` are left alone.
GraphQL field names, the generated SDK and `auth.OpGates` keys therefore don't move.

Functions with DI parameters (`NewXxx(svc, db, p nexus.Params[T])`) are still valid
handlers. To drop the `nexus.Op` later, rename the function (or move it onto a service
as a method) and remove the option in the same change.

## `AsCRUD` → `nexus.Resource`

```go
// v1
nexus.AsCRUD[Pet](NewPetStore)

// v2
type PetsController struct{ store *PetStore }

func NewPetsController(s *PetStore) *PetsController { return &PetsController{store: s} }

func (c *PetsController) Index(ctx context.Context) ([]Pet, error)            { … }
func (c *PetsController) Show(ctx context.Context, id int64) (*Pet, error)    { … }
func (c *PetsController) Create(ctx context.Context, in PetInput) (*Pet, error) { … }
func (c *PetsController) Update(ctx context.Context, id int64, in PetInput) (*Pet, error) { … }
func (c *PetsController) Destroy(ctx context.Context, id int64) error         { … }

nexus.Resource[*PetsController]("/pets").Provide(NewPetsController)
```

`Resource` registers Index `GET /`, Show `GET /:id`, Create `POST /`, Update
`PUT`+`PATCH /:id` and Destroy `DELETE /:id` — the routes `AsCRUD` generated and the
client's `nx.crud` calls. Define only the actions you need.

**Codemod:** a TODO on each `AsCRUD[` call. See [controllers](./controllers).

## `AsRestHandler` → `AsRest`

`AsRest` takes the factory `AsRestHandler` took: a function of DI dependencies returning
`httpx.HandlerFunc`, built once at boot.

```go
// v1
nexus.AsRestHandler("POST", "/files", func(s *Store) httpx.HandlerFunc {
    return func(c *httpx.Ctx) { … }
})

// v2
nexus.AsRest("POST", "/files", func(s *Store) httpx.HandlerFunc {
    return func(c *httpx.Ctx) { … }
})

// v2 — or a raw handler, whose parameters are filled per request
nexus.AsRest("POST", "/files", func(s *Store, c *httpx.Ctx) { … })
```

**Codemod:** renames `AsRestHandler` to `AsRest`.

## The error model

One error type, one mapping per transport.

```go
// v1
errs := nexus.NewErrors()
errs.Field("email", "already taken")
if errs.Any() { return nil, errs }
return nil, nexus.ErrForbidden

// v2
errs := nexus.Invalid()
errs.Field("email", "already taken")
if errs.Any() { return nil, errs }
return nil, nexus.Forbidden

return nil, nexus.Err(nexus.NotFound, "user not found")
return nil, nexus.Errf(nexus.Conflict, "order %d is closed", id)
if errors.Is(err, nexus.NotFound) { … }
```

The codes are `InvalidInput` (422), `Unauthenticated` (401), `Forbidden` (403),
`NotFound` (404), `Conflict` (409), `TooMany` (429), `Unavailable` (503) and `Internal`
(500). A `Code` is itself an error. REST answers every error with the code's status and
`{code, message, errors}`; GraphQL carries `extensions.code`; WebSocket sends `{type,
code, message, errors}`. An error without a code is `Internal`, its message hidden
outside `nexus dev` — wrap user-facing failures with a code.

**Codemod:** `nexus.NewErrors()` → `nexus.Invalid()`, `nexus.Errors` →
`nexus.Error` (`Field`, `Global`, `Any` and `First` are unchanged),
`nexus.ErrForbidden` → `nexus.Forbidden`, and the boot option `nexus.Error(err)` →
`nexus.FailBoot(err)`.

**TODO — `MapCRUDError`:** there is no per-error mapping any more. Where you returned
a mapped status, return a coded error; where you read the status, ask the error:

```go
// v1
if status, ok := nexus.MapCRUDError(err); ok { c.JSON(status, …) }

// v2
nexus.WriteError(c, err)                     // on a raw *httpx.Ctx: status + body
status := nexus.ErrorOf(err).HTTPStatus()    // or just the status
```

**Validation on every transport.** `validate:` tags now run on REST too (v1 enforced
them on GraphQL only), after binding; a failure — or a binding failure — is a 422
`InvalidInput` with per-field messages. Check REST clients that sent values the tags
reject. `nexus.Validate(v)` runs the same check by hand. `ErrCRUDValidation` is a 422.

See [handlers](./handlers) and [forms](./forms).

## App-wide middleware: `nexus.Middleware`

`Config.Middleware.Global` is gone. App-wide middleware is an option, may be a DI
constructor, and is placed by stage rather than slice position.

```go
// v1
cfg := nexus.MustLoadConfig()
cfg.Middleware.Global = append(cfg.Middleware.Global,
    middleware.Middleware{Name: "compress", Gin: compress})
nexus.Run(cfg, opts...)

// v2
var Compress = middleware.Middleware{
    Name:  "compress",
    Stage: middleware.Edge,   // Edge, Session, Auth, App (the default)
    HTTP:  compress,
}

nexus.Boot(
    nexus.Middleware(Compress, NewThemeHead),   // values or constructors
    usersModule,
)

func NewThemeHead(doc *nexus.Document) middleware.Middleware { … }
```

Within a stage, declaration order holds across modules. **`middleware.Middleware.Gin`
is renamed `HTTP`**: the codemod rewrites `middleware.Middleware{Gin: …}` literals; a
field read through a variable (`mw.Gin`) is left to the compiler.

**Codemod:** a TODO on each `Middleware.Global` line. Move each middleware into a
`nexus.Middleware(...)` option, give it a `Stage`, and if `cfg` is left with only keys
nexus.toml can hold, replace `MustLoadConfig` + `Run` with `nexus.Boot`.

## `nexus.Setup` for pre-serve work

```go
// v1 — ordering carried by where the Invoke sits
nexus.Invoke(resources.EnsureIndexes)

// v2
nexus.Setup(resources.EnsureIndexes, resources.SeedRoles)

func EnsureIndexes(ctx context.Context, db *MainDB) error { … }
```

`Setup` functions take DI parameters (and the boot context), return nothing or an error,
and run after every resource and worker has started and before the listeners open, in
declaration order; the first error stops boot. Listeners now start last and stop first.

**Codemod:** a TODO on `nexus.Invoke(x.Ensure…)` and on `Invoke` of `Migrate*`,
`Seed*` and `Backfill*` functions. `Invoke` still works for eager construction.

## `nexus.Frontend` and `*nexus.Document`

```go
// v1
nexus.ServeFrontend(webFS, "web/dist")
shell, _ := webFS.ReadFile("web/dist/index.html")   // stale under nexus dev

// v2
nexus.Frontend(webFS, "web/dist")

func NewThemeHead(doc *nexus.Document) middleware.Middleware {
    // doc.Get(ctx) → the built index.html in production, Vite's live one under nexus dev
}
```

**Codemod:** `ServeFrontend` → `Frontend`. Reading `index.html` from the embed is
yours to replace: take `*nexus.Document` as a parameter.

## Client IP

```go
// v1
ip := nexus.ClientIPFromCtx(ctx)   // or ratelimit.ClientIPFromCtx

// v2 — on REST, GraphQL and WebSocket alike
ip := nexus.ClientIP(ctx)
```

The address honours `[runtime.server] trusted_proxies`. **Codemod:**
`ClientIPFromCtx` → `nexus.ClientIP`; `WithClientIP` (root and `extension/ratelimit`)
gets a TODO — delete the call, the framework sets the address on every request.

## GraphQL behind a seam

No graphql-go type appears in the public API. `graph` and `transport/gql` are internal;
what a handler or middleware sees of a resolve is in the `gql` package
(`github.com/paulmanoni/nexus/v2/gql`).

```go
// v1
func Audit(next graph.FieldResolveFn) graph.FieldResolveFn {
    return func(p graph.ResolveParams) (any, error) { return next(p) }
}

// v2
func Audit(next gql.Resolver) gql.Resolver {
    return func(f gql.Field) (any, error) { return next(f) }
}

nexus.AsQuery((*OrderService).SearchOrders, nexus.GraphMiddleware("audit", "logs resolves", Audit))
```

**Codemod:** `graph.FieldMiddleware` → `gql.Middleware`, `graph.FieldResolveFn` →
`gql.Resolver`, `graph.ResolveParams` → `gql.Field`, `SetStatusCode` →
`nexus.SetGraphStatus`. TODOs mark the removed names:

- **Validators** (`graph.Required`, `StringLength`, `IntRange`, `OneOf`, `StringMatch`,
  `Custom`, `nexus.WithArgValidator`) — tag the args field (`validate:"required"`,
  `validate:"len=3|120"`, `validate:"int=1|100"`, `validate:"oneof=a|b"`), or check in
  the handler and return `nexus.Invalid().Field(…)`.
- **`RegisterGqlType`** takes a Go type: `nexus.RegisterGqlType[Address]("ShippingAddress")`
  for an input object, `nexus.RegisterGqlType("Status", Status("active"), Status("archived"))`
  for an enum.
- **Hand-built schemas** (`Service.MountGraphQL`, `transport/gql.Mount`), `GqlField`,
  `GqlFieldGroup`, `graph.NewResolver` — register the fields with `nexus.AsQuery` /
  `nexus.AsMutation`.
- **`graph.GetRootInfo` / `GetRootString`** — put the value on the context.

See [transports](./transports#graphql).

## Changed defaults

### CSRF follows what the app uses

`[runtime.middleware.security] csrf` is unset by default, which now means: on once the
app uses something a browser authenticates on its own — `extension/session`, an auth
scheme reading a cookie, or Inertia — and off for a token-only API.

The client SDK and Inertia (axios) send the token already. A hand-written
`fetch` that posts with cookies must echo the `csrftoken` cookie in `X-CSRFToken`. To
keep v1's behaviour:

```toml
[runtime.middleware.security]
csrf = false    # or true to force it on
```

### Request bodies are capped at 32MB

```toml
[runtime.server]
max_body_bytes = 104857600   # raise it; -1 turns the cap off
```

Or per endpoint, for an upload that streams to storage, plus a deadline for a slow one:

```go
nexus.AsRest("POST", "/files", (*Files).Upload, nexus.MaxBody(2<<30), nexus.Timeout(10*time.Minute))
```

An over-limit JSON body is now a 413, not a 400.

## Logging is `log/slog`

nexus links no zap. `App.Logger()` returns `*slog.Logger`, and the framework provides it
into DI.

```go
// v1
nexus.Provide(zap.NewExample)
nexus.Managed("queue", func(l *zap.Logger) (*Queue, error) { … }, nil)
func NewDB(l *zap.Logger) *DB { … }

// v2
nexus.Managed("queue", func(l *slog.Logger) (*Queue, error) { … }, nil)
func NewDB(l *slog.Logger) *DB { … }

nexus.Boot(nexus.WithLogger(slog.New(myHandler)), …)   // to replace the default
```

- **Remove your zap providers.** A second `*slog.Logger` provided by hand fails boot;
  use `nexus.WithLogger`. An app that prefers zap's encoder wraps a zap core in an
  `slog.Handler` (zap's `zapslog` package) and passes it to `WithLogger`.
- `db.WithLogger`, `db.Provide` and the binders, and `extension/cache`'s `NewManager`,
  `Provide`, `Bind` and `Manager.Logger()` take or return `*slog.Logger`.
- The default logger writes JSON to stdout; set its level with
  `[runtime.logging] level = "debug"`.

Not rewritten — the compiler points at each `*zap.Logger`.

## Auth

`extension/auth` was rebuilt around one interface you write, `auth.Users`, and the
whole v1 surface — resolvers, `Backend`, extractors, `Manager`, `Endpoints`,
`ErrorHandler`, `extension/oauth2`, `extension/inertia/iauth` — is gone. nexus now
issues and checks the credentials itself. The codemod renames what maps one to one and
puts a `// TODO(nexus v2)` on every other use.

```go
// v1
auth.Single(func(ctx context.Context, tok string) (*auth.Identity, error) {
    u, err := tokens.Validate(ctx, tok)
    if err != nil { return nil, err }
    return &auth.Identity{ID: u.ID, Roles: u.Roles, Extra: u}, nil
})

// v2
type Users struct{ db *DB }

func (u *Users) FindLogin(ctx context.Context, login string) (*auth.Identity, string, error) { … } // + encoded password
func (u *Users) Load(ctx context.Context, id string) (*auth.Identity, error)                   { … } // Perms from roles

auth.Module(auth.Config{Users: auth.UseUsers(NewUsers)})
```

| v1 | v2 | Codemod |
|---|---|---|
| `auth.Single`, `auth.Module(auth.Config{Authentication, Backend, …})` | `auth.Module(auth.Config{Users: auth.UseUsers(NewUsers)})` + `[auth.schemes.*]` | flagged |
| `Scheme.Resolve`, `Resolver`, `Backend`, `UseBackend`, `UserStore`, `ModelBackend`, `MemoryUserStore` | `auth.Users` (+ `SetPassword`, `CheckLogin`, `Public`); `authtest.Users` for tests | flagged |
| `auth.Bearer()`, `Cookie`, `APIKey`, `Chain`, `SessionCookie` | `[auth.schemes.*] type = "bearer" \| "session" \| "apikey" \| "jwt"` | flagged |
| tokens you issue yourself, `oauth2.Module` | `auth.SignIn` / `[auth.endpoints] token` (password, refresh_token, client_credentials) | flagged |
| `Identity{Roles, Scopes, Extra}` | `Identity{Perms, Kind, User}` — roles expand into `Perms` in `Load` | flagged |
| `Authorization{Default: Authenticated()}` | the default; `[auth] default = "public"` for the old behaviour | flagged |
| `Authority: Wildcard()`, `AnyOf`, `AllOf`, `PermissionFn` | `Perms` match with wildcards; `auth.RequiresAny`; `auth.Policy` | flagged |
| `auth.IdentityFrom(ctx)` | `auth.Current(ctx)` (nil when anonymous) | flagged |
| `auth.Subject[T]` | `auth.ID[T]` | yes |
| `auth.Optional()` | `auth.Public()` | yes |
| `nexus.Public()` | `auth.Public()` (the same option) | — |
| `Endpoints`, `LoginEndpoint`, `LogoutEndpoint`, `LoginHandler` | `[auth.endpoints]` login / logout / me / token / revoke | flagged |
| `Manager.Invalidate*` | `auth.SignOut`, `auth.Revoke`, `auth.RevokeUser` | flagged |
| `ErrorHandler`, `OnError`, `iauth.ErrorHandler` | `[auth.areas.*]` login / forbidden redirects; `nexus.Envelope` for JSON shapes | flagged |
| `CacheFor` | `[auth] cache` (per user id) | flagged |

Read the [auth guide](./auth) for the new flows, and run `nexus auth check` on your
nexus.toml once `[auth]` is written.

## Removed APIs

| v1 | v2 | Codemod |
|---|---|---|
| `nexus.UseVolume` / `App.UseVolume` | `nexus.DeclareVolume` | the function form |
| `uri:"id"` tag | `path:"id"` | yes |
| `AppFromGin` | removed | no |
| `extension.Plugin.Generate`, `extension.Generate`, `App.RegisterGenerateDriver` | removed (nothing read them) | no |
| the `crud` package, `storage/gorm`, `multi` | removed | no |
| `nexus dev --go-run`, `--frontend-cmd`; `nexus new --tooling`; `NEXUS_VITE_DEV` | removed | — |

## Views and the editor

### `nexus lsp`; views compile in memory

`nexus dev` no longer writes `*_templ.go`, `view_gen.go` or `view_imports_gen.go`; it
compiles views in memory and overlays them into the build. Point your editor's Go
language server at `nexus lsp` — it proxies gopls and opens the generated Go as editor
buffers, so code calling a templ component type-checks and jumps to definition, and
`.templ` files get diagnostics, hover and completion.

```bash
nexus test ./...     # go test through the same overlay
nexus vet ./...      # go vet likewise
```

If you committed the generated files, delete them (and keep them in `.gitignore`).
An editor on plain gopls can keep the files on disk with `nexus dev --view-files`. See
[views](./views).

### `viewtest`

Test view pages end to end without a browser:

```go
func TestBoard(t *testing.T) {
    app := nexustest.New(t, config.Runtime{}, appOptions()...)
    p := viewtest.Mount[*pets.Board](t, app)
    p.Fill("name", "Rex").Click("#add-submit")
    p.Expect("#pet-Rex").Exists()
}
```

### `view/ui`

A component kit (`github.com/paulmanoni/nexus/v2/view/ui`): Button, Field and inputs,
Tabs, Dialog, Dropdown and RowActions, DataTable, Toast, Loader, PageHeader. Load its
assets with `@ui.Script()`; vendor a component into your app with
`nexus add ui <component>`. If you wrote your own loader or toast helpers, `ui.Loading`
and `ui.Toast` replace them.

### Form fields

A textarea now follows the server's value like an input, a focused field keeps what it
shows, and a checked checkbox without a `value` binds as `true`. `view.Value(v)` marks a
field server-owned.

## Traces to OpenTelemetry

New and off by default:

```toml
[runtime.telemetry]
otlp_endpoint = "http://localhost:4318"   # spans POST to /v1/traces
service_name  = "orders"
```

## `nexus doctor` and `nexus release`

`nexus doctor` with no argument checks the project: Go against go.mod, the module on
nexus v2, nexus.toml under the strict rules, Node / the package manager / Vite, the
Tailwind CLI and generated view files. A deployment manifest on stdin is now
`nexus doctor -`.

`nexus release <version>` is the multi-module release for repositories with several
modules: it prints the plan, and `--yes` runs it.
