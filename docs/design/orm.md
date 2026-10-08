# nexus ORM — design

Status: proposal. Builds on the Django-style prototype (`Model.Objects().Filter(...)`,
`SelectRelated`, `PrefetchRelated`, embedded `Base` flattened into columns) and asks
what changes when it lives inside nexus instead of beside it.

## Goals

1. **Django's query vocabulary, Go's types.** `Users.Filter(orm.Q{"age__gte": 18}).OrderBy("-age").All(ctx)`
   reads like Django; the result is `[]User`, errors are `nexus.Error`s, and with
   codegen the lookups are checked at compile time.
2. **Works on the tables apps already have.** Existing GORM models (`gorm:"column:x"`,
   embedded `Base`, `TableName()`) are ORM models without retagging. Legacy schemas
   are never migrated unless a model opts in.
3. **Uses what nexus already owns.** Connections, reconnects, pools and drivers
   stay in `db.Manager`; tracing goes to the dashboard; not-found and conflicts go
   through the one error table; GraphQL relations batch through `dataloader`.
4. **Pay for what you use.** A separate module, like `ginrouter`: an app that never
   imports `orm` links none of it.

Non-goals for the first phases: an admin UI, schema migrations for unmanaged
tables, a query language beyond Django's lookups.

## Where it lives

`github.com/paulmanoni/nexus/orm` — its own module, `go 1.27`.

- Go 1.27 brings **generic methods** (`func (q QuerySet[T]) Values[R any](...)`),
  which remove the prototype's biggest limitation: type-changing chains. Checked
  on go1.27.1 here.
- The root module stays on 1.26 until it has a reason to move; apps opt in by
  importing `orm`.

## Model declaration

A model is a plain struct. Metadata comes from tags, read in this order per field:
`orm:"…"` → `db:"…"` → `gorm:"column:…"` → snake_case of the field name.
Embedded structs are flattened with `reflect.VisibleFields`, so a shared `Base`
contributes its columns to every table, and Go 1.27's promoted-field literals make
`User{ID: 1, Name: "Ali"}` work.

```go
type Base struct {
    ID        int64     `orm:"pk"`
    CreatedAt time.Time `orm:"auto_now_add"`
    UpdatedAt time.Time `orm:"auto_now"`
}

type User struct {
    Base
    Name  string
    Email string `orm:"unique"`
    Posts []Post `orm:"rel:author_id"`           // reverse FK
}

type Post struct {
    Base
    Title    string
    AuthorID int64 `db:"author_id"`
    Author   *User `orm:"fk:author_id"`           // FK
    Tags     []Tag `orm:"m2m:post_tags"`          // many-to-many through a table
}

func (Post) TableName() string { return "posts" } // optional, GORM-compatible
```

The manager is a package-level value; the model type is the only argument:

```go
var Users = orm.For[User]()                    // default database
var Archive = orm.For[Post](orm.On("archive")) // a named db.Bind database
var Interviews = orm.For[Interview](orm.Unmanaged()) // never migrated (legacy tables)
```

`orm.For` is also a `nexus.Option` (pass it to `Boot` or a module), which is how it
becomes visible to the framework: boot validates the metadata (unknown tags,
missing FK columns, conflicting promoted fields) and fails with file:line, and the
dashboard lists the model under its database resource.

## Queries

```go
users, err := Users.All(ctx)
adults, err := Users.Filter(orm.Q{"age__gte": 18}).Exclude(orm.Q{"name__icontains": "j"}).OrderBy("-age").All(ctx)
u, err := Users.Get(ctx, orm.Q{"email": "juma@mail.com"})   // nexus.NotFound / nexus.Conflict
n, err := Users.Filter(orm.Q{"age__lt": 18}).Delete(ctx)
n, err := Users.Filter(orm.Q{"active": false}).Update(ctx, orm.Set{"active": true})
for u, err := range Users.OrderBy("id").Iter(ctx) { … }      // streams, range-over-func
names, err := Users.Values[string]("name").All(ctx)          // generic method (1.27)
stats, err := Users.Aggregate(ctx, orm.Count("id"), orm.Avg("age"))
```

- **QuerySet[T] is immutable and lazy.** Each call returns a new value; nothing runs
  until a terminal (`All`, `First`, `Get`, `Count`, `Exists`, `Iter`, `Delete`,
  `Update`, `Aggregate`). Safe to keep in a package var and share across requests.
- **Every terminal takes `ctx` first.** The context carries the request's
  database, transaction, trace span and deadline. There is no hidden global
  connection, so a query inside `orm.Atomic` joins its transaction with no extra
  argument.
- **Lookups:** `exact iexact contains icontains startswith endswith in gt gte lt lte
  range isnull`, then `date year month` per dialect. Paths cross relations:
  `author__name__icontains`, joined automatically.
- **`orm.Q` / `orm.Or` / `orm.Not`** compose conditions. Keys are checked against
  the model's metadata when the QuerySet is built, so a typo fails on the first call,
  not deep inside SQL.

### Typed lookups (codegen, optional)

Strings are Django-faithful but unchecked by the compiler. nexus already generates
code into the build overlay (handlers, views) without committing files; the same
step can emit per-model field sets:

```go
Users.Filter(UserFields.Age.Gte(18), UserFields.Name.IContains("a")).OrderBy(UserFields.Age.Desc())
```

Both forms build the same internal condition tree; apps can mix them. `nexus lsp`
already serves overlay code to the editor, so completion works with nothing
committed.

**Done.** ormgen writes `<Model>FieldSet` and the `<Model>Fields` variable beside
each scanner:

- **Fields:** one per column, a `Field[V]` (a `TextField[V]` for strings).
  `V` is the column's Go type, with pointers removed.
- **Relations:** a method per relation, returning the related model's set
  re-rooted at the relation's path (`Under("author__")`), across packages too.
- **Names you own win:** a package that declares the name itself keeps it, and
  that set isn't generated.
- **Type errors:** they don't stop generation, because code naming a set doesn't
  type-check until the set exists.

## Expressions and custom functions

An `orm.Expr` is SQL the query computes: `orm.F("age")` (a field or an
annotation), a function call, or a template. Expressions go wherever values go
and nest.

```go
// Defined once, package-level; a Template per database where they differ.
var Unaccent = orm.Function("unaccent")                       // unaccent(…)
var Prefix = orm.Function("prefix", orm.Template("", "substr({0}, 1, {1})"))
var Initial = orm.Function("initial", orm.Template("", "substr({0}, 1, 1)")).Transform()

Users.Filter(orm.Where(Prefix.Of(orm.F("name"), 2), "exact", "Am"))   // any lookup
Users.Filter(orm.Q{"name__initial": "A"})                             // a Transform is a Q key step
Users.Filter(orm.Q{"name__unaccent__icontains": "jose"})
Users.Filter(orm.Q{"age__gt": orm.F("min_age")})                      // column against column
Users.Update(ctx, orm.Set{"views": orm.SQL("{0} + {1}", orm.F("views"), 1)})
Users.Annotate("joined", orm.Year.Of(orm.F("created_at"))).
	Filter(orm.Q{"joined__gte": 2025}).OrderBy("-joined").Values[int]("joined")
Users.Aggregate(ctx, orm.AggOf("names", GroupConcat.Of(orm.F("name"), ", ")))
```

- **Templates:** `{0}`, `{1}`, … are arguments and `{*}` is all of them. Each
  argument is an `Expr` or a value sent as a bound argument; `{{`/`}}` are literal
  braces. `orm.SQL` is the escape hatch; its template must never come from user
  input.
- **Built-in transforms:** `lower`, `upper`, `length`, `trim`, `abs`, `year`,
  `month`, `day`, `date`, each spelled per dialect. `orm.Coalesce` is also built in.
- **Annotations:** `Annotate(name, expr)` adds a named computed column that
  `Filter`, `OrderBy` and `Values` use by name. A model field tagged
  `orm:"computed"` (matched by Go name or snake_case) receives it when the query
  annotates it, and is never a column.

## Writes, transactions, hooks

```go
err := Users.Create(ctx, &u)            // INSERT … RETURNING id (Postgres/SQLite), LastInsertId (MySQL)
err := Users.Save(ctx, &u)              // UPDATE by pk, auto_now fields set
err := Users.BulkCreate(ctx, rows, orm.Batch(500))
u, created, err := Users.GetOrCreate(ctx, orm.Q{"email": e}, orm.Defaults{"name": n})

err := orm.Atomic(ctx, func(ctx context.Context) error {
    // every query here uses the transaction; nested Atomic = savepoint
})
```

Hooks are interfaces on the model, promoted from embedded structs like any method:
`BeforeCreate(ctx) error`, `AfterCreate`, `BeforeSave`, `AfterSave`, `BeforeDelete`.

**Change signals** run after the transaction commits, never inside it:
`Users.OnChange(func(ctx, orm.Change[User]))`. This is the natural place to call
`view.Broadcast` so live pages refresh when data changes.

## Relations

- **`SelectRelated("Author", "Author__Profile")`** loads FK paths with one LEFT JOIN
  each, nested paths included; a missing row leaves a nil pointer.
- **`PrefetchRelated("Posts", "Tags")`** makes one `IN (…)` query per relation, for
  FK, reverse FK and many-to-many.
- **`orm.Prefetch("Posts", Posts.Filter(orm.Q{"published": true}).OrderBy("-created_at"))`**
  prefetches with a custom QuerySet, as Django's `Prefetch` does.
- **No lazy loading.** Go can't intercept field access, so an unloaded relation stays
  nil or empty. The dev-time N+1 detector below covers the mistake this invites.

**How lookups cross relations.** The builder walks a key one step at a time:

- **A foreign key** joins the related table as a LEFT JOIN aliased by its path
  (`"author__profile"`), added once per path however often it's named.
- **A relation holding many rows** (reverse FK, many-to-many) becomes `EXISTS
  (SELECT 1 FROM … WHERE link AND rest)`, so the outer rows never repeat. A
  many-to-many's through table is joined inside the subquery.
- **Under `Not`/`Exclude`,** a comparison on a column that can be NULL (a pointer
  field, or any column behind a LEFT JOIN) is written `(cond AND col IS NOT NULL)`.
  That keeps NULL rows in the result, as Django's `exclude` does.
- **Bulk writes:** an UPDATE or DELETE whose conditions join selects its keys
  through a derived table (`pk IN (SELECT pk FROM (…) AS t)`), which MySQL
  requires.

## Subqueries

A subquery is a query placed inside another: as a condition, as a value, or as a
computed column.

```go
// IN: a QuerySet stands for its primary keys; a Values for its one column.
Authors.Filter(orm.Q{"id__in": Books.Filter(orm.Q{"published": true}).Values[int64]("author_id")})

// EXISTS, correlated to the outer row by OuterRef.
Authors.Filter(orm.Exists(Books.Filter(orm.Q{"author_id": orm.OuterRef("id")})))

// A scalar, as an annotation.
latest := Books.Filter(orm.Q{"author_id": orm.OuterRef("id")}).OrderBy("-id").Limit(1).Values[string]("title")
Authors.Annotate("latest", orm.Subquery(latest)).OrderBy("latest")
```

**The pieces.**

- **`subquerier`:** an unexported interface `QuerySet[T]` and `Values[R]` implement,
  `subquerySQL(b *builder) (string, error)`. A QuerySet selects its key; a Values
  selects its single column (two columns is an error, raised when the SQL is built).
- **Uses:** `Q{"x__in": sub}` accepts one in `compare`. `orm.Subquery(sub)` is an
  `Expr` (annotations, `Where`, function arguments). `orm.Exists(sub)` is both a
  `Cond` and an `Expr`, and `orm.Not(orm.Exists(…))` negates it.
- **`orm.OuterRef(name)`** is an `Expr` that renders through the builder's `outer`
  link. It resolves `name`, relation paths included, against the enclosing query's
  model and alias. Used where there is no enclosing query, it fails instead of
  silently comparing a column to itself.

**Rendering.**

- **Shared statement:** the inner query is built by `query.sub(b)`, a child builder
  sharing the outer statement's argument list (`stmt`). Placeholders therefore
  number on in one sequence (`$1…$n` on Postgres) however deep the nesting.
- **Aliases:** each subquery takes the next `nexus_N`, so a table may query itself
  (`Authors.Filter(orm.Exists(Authors.Filter(orm.Q{"id__lt": orm.OuterRef("id")})))`).
  Its own FK joins hang off that alias.
- **Correlation crosses joins:** `outer` is carried through `follow` and `exists`,
  so an `OuterRef` inside a relation path or an EXISTS within the subquery still
  reaches the outermost query's row.
- **Clauses:** the inner query keeps its `ORDER BY`, `LIMIT` and `OFFSET` (needed
  for "latest" scalars). Prefetches and select-related plans are dropped: a
  subquery returns values, not rows.

**Not done:** `OuterRef` to a query two levels out (`OuterRef(OuterRef("id"))`), and
subqueries in `Update`'s SET (`Set{"x": orm.Subquery(…)}`). Both fit the same
design: an `outer` chain, and SET values that render expressions.

## nexus integration

**Errors.** `Get` with no row returns `nexus.NotFound` and with several returns
`nexus.Conflict`. Unique and foreign-key violations become `nexus.Conflict` /
`nexus.InvalidInput` with the column as the field name, so REST answers 409 or 422,
GraphQL gets `extensions.code`, and Inertia flashes the field error. Handlers return
the error as is.

**GraphQL relations.** A model registered with `orm.For` exposes its relations to the
schema. Each FK or reverse FK becomes a batched field through `dataloader`, which is
`LoadField` with the fetch written for you, so nested queries are N+1-free by default.

**Lists for REST and views.** `orm.Paginate(ctx, qs, orm.PageFrom(query),
orm.Sortable("name", "age"), orm.Searchable("name", "email"))` turns query parameters
into `ORDER BY` / `LIMIT` / search, with an allowlist of sortable columns. It replaces
the hand-rolled paging in every list page.

**Tracing and the dashboard.**
- **Spans:** each query is a span on the request's trace in `/__nexus` (SQL, args
  count, rows, duration).
- **N+1 detector:** under `nexus dev`, when one request runs the same query shape
  more than N times, the trace flags it and suggests the
  `SelectRelated`/`PrefetchRelated` call that fixes it.
- **Models panel:** the Resources panel lists each database's models.

**Multiple databases.**
- `orm.On("name")` binds a model to a `db.Bind` database.
- `orm.Using(ctx, "replica")` reroutes one call.

The connection, pool and reconnects are still `db.Manager`'s. The ORM takes the
Manager's `*sql.DB` and `Driver()` and never opens connections itself.

**Testing.** `ormtest.Open(t, models...)` opens in-memory SQLite and creates the
tables. `ORMTEST_DRIVER`/`ORMTEST_DSN` run the same tests on Postgres (a schema per
test) or MySQL (a database per test). `Seed`, `Exec` and `CountQueries` help.

**Several apps in one process.** A manager may be passed to two apps (tests, a
gateway). Each app puts its binding on its request contexts
(`App.SetRequestValue`), so a query made in a request uses that app's databases.
Code outside a request uses the app the manager was last bound to.

**Dev state.** Nothing to preserve: the data lives in the database.

## Engine

- **SQL:** its own small builder over `database/sql`, not over GORM. Dialects
  (Postgres `$n` + `RETURNING` + `ILIKE`, MySQL `?` + backticks, SQLite) are an
  interface of a dozen methods.
- **GORM stays where it is.** `db.Manager` keeps owning connections, and
  `orm.FromGorm(db)` lets one query use the GORM handle an app already has.
  Migrating a project is model by model, with no flag day.
- **Scanning:** per-type metadata is computed once into a `sync.Map`: column list,
  field index paths, pointer-embed allocation and the FK/relation graph. Rows scan
  through precomputed `FieldByIndex` paths, with no per-row tag parsing.
- **Generated scanners (done):** `orm/ormgen` writes one row scanner per model
  that some `orm.For[T]()` names, as `orm_scanners_gen.go` in the model's
  package.
  - **Delivery:** `nexus dev`, `build`, `test` and `vet` overlay them whenever
    the project's go.mod requires the ORM, with nothing written to the tree.
    Builds without the nexus CLI use `go get -tool …/orm/cmd/ormgen` and
    `go tool ormgen`.
  - **Speed:** reading 1,000 rows costs the same as hand-written
    `database/sql`: 1.69 ms against 1.71 ms, down from 1.83 ms by reflection.
  - **Scope:** only packages whose source mentions `orm.For[` are type-checked,
    so a dev-loop save stays cheap.
  - **Safety:** a scanner whose columns no longer match its model is ignored at
    runtime and rows are read by reflection, so stale generated code can't
    misread data. `orm.Generated[T]()` reports which path a model uses.
- **Statements:** prepared statements are cached per SQL shape per connection.

## Migrations

- **Managed models only.** Models marked `orm.Unmanaged()` (every legacy table) are
  never touched.
- **One schema description.** `orm/internal/schema` describes tables, columns
  (named constraints, foreign keys), unique and check constraints, and declared
  indexes once for every dialect (what differs, a generated expression or an index,
  is SQL per dialect), and renders each change per dialect: `ALTER` where the
  database can, a table rebuild on SQLite where it can't. `CreateTables` and the
  migrations both use it.
- **Migrations are Go** (`orm/migration`, Django's): each file of the app's
  migrations package registers a `Migration` of operations from its `init`. An
  operation mutates the replayed state and writes its SQL forwards and back from the
  states around it, so constraint names are a function of history (given when a
  column comes to have one) and never appear in the files.
- **Generating.** `nexus makemigrations` asks the app itself:
  - **The tool:** it overlays `0_nexus_orm_tool.go` into the main package, an `init`
    calling `orm.ServeCLI()` that imports the migrations packages, and builds the
    app with the usual overlay (the model registrations among it).
  - **The run:** with `NEXUS_ORM` set, the binary replays the registered migrations,
    diffs the state with the models it declares (the autodetector, asking about
    renames on the CLI's terminal), writes the next file's source and exits before
    `main`. Every package-level `orm.For` and `orm.Register` has run by then, so the
    schema is the one the app links, read through the same reflection the runtime
    uses.
- **Applying.** `orm.Migrate()` is a `nexus.Setup` step; `nexus migrate` runs the
  same code through the tool, connecting with nexus.toml's blocks:
  - **Order:** every registered migration after its dependencies, across databases.
  - **Locking:** on one connection per database, under a Postgres advisory lock or
    MySQL `GET_LOCK`.
  - **Transactions:** each migration runs in one where the database supports DDL
    in transactions, `RunGo`'s ORM queries included.
  - **Recording:** each is recorded in its database's `nexus_migrations`;
    unapplying removes the record.

## Phases

All done:

1. **Core:** metadata, dialects, QuerySet, CRUD, `Atomic`, hooks, errors, spans.
2. **Relations:** FK, reverse FK and many-to-many lookups; nested `SelectRelated`;
   `Prefetch` with custom QuerySets; GraphQL relation fields; the N+1 warning.
3. **Surfaces:** `Paginate`, `OnChange`, the dashboard's models, `ormtest`,
   subqueries.
4. **Codegen:** generated scanners and insert writers, typed field sets, and
   `nexus lsp` serving them.
5. **Migrations:** Go migration files, `nexus makemigrations`, `orm.Migrate()`, `nexus
   migrate`/`showmigrations`/`sqlmigrate`.

## Open questions

- **GORM, long term:** should `db.Manager` drop GORM once the ORM covers what apps use
  it for, or keep it for apps that want GORM?

## Decisions

- **Lookup style:** `orm.Q{"age__gte": 18}` strings are the main form; generated
  typed fields come later as an optional extra.
- **Module:** `github.com/paulmanoni/nexus/orm`, on Go 1.27.
- **GORM stays:** `db.Manager` keeps GORM. The ORM uses its connection pool,
  and GORM models keep working beside ORM models.
- **Model registration:** `orm.For[T]()` as a `nexus.Option` is the only way to
  register a model; there is no `//nexus:model` decorator.
