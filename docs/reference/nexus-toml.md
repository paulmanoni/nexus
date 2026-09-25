# nexus.toml

`nexus.Boot` loads `nexus.toml` from the working directory. Set `NEXUS_CONFIG` to use
another path. Every key is optional. See [Configuration](/guide/configuration) for how
values are resolved.

::: tip
Runtime keys live under `[runtime]` or a `[runtime.<sub>]` table. A runtime key at the
top level is ignored.
:::

## `[runtime]`

```toml
[runtime]
environment    = "development"   # development | staging | production
                                 # NEXUS_ENVIRONMENT overrides it
version        = "1.0.0"         # shown on the dashboard
introspection  = true            # open /__nexus (off by default → 404)
introspection_networks = ["10.0.0.0/8"]  # allowed even when introspection is off
trace_capacity = 1000            # request-trace ring buffer (0 = off)
sdk            = true            # generate and serve the typed client SDK
```

## `[runtime.server]`

```toml
[runtime.server]
addr                 = ":8080"
route_prefix         = ""        # prepended to every REST, GraphQL and WS route
strip_trailing_slash = true      # "/users/" routes as "/users" (off by default)
max_body_bytes       = 33554432  # off by default; over the limit → 413
max_header_bytes     = 1048576
idle_timeout         = "120s"    # keep-alive cap; "-1s" uses Go's default
read_timeout         = "0s"      # off by default (would cut large uploads)
write_timeout        = "0s"      # off by default (would cut SSE and downloads)
shutdown_timeout     = "10s"     # graceful drain; 250ms under nexus dev

[runtime.server.listeners.admin] # optional extra listeners
addr  = "127.0.0.1:7000"
scope = "admin"                  # public | internal | admin
```

## `[runtime.websocket]`

```toml
[runtime.websocket]
allowed_origins = ["https://app.example.com", "*.example.com"]  # "*" disables the check
```

## `[runtime.dashboard]` and `[runtime.graphql]`

```toml
[runtime.dashboard]
enabled = true
name    = "My App"

[runtime.graphql]
path               = "/graphql"
disable_playground = false       # true hides the browser IDE
```

## `[runtime.middleware.*]`

```toml
[runtime.middleware.cors]
allow_origins = ["*"]

[runtime.middleware.ratelimit]
rpm   = 600
burst = 50

[runtime.middleware.security]
headers         = true           # the default security headers
frame_options   = "SAMEORIGIN"   # "-" omits X-Frame-Options
referrer_policy = "no-referrer"
csp             = "default-src 'self'"
hsts_max_age    = 31536000
csrf            = true           # double-submit CSRF (off by default)
```

## `[runtime.logging]`

Used by `nexus dev`.

```toml
[runtime.logging]
format   = "pretty"              # pretty | logfmt | pattern | raw
pattern  = "%time  %-5level  %caller  %msg  %fields"
requests = true                  # dev-only per-request console lines
```

## `[databases.<name>]`

A top-level table, wired with `db.BindFromConfig[T]("<name>")`.

```toml
[databases.main]
driver   = "postgres"            # postgres | mysql | sqlite
host     = "localhost"
port     = "5432"
user     = "postgres"
password = "${DB_PASSWORD}"
name     = "myapp"
sslmode  = "disable"
default  = true
log      = "warn"                # silent | error | warn | info; omit for auto

[databases.reporting]            # values from a config server instead
driver     = "postgres"
key_prefix = "db.reporting"      # reads db.reporting.{host,port,username,password,name}
```

## `[cache.*]`, `[storage.*]`, `[mail.*]`

Read by `cache.BindFromConfig`, `storage.BindFromConfig` and `mail.BindFromConfig`. Keys
are the snake_case config field names. See [File storage](/guide/storage) and
[Mail](/guide/mail).

## `[env.*]`

Top-level. Each key becomes a process environment variable, and it can be referenced
from the frontend as `import.meta.env.<dotted.key>`. See
[the `[env]` bridge](/guide/configuration#the-env-bridge).

```toml
[env.client]
id = "myapp-web"                 # os.Getenv("client.id"), import.meta.env.client.id
```

## `[extensions.*]`

Decoded for extensions that are blank-imported:

```toml
[extensions.config]              # _ "github.com/paulmanoni/nexus/extension/config"
endpoint      = "http://localhost:8078"
identity      = "myapp"
profile       = "default"
poll_interval = "30s"
```

## `[decorators.imports]`

Import hints for [`//@` decorators](/guide/decorators). Usually unnecessary.

```toml
[decorators.imports]
inertia = "github.com/paulmanoni/nexus/extension/inertia"
```

## Your own sections

Any other table is readable with `nexus.Get`:

```toml
[shop]
currency = "EUR"
```

```go
nexus.Get[string]("shop.currency")
```
