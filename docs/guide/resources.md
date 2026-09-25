# Databases & caches

Resources are typed wrappers that embed a framework manager. nexus manages their
lifecycle (start on boot, stop on shutdown), provides them to the DI graph, and shows them
on the dashboard, in red when they are down.

## Databases

```go
import (
    "github.com/paulmanoni/nexus/db"
    _ "github.com/paulmanoni/nexus/db/postgres" // the driver(s) you use
)

type DB struct{ *db.Manager }

nexus.Boot(
    db.BindFromConfig[DB]("main"),   // reads [databases.main]
    usersModule,
)
```

```toml
[databases.main]
driver   = "postgres"
host     = "localhost"
port     = "5432"
user     = "postgres"
password = "${DB_PASSWORD}"
name     = "shop"
sslmode  = "disable"
default  = true
# log    = "warn"   # SQL logging: on in dev, silent in prod unless forced
```

Handlers and services take `*DB` as a parameter. `db.GetDB()` returns the `*gorm.DB`.

- **Drivers are opt-in blank imports**, as with `database/sql`: `db/postgres` (pgx),
  `db/mysql` or `db/sqlite` (pure Go). A config naming a driver that isn't linked fails
  at boot and names the import to add.
- **File-backed SQLite** gets a small read pool. Add a `busy_timeout` pragma to the DSN.
- **Inline config** with no TOML: `db.Bind[DB]("main", func() db.Config { ... })`.
- **Options:** `db.WithDefault()`, `db.WithDescription(...)`, `db.WithDetails(...)`.

## Caches

```go
import (
    "github.com/paulmanoni/nexus/extension/cache"
    _ "github.com/paulmanoni/nexus/extension/cache/redis" // optional: Redis
)

type Cache struct{ *cache.Manager }

cache.Bind[Cache]("session",
    func() *cache.Config { return &cache.Config{} },
    cache.WithDefault(),
)
```

The default cache is **in-memory only** and pulls in no heavy dependencies. Blank-import
`extension/cache/redis` to use Redis with transparent in-memory failover. Without that
import, go-redis is never linked. `cache.BindFromConfig[Cache]("session")` reads a
`[cache.session]` table.

## Pay for what you import

The binders live in `db` and `extension/cache`, not the nexus root package. Importing
`nexus` alone does not pull GORM, SQL drivers, Redis or Prometheus into your build.

## Other resources

Register anything else, such as a queue, by hand:

```go
q := resource.NewQueue("jobs", "Background jobs", details, healthy, resource.DependsOn("main"))
app.Register(q)
```

Or implement `NexusResources() []resource.Resource` on a type your constructors return,
and nexus picks the resources up automatically.
