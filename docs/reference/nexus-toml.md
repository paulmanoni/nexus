# nexus.toml

`nexus.Boot` loads `nexus.toml` from the working directory. Set `NEXUS_CONFIG` to use
another path. Every key is optional. See [Configuration](/guide/configuration) for how
values are resolved.

::: tip The file is strict
Every table is declared by its owner and every key must be one the table has. A runtime
key at the top level, a misspelt key, an `[extensions.x]` block without its extension
imported, or a section nobody declared fails boot with the line and a did-you-mean. Check
a file with `nexus config check`; see [Strictness](/guide/configuration#strictness).

Editors complete keys from the published JSON schema — add this first line:
`#:schema https://paulmanoni.github.io/nexus/nexus.toml.schema.json`
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
dotenv         = [".env"]        # loaded before ${VAR}s expand (the default); "!.env" = required
```

## `[runtime.server]`

```toml
[runtime.server]
addr                 = ":8080"
route_prefix         = ""        # prepended to every REST, GraphQL and WS route
strip_trailing_slash = true      # "/users/" routes as "/users" (off by default)
max_body_bytes       = 33554432  # default 32MB; -1 = no cap; over the limit → 413
max_header_bytes     = 1048576
idle_timeout         = "120s"    # keep-alive cap; "-1s" uses Go's default
read_timeout         = "0s"      # off by default (would cut large uploads)
write_timeout        = "0s"      # off by default (would cut SSE and downloads)
shutdown_timeout     = "10s"     # graceful drain; 250ms under nexus dev
trusted_proxies      = ["10.0.0.0/8"]  # peers whose X-Forwarded-For nexus.ClientIP honours

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
csrf            = true           # force double-submit CSRF on (false: off); unset follows the app
```

## `[runtime.logging]`

`level` sets the app logger's level; the rest is used by `nexus dev`.

```toml
[runtime.logging]
level    = "info"                # debug | info | warn | error (App.Logger's default handler)
format   = "pretty"              # pretty | logfmt | pattern | raw
pattern  = "%time  %-5level  %caller  %msg  %fields"
requests = true                  # dev-only per-request console lines
```

## `[runtime.telemetry]`

Exports traces to an OpenTelemetry collector over OTLP/HTTP. Off unless
`otlp_endpoint` is set.

```toml
[runtime.telemetry]
otlp_endpoint = "http://localhost:4318"   # spans POST to /v1/traces
service_name  = "orders"                  # default: the dashboard name

[runtime.telemetry.otlp_headers]
authorization = "Bearer ${OTLP_TOKEN}"
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
are the snake_case config field names. Each table is declared by its package, so the app
must import `extension/cache`, `extension/storage` or `extension/mail` for the file to
carry it (`[jobs]` likewise needs `extension/jobs`). See [File storage](/guide/storage) and
[Mail](/guide/mail).

A cache can also choose its store with `driver`:

```toml
[cache.session]
driver = "memory"   # memory: never Redis | redis: in every environment | auto (default): Redis in production
```

See [choosing the store per cache](/guide/resources#choosing-the-store-per-cache).

## `[jobs]`

Read by `jobs.Module`. See [Background jobs](/guide/jobs).

```toml
[jobs]
driver = "db"              # memory | db | redis | rabbitmq (default: the bound driver's, else memory)
run    = true              # false: enqueue here, run the jobs elsewhere
shutdown_grace = "10s"     # running jobs get this long on shutdown (0 under nexus dev)
lease  = "30s"             # db/redis: a claim's lease, renewed while the job runs
poll   = "1s"              # db/redis: how often idle workers check for work

[jobs.queues]              # queue → concurrent workers (default: default = 4)
default = 4
low     = 1

[jobs.redis]               # jobsredis.Bind
url    = "redis://:password@redis:6379/2"   # else REDIS_URL
prefix = "{nexus:jobs}:"

[jobs.rabbitmq]            # jobsamqp.Bind
url              = "amqp://user:pass@rabbitmq:5672/"   # else RABBIT_URL
prefix           = "nexus.jobs."
consumer_timeout = "8h"    # keep above your longest jobs.Timeout
delivery_limit   = 20
```

## `[auth]`

Read by `auth.Module` when `Config.Users` is set (the config-driven path). See
[Accounts and sign-in](/guide/auth#accounts-and-sign-in).

```toml
[auth]
default = "signed-in"      # every endpoint needs a sign-in unless Public; "public" opts out
cache   = "5m"             # Users.Load kept per user id; a negative value turns it off
login   = "/login"         # sign-in page for page visits outside areas (unset: 401)
home    = "/"              # landing after sign-in without a next
next_param = "next"        # the query/form field carrying next

[auth.areas.admin]
prefix = "/admin"          # these paths need a sign-in of one of kinds
kinds  = ["staff"]         # empty: any signed-in user
login  = "/admin/login"    # this area's sign-in page
home   = "/admin"          # landing after sign-in here (default: prefix)

[auth.throttle]
account = "5/15m"          # failed sign-ins per account per window; "off" disables
ip      = "50/15m"         # per client IP
lockout = "15m"            # an account's lock at its limit

[auth.endpoints]           # each mounted only when set
login  = "/api/auth/login"
logout = "/api/auth/logout"
me     = "/api/auth/me"

[auth.schemes.web]         # tried apikey → bearer → session, by name within a type
type   = "session"         # cookie → server-side session (extension/session) → user id
cookie = "nexus_session"   # the session cookie (default nexus_session)
ttl    = "336h"            # the session's lifetime (default 14 days)
secure = true              # mark the cookie Secure behind TLS

[auth.schemes.api]
type = "bearer"            # opaque tokens auth.SignIn issues, stored as SHA-256
ttl  = "12h"               # default 12h

[auth.schemes.partners]
type   = "apikey"          # never expire; revoked by signing out with them
header = "X-API-Key"       # default X-API-Key

[auth.passwords]
hashers    = ["bcrypt", "argon2id", "pbkdf2"]   # the first hashes new passwords; all verify
min_length = 8             # auth.SetPassword's shortest password
common     = true          # refuse common passwords
numeric    = true          # refuse all-digit passwords
similar    = true          # refuse passwords like the user's id or login
```

Without `[auth.schemes.*]`, one session scheme named `web` is used.

## `[env.*]`

Top-level. Each key becomes a process environment variable, and it can be referenced
from the frontend as `import.meta.env.<dotted.key>`. See
[the `[env]` bridge](/guide/configuration#the-env-bridge).

```toml
[env.client]
id = "myapp-web"                 # os.Getenv("client.id"), import.meta.env.client.id
```

## `[extensions.*]`

Decoded for extensions that are blank-imported. A block whose extension isn't imported
fails boot:

```toml
[extensions.config]              # _ "github.com/paulmanoni/nexus/v2/extension/config"
endpoint      = "http://localhost:8078"
identity      = "myapp"
profile       = "default"
poll_interval = "30s"
```

## `[decorators.imports]`

Import hints for [`//nexus:` decorators](/guide/decorators). Usually unnecessary.

```toml
[decorators.imports]
inertia = "github.com/paulmanoni/nexus/v2/extension/inertia"
```

## Your own sections

Any other top-level table must be declared by the app with `config.Section`, which also
decodes it into a typed struct and checks its keys:

```toml
[shop]
currency = "EUR"
```

```go
type ShopConfig struct {
    Currency string `toml:"currency"`
}

var Shop = config.Section[ShopConfig]("shop")

Shop.Get().Currency                   // "EUR"
config.Get[string]("shop.currency")   // also works, with SHOP_CURRENCY as an override
```

See [Your own sections](/guide/configuration#your-own-sections-config-section).
