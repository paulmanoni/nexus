# Configuration

## `nexus.Boot` and `nexus.toml`

Most apps start with one call:

```go
func main() {
    nexus.Boot(usersModule, ordersModule)
}
```

`nexus.Boot(opts...)` loads `nexus.toml` from the working directory, then runs the app.
It reads:

- the `[runtime]` tables, which become the runtime `config.Runtime`
- every `[extensions.*]` block
- the `[env]` bridge
- the value store behind `config.Get`

A missing `nexus.toml` is fine, and defaults apply. A malformed one — or one with a key
nothing reads (see [Strictness](#strictness)) — fails boot with the file, line and fix.
Point at a different file with the `NEXUS_CONFIG` environment variable, or call
`nexus.BootFrom(path, opts...)`.

If you prefer to build the config in Go, use `nexus.Run` with a `config.Runtime`
from the `config` package (`github.com/paulmanoni/nexus/v2/config`):

```go
nexus.Run(config.Runtime{
    Server:        config.Server{Addr: ":8080"},
    Dashboard:     config.Dashboard{Enabled: true, Name: "Shop"},
    Introspection: true,
}, usersModule)
```

`Boot` is shorthand for
`nexus.Run(config.MustLoad(), append(nexus.MustLoadExtensions(), opts...)...)`.

::: warning Runtime keys live under `[runtime]`
Every runtime key belongs in `[runtime]` or a `[runtime.<sub>]` table. A runtime key at
the top level fails boot (`environment` at the top level → `[runtime] environment`).
`[databases.*]`, `[extensions.*]` and `[env.*]` are top-level tables of their own.
:::

## A typical file

```toml
[runtime]
environment   = "development"   # NEXUS_ENVIRONMENT overrides it
introspection = true            # opens /__nexus (off by default)

[runtime.server]
addr           = ":8080"
max_body_bytes = 104857600      # default 32MB; -1 turns the cap off

[runtime.dashboard]
enabled = true
name    = "Shop"

[databases.main]
driver   = "postgres"
host     = "localhost"
port     = "5432"
user     = "postgres"
password = "${DB_PASSWORD}"     # ${ENV} is expanded at load
name     = "shop"
default  = true
```

Every key is documented in the [nexus.toml reference](/reference/nexus-toml).

## Environment

`environment` is `development`, `staging` or `production`. Scaffolds ship
`environment = "development"`, which turns on dev-only behaviour:

- pages follow a running Vite dev server
- the typed client SDK is written into `web/sdk`

Deployments set **`NEXUS_ENVIRONMENT=production`**, which overrides the file, so you
never edit `nexus.toml` to ship. Go code can read the resolved value with
`nexus.ActiveEnvironment()`.

## Strictness

`nexus.toml` is strict. Every table in it has an owner that declares it, and every key in
a declared table is one its type has. Boot fails — in development and production alike —
on:

- **a key at the top level**, outside any table: `environment = "production"` above
  `[runtime]` reports *did you mean [runtime] environment?*
- **an unknown key in a declared table**: `[runtime.server] adress` lists what
  `[runtime.server]` accepts; `enabeld` suggests `enabled`
- **an unknown table under a declared one**, such as `[runtime.inertia]`
- **an undeclared section**: a top-level table nobody declared, such as `[shop]` before the
  app declares it
- **an `[extensions.x]` block** whose extension package isn't imported

The error is printed as a diagnostic block with each problem's `file:line` and fix.

The owners are nexus itself (`[runtime]`, `[databases]`, `[env]`, `[extensions]`,
`[decorators]` and the deploy-manifest tables), the framework extensions (`[cache]`,
`[storage]`, `[mail]` and `[jobs]`, declared when the app imports `extension/cache`,
`extension/storage`, `extension/mail` or `extension/jobs`), extension decoders for
`[extensions.<name>]`, and the app.

## Your own sections: `config.Section`

An app declares its own tables with `config.Section[T](name, default...)`, usually as a
package-level variable:

```toml
[shop]
currency  = "EUR"
page_size = 50
timeout   = "30s"

[shop.payments]
provider = "card"
```

```go
import "github.com/paulmanoni/nexus/v2/config"

type ShopConfig struct {
    Currency string        `toml:"currency"`
    PageSize int           `toml:"page_size"`
    Timeout  time.Duration `toml:"timeout"`   // a Go duration string
    Payments struct {
        Provider string `toml:"provider"`
    } `toml:"payments"`
}

var Shop = config.Section[ShopConfig]("shop", ShopConfig{Currency: "USD", PageSize: 20})

func pageSize() int { return Shop.Get().PageSize }
```

- The declaration is what makes `[shop]` legal in the file, and the table is checked
  against `ShopConfig`: a key it has no field for fails boot like any other.
- `Get()` returns the table decoded over the default (a key missing from the file keeps
  its default). `Present()` reports whether the file has the table at all.
- Keys come from `toml` tags (else the lowercased field name). `time.Duration` fields read
  Go duration strings.
- `T` may be a map: `config.Section[map[string]Store]("stores")` declares
  `[stores.<name>]` tables. `config.Section[map[string]any]("flags")` declares a
  free-form table whose keys aren't checked.
- A name may be declared once. Declaring a framework table (`jobs`, `cache`, …) or a
  name declared elsewhere panics at init.

## Reading values: `config.Get`

Any value in `nexus.toml` is also readable by its dotted path, with an environment
override:

```go
currency := config.Get[string]("shop.currency")
size     := config.Get[int]("shop.page_size", 20)          // second arg is the default
ttl      := config.Get[time.Duration]("shop.timeout", 5*time.Minute)
```

`config.Get` reads declared tables like any other; declaring a section is what lets it be
in the file, and `Get` keeps working on it. (A key that comes only from an environment
variable or the config server needs no declaration.)

Values resolve per key, highest priority first:

1. **An environment variable.** `shop.page_size` is overridden by `SHOP_PAGE_SIZE`.
   (A section handle's `Get()` returns the file's values as decoded at boot.)
2. **The config server snapshot**, when `[extensions.config]` is wired. Its values are
   hot-reloadable and can come from a remote server.
3. **`nexus.toml`.**

Plain file reads need no extension. Wire
[`extension/config`](https://github.com/paulmanoni/nexus/blob/main/extension/config/README.md)
only when you need hot reload, profiles or a remote config server.

## The `[env]` bridge

Tables under `[env]` become process environment variables. The same values can also be
used by the frontend:

```toml
[env.client]
id  = "shop-web"
url = "${PUBLIC_URL}"
```

- **Go:** `os.Getenv("client.id")`. Nested tables flatten with dots.
- **Frontend:** `import.meta.env.client.id`. When `nexus dev` or `nexus build` starts
  Vite, each exact reference like this is replaced with its value. Only keys you
  reference reach the browser. The values are never added to the `import.meta.env`
  object, so the bracket form (`import.meta.env["client.id"]`) and whole-object access
  see none of them.

::: danger Only public values
A value the frontend references ships in the JavaScript bundle. Reference client-public
data only (an OAuth client id, a public URL). Never reference a server secret.
:::

## Checking a file

```sh
nexus config check            # ./nexus.toml, or $NEXUS_CONFIG
nexus config check deploy/nexus.toml --json
```

`nexus config check` applies the boot rules — undeclared tables, unknown and misplaced
keys, value types, and bad addresses, CIDRs, durations or listener scopes — and exits 1 on
an error, so it gates CI. It reads the app's own declarations from the Go module the file
sits in: `config.Section[T]("name")` calls (with `T`'s keys) and
`RegisterExtensionDecoder("name", …)` calls. `${VAR}` placeholders need not be set.
`nexus lint` reports the same problems as errors.

### Editor completion

nexus publishes a JSON schema for `nexus.toml`. Put this on the file's first line, and
Taplo-based editors (VS Code's Even Better TOML, Zed, Helix) complete and check keys:

```toml
#:schema https://paulmanoni.github.io/nexus/nexus.toml.schema.json
```

The published schema covers the tables nexus declares. `nexus config schema -o
nexus.schema.json` writes one that also includes your app's sections; point `#:schema` at
that file instead.

### From v1

v1 ignored keys nothing read. `nexus migrate v2` moves the ones it recognises as real
settings in the wrong table — a top-level `environment`, an `addr` under `[runtime]` —
into the table they belong to. Run `nexus config check` afterwards for the rest: typos,
and app sections to declare with `config.Section`.
