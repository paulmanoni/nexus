# ORM

`github.com/paulmanoni/nexus/orm` is a Django-style ORM. A model is a plain struct,
its manager a package-level value, and its queries lazy, chainable QuerySets read with
Django's lookups. It is a module of its own (Go 1.27), so an app that doesn't import it
links none of it.

```bash
go get github.com/paulmanoni/nexus/orm
```

```go
import "github.com/paulmanoni/nexus/orm"

type Base struct {
    ID        int64 `orm:"pk"`
    CreatedAt time.Time // auto_now_add by its name
    UpdatedAt time.Time // auto_now by its name
}

type User struct {
    Base
    Name  string
    Email string `orm:"column:email"`
    Age   int
    Bio   *string // nullable
}

var Users = orm.For[User]()

nexus.Boot(
    db.Bind[MainDB]("main", …, db.WithDefault()),
    Users, // binds the model to the app's database; a model the ORM can't map fails boot
)
```

The connection stays with [`db.Bind`](/guide/resources): the ORM reads and writes
through the database bound with `db.WithDefault()`, or the one `orm.On("name")`
names.

## Models

- **Columns:** each field's column comes from its `orm:"column:…"` tag, else `db:"…"`,
  else GORM's `column:`, else the field name in snake_case. GORM's `primaryKey`, `-`,
  `embedded` and `autoCreateTime` are honoured too, so models written for GORM need no
  new tags.
- **Embedded structs** are flattened into the table, by value or as a pointer (`*Base`
  is allocated while scanning). An outer field shadows an embedded one of the same name.
- **Table:** `orm.For[T](orm.Table("…"))`, else the model's `TableName()`, else the
  type's name, plural and in snake_case.
- **Not columns:** relation fields like `Posts []Post`, unexported fields, and fields
  tagged `orm:"-"`.
- **Legacy tables:** `orm.For[T](orm.Unmanaged())` marks a table the ORM must never
  create or migrate. NULLs read as zero values.

## Queries

```go
users, err := Users.All(ctx)
adults, err := Users.Filter(orm.Q{"age__gte": 18}).
    Exclude(orm.Q{"name__icontains": "j"}).OrderBy("-age").All(ctx)
u, err := Users.Get(ctx, orm.Q{"email": e})   // nexus.NotFound / nexus.Conflict
n, err := Users.Filter(orm.Q{"active": true}).Count(ctx)
page, err := Users.OrderBy("id").Offset(40).Limit(20).All(ctx)
names, err := Users.OrderBy("age").Values[string]("name").All(ctx)
for u, err := range Users.OrderBy("id").Iter(ctx) { … }   // one row at a time
r, err := Users.Aggregate(ctx, orm.Count("id"), orm.Avg("age"))  // r.Float("age__avg")
```

- **Lazy and immutable:** each method returns a new QuerySet; nothing runs until a
  terminal (`All`, `First`, `Get`, `Count`, `Exists`, `Iter`, `Delete`, `Update`,
  `Aggregate`). Keep one in a package variable and share it.
- **Every terminal takes `ctx`.** It carries the transaction, the trace and the
  deadline.
- **Lookups:** `exact` (the default), `iexact`, `contains`, `icontains`, `startswith`,
  `istartswith`, `endswith`, `iendswith`, `in`, `gt`, `gte`, `lt`, `lte`, `range`,
  `isnull`. A nil value with `exact` means `IS NULL`.
- **Conditions** combine with `orm.Or`, `orm.Not` and `orm.And`.
- **A misspelt field** is an error on the first call, never SQL that silently
  matches nothing.

## Writes

```go
err := Users.Create(ctx, &u)              // sets u.ID
err := Users.BulkCreate(ctx, []*User{…})  // up to 500 rows per statement, ids set
err := Users.Save(ctx, &u)                // by primary key; auto_now set
err := Users.Remove(ctx, &u)
n, err := Users.Filter(orm.Q{"age__lt": 18}).Update(ctx, orm.Set{"active": false})
n, err := Users.Filter(orm.Q{"active": false}).Delete(ctx)
u, created, err := Users.GetOrCreate(ctx, orm.Q{"email": e}, orm.Defaults{"name": n})
```

**No accidental "every row".** An `Update` or `Delete` with no condition fails with
`orm.ErrUnfiltered` and runs nothing. That covers no `Filter` at all, and a filter of
nothing, such as an empty `Q` built from an empty form. Ask for every row explicitly:

```go
n, err := Sessions.Unfiltered().Delete(ctx)
```

A condition that matches nothing (`Q{"id__in": []int64{}}`) is still a condition: it
runs and changes no rows.

**Hooks** are methods on the model, promoted from an embedded `Base` like any method:
`BeforeCreate`, `AfterCreate`, `BeforeSave`, `AfterSave`, `BeforeDelete`,
`AfterDelete`, each `func(ctx context.Context) error`. A Before hook's error stops
the write.

**Errors** go through nexus's [error model](/guide/handlers): a missing row is
`nexus.NotFound`, several rows from `Get` are `nexus.Conflict`, a duplicate key is
`nexus.Conflict` with the column as the field, and a missing foreign-key row is
`nexus.InvalidInput`. Handlers return them as they are.

## Transactions

```go
err := orm.Atomic(ctx, func(ctx context.Context) error {
    if err := Users.Create(ctx, &u); err != nil {
        return err // rolls back
    }
    return orm.Atomic(ctx, func(ctx context.Context) error { … }) // a savepoint
})
```

Queries take part when they use the `ctx` the function is given. A nested `Atomic` is
a savepoint that rolls back alone.

## Expressions and custom functions

An `orm.Expr` is SQL the query computes: a field (`orm.F("age")`), a function call,
or a template. Expressions go wherever values go.

```go
// Defined once, package-level; a Template per database where they differ.
var Unaccent = orm.Function("unaccent").Transform()
var Prefix = orm.Function("prefix", orm.Template("", "substr({0}, 1, {1})"))

Users.Filter(orm.Where(Prefix.Of(orm.F("name"), 2), "exact", "Am"))
Users.Filter(orm.Q{"name__unaccent__icontains": "jose"})  // a Transform is a key step
Users.Filter(orm.Q{"created_at__year": 2026})              // built in
Users.Filter(orm.Q{"age__gt": orm.F("min_age")})           // column against column
Users.Update(ctx, orm.Set{"views": orm.SQL("{0} + {1}", orm.F("views"), 1)})
Users.Annotate("joined", orm.Year.Of(orm.F("created_at"))).
    Filter(orm.Q{"joined__gte": 2025}).OrderBy("-joined")
```

- **Templates:** `{0}`, `{1}`, … are arguments and `{*}` is all of them; values are
  sent as bound arguments, never pasted into the SQL. `orm.SQL` is the escape hatch:
  never build its template from user input.
- **Built-in transforms:** `lower`, `upper`, `length`, `trim`, `abs`, `year`,
  `month`, `day`, `date`, each spelled for every database. `orm.Coalesce` is also
  available.
- **Annotations:** `Annotate(name, expr)` is a named computed column that `Filter`,
  `OrderBy` and `Values` use by name. A model field tagged `orm:"computed"` receives
  it.
- **Cast:** `orm.Cast(v, t)` converts a field, an expression or a value, spelled
  for each database: `orm.AsInt`, `AsFloat`, `AsText`, `AsDate`, `AsDateTime`,
  `AsDecimal(10, 2)`, or `AsType("CHAR(20)")` for a type of your own (a name and
  its size only; MySQL's `CAST` takes few types, no `VARCHAR`).

  ```go
  Users.Annotate("age_text", orm.Cast(orm.F("age"), orm.AsText)).
      Filter(orm.Q{"age_text__startswith": "2"})
  Orders.Filter(orm.Where(orm.Cast(orm.F("created_at"), orm.AsDate), "exact", day))
  ```
- **Custom aggregates:** `orm.AggOf("names", GroupConcat.Of(orm.F("name"), ", "))`.

## Generated scanners

Rows are read by reflection unless a generated scanner exists for the model, and with
one, reading costs what hand-written `database/sql` does. `nexus dev`, `nexus build`,
`nexus test` and `nexus vet` generate scanners for every `orm.For[T]()` and add them
through the build overlay: there is nothing to run and nothing lands in the tree.
The same files hold each model's typed field set and its insert writer, so `Create`
skips reflection too.

Without the nexus CLI, write them to disk:

```bash
go get -tool github.com/paulmanoni/nexus/orm/cmd/ormgen
go tool ormgen          # -check in CI
```

A scanner whose columns no longer match its model is ignored and rows are read by
reflection, so stale generated code can't misread data. `orm.Generated[T]()` says
which a model uses.

## Outside nexus and in tests

```go
ctx := orm.WithDB(context.Background(), orm.Open(sqlDB, "sqlite"))
users, err := Users.All(ctx)
```

`orm.Using(ctx, "replica")` sends one call to another database `db.Bind` registered.
Each query is a span of the request's trace on the [dashboard](/guide/dashboard).

## Relations

A field holding another model is a relation. By convention:

- `Author *User` beside an `AuthorID` column is a **foreign key**.
- `Posts []Post` is the **reverse** of `Post`'s foreign key to this model.

Tags name what the convention can't: `gorm:"foreignKey:WriterID"`, `orm:"fk:writer_id"`,
`orm:"rel:writer_id"` for the reverse side (the related rows' column), and `orm:"m2m:book_tags"` for a
**many-to-many** through a table (its columns default to `<model>_id`;
`m2m:book_tags,book_id,tag_id` spells them out).

```go
type Book struct {
    ID       int64
    Title    string
    AuthorID int64
    Author   *Author
    Tags     []Tag `orm:"m2m:book_tags"`
}
```

Lookups cross relations with `__`:

```go
Books.Filter(orm.Q{"author__name": "Ali"})                // LEFT JOIN authors
Books.Filter(orm.Q{"author__profile__bio__icontains": "poet"})
Authors.Filter(orm.Q{"books__title__icontains": "go"})     // EXISTS over books
Authors.Filter(orm.Q{"books__isnull": true})               // authors with no book
Books.Filter(orm.Q{"tags__name": "golang"})                // through book_tags
Books.OrderBy("author__name").Values[string]("author__name")
```

A foreign-key path is a LEFT JOIN, joined once however often it's named. A relation
holding many rows is an `EXISTS` subquery, so rows never repeat and no `Distinct` is
needed. `Exclude` keeps rows whose compared column is NULL, as Django's does.

Loading related rows is explicit, because Go can't load a relation lazily on field
access:

```go
books, _ := Books.SelectRelated("author__profile").All(ctx)      // one query, joined
authors, _ := Authors.PrefetchRelated("books__tags", "profile").All(ctx)
authors, _ = Authors.PrefetchRelated(
    orm.Prefetch("books", Books.Filter(orm.Q{"published": true}).OrderBy("-title")),
).All(ctx)
```

- **`SelectRelated`** follows foreign keys only. A missing row leaves the pointer nil.
- **`PrefetchRelated`** runs one `IN (…)` query per relation and level, in chunks of
  1,000 keys. A relation with no rows becomes an empty slice, not nil.

### GraphQL

`GraphRelation` adds a batched field to a model's GraphQL type: one query per level of
the request, however many parents.

```go
nexus.Boot(…, Books, Authors,
    Books.GraphRelation[*Author]("author"),
    Authors.GraphRelation[[]Book]("books"),
)
```

### N+1 warnings

Under `nexus dev`, a query shape run 5 times in one request (`orm.RepeatWarn`) logs a
warning naming the `SelectRelated` or `PrefetchRelated` that fixes it. The warning is
also marked on the request's trace.

## Subqueries

```go
published := Books.Filter(orm.Q{"published": true}).Values[int64]("author_id")
Authors.Filter(orm.Q{"id__in": published})                       // IN (SELECT …)

Authors.Filter(orm.Exists(Books.Filter(orm.Q{"author_id": orm.OuterRef("id")})))
Authors.Filter(orm.Not(orm.Exists(…)))

latest := Books.Filter(orm.Q{"author_id": orm.OuterRef("id")}).OrderBy("-id").Limit(1).Values[string]("title")
Authors.Annotate("latest", orm.Subquery(latest))                 // a computed column
```

- **What can be a subquery:** a QuerySet (it selects its primary key), or a `Values`
  of one column.
- **`OuterRef("id")`** reads the row of the query around it, and may follow
  relations.
- **Aliases:** a subquery's tables are aliased apart (`nexus_1`, …), so a table can
  query itself.

## Typed lookups

String lookups are checked when the query runs. The generated field sets check them
when the code compiles. For every model, beside its scanner, the generator writes
`<Model>Fields`:

```go
Users.Filter(UserFields.Age.Gte(18), UserFields.Name.IContains("al")).
    OrderBy(UserFields.CreatedAt.Desc())
Books.Filter(BookFields.Author().Profile().Bio.StartsWith("a poet"))
Authors.Filter(AuthorFields.ID.InQuery(Books.Values[int64](BookFields.AuthorID.Name())))
```

- **Methods on a field:** `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `In`, `Range`,
  `IsNull`, `InQuery`, `Asc` and `Desc`.
- **Text fields** add `Contains`, `IContains`, `StartsWith`, `EndsWith`, their `I`
  forms, and `IExact`.
- **Relations** are methods returning the related model's set.
- **Values** take the field's own Go type, so `UserFields.Age.Gte("18")` doesn't
  compile.
- **Mixing:** both forms build the same conditions and can be combined.
- **Your names win:** a package that declares a name of its own (`UserFields`)
  keeps it, and that set isn't generated.

`nexus lsp` serves the generated files to the editor, so completion and
go-to-definition work with nothing in the tree.

## Pagination

```go
func ListUsers(ctx context.Context, c *httpx.Ctx) (orm.PageResult[User], error) {
    return orm.Paginate(ctx, Users.Filter(orm.Q{"active": true}), orm.PageFrom(c.Request.URL.Query()),
        orm.Sortable("name", "created_at"), orm.Searchable("name", "email"),
        orm.DefaultSort("-created_at"), orm.MaxSize(100))
}
```

- **Query parameters:** `PageFrom` reads `page`, `size`, `sort` (`-name` for
  descending) and `q`.
- **Sorting:** a sort not listed in `Sortable` falls back to the default, so a query
  string can't order by arbitrary columns.
- **Search** is a case-insensitive `contains` across the `Searchable` fields.
- **The result:** `PageResult` carries `Items`, `Total`, `Page`, `Size` and `Pages`.

## Change signals

```go
Users.OnChange(func(ctx context.Context, c orm.Change[User]) {
    view.Broadcast(ctx, "users", c.Kind)
})
```

- **When it runs:** after the write's transaction commits, or straight away outside
  one; never for a rolled-back transaction.
- **What it hears:** `Created`, `Updated` and `Deleted` carry the row, from
  `Create`/`BulkCreate`, `Save` and `Remove`.
- **Bulk writes:** a QuerySet's `Update` and `Delete` report `BulkUpdated` and
  `BulkDeleted` with a count.

## Migrations

```bash
nexus makemigrations            # writes migrations/0001_initial.sql + schema.json
nexus makemigrations add_age    # the next one, named
nexus makemigrations --check    # CI: fail when the models changed without a migration
```

`makemigrations` builds the app with a planner in its main package. It runs that
binary (main never runs), compares the models it declares with the snapshot the last
migration left, and writes the difference as plain SQL.

- **Dialect:** from the database's `[databases.<name>]` block (`--dialect` overrides).
- **Other databases:** `--db` targets one and writes to `migrations/<name>`.
- **Unmanaged tables:** `orm.Unmanaged()` keeps a model's table out.
- **Review before committing.** A rename shows as a drop and an add, and steps that
  need a person are marked `-- NOTE:`.

Apply the migrations at boot:

```go
//go:embed migrations
var migrations embed.FS

nexus.Boot(db.BindFromConfig[DB]("main"), Users, Posts,
    orm.Migrate(migrations, orm.MigrateDir("migrations")))
```

- **When:** after the database connects and before the app serves, in name order.
- **Recording:** each applied migration is recorded in `nexus_migrations`.
- **Transactions:** each migration runs in one on Postgres and SQLite. MySQL commits
  DDL as it goes, so a migration that fails halfway there needs fixing by hand.
- **Replicas** booting together take turns under a lock (a Postgres advisory lock,
  MySQL `GET_LOCK`).
- **Editing an applied migration** logs a warning and is not re-run: write a new
  migration instead.

For tests and tools, `orm.CreateTables(ctx, Users, Posts)` creates the tables
directly.

## Tests

```go
func TestUsers(t *testing.T) {
    ctx := ormtest.Open(t, Users, Posts) // a fresh database with the tables
    ormtest.Seed(t, ctx, Users, &User{Name: "Ali"})
    n := ormtest.CountQueries(ctx)
    …
}
```

`ormtest` runs on in-memory SQLite. Set `ORMTEST_DRIVER=postgres` (or `mysql`) and
`ORMTEST_DSN` to run the same tests on a real server: each test gets a schema or
database of its own, dropped when the test ends.

## Dashboard

The Resources tab lists each database's models, and whether a generated scanner reads
them. Every query is a span on its request's trace.

One manager passed to two apps in one process queries the database of the app
serving the request.
