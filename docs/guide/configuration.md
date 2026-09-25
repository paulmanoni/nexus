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

- the `[runtime]` tables, which become the runtime `nexus.Config`
- every `[extensions.*]` block
- the `[env]` bridge
- the value store behind `nexus.Get`

A missing `nexus.toml` is fine, and defaults apply. A malformed one panics at boot.
Point at a different file with the `NEXUS_CONFIG` environment variable, or call
`nexus.BootFrom(path, opts...)`.

If you prefer to build the config in Go, use `nexus.Run`:

```go
nexus.Run(nexus.Config{
    Server:        nexus.ServerConfig{Addr: ":8080"},
    Dashboard:     nexus.DashboardConfig{Enabled: true, Name: "Shop"},
    Introspection: true,
}, usersModule)
```

`Boot` is shorthand for
`nexus.Run(nexus.MustLoadConfig(), append(nexus.MustLoadExtensions(), opts...)...)`.

::: warning Runtime keys live under `[runtime]`
Every runtime key belongs in `[runtime]` or a `[runtime.<sub>]` table. A runtime key at
the top level is silently ignored. `[databases.*]`, `[extensions.*]` and `[env.*]` are the
top-level exceptions.
:::

## A typical file

```toml
[runtime]
environment   = "development"   # NEXUS_ENVIRONMENT overrides it
introspection = true            # opens /__nexus (off by default)

[runtime.server]
addr           = ":8080"
max_body_bytes = 33554432       # off by default; set it in production

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

## Reading values: `nexus.Get`

Any value in `nexus.toml`, including your own sections, is readable by its dotted path:

```toml
[shop]
currency = "EUR"
page_size = 50
```

```go
currency := nexus.Get[string]("shop.currency")
size     := nexus.Get[int]("shop.page_size", 20)          // second arg is the default
ttl      := nexus.Get[time.Duration]("cache.ttl", 5*time.Minute)
```

Values resolve per key, highest priority first:

1. **An environment variable.** `shop.page_size` is overridden by `SHOP_PAGE_SIZE`.
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
