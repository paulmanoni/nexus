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
err := Users.BulkCreate(ctx, []*User{…})  // 500 rows per statement, ids set
err := Users.Save(ctx, &u)                // by primary key; auto_now set
err := Users.Remove(ctx, &u)
n, err := Users.Filter(orm.Q{"age__lt": 18}).Update(ctx, orm.Set{"active": false})
n, err := Users.Filter(orm.Q{"active": false}).Delete(ctx)
u, created, err := Users.GetOrCreate(ctx, orm.Q{"email": e}, orm.Defaults{"name": n})
```

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
- **Custom aggregates:** `orm.AggOf("names", GroupConcat.Of(orm.F("name"), ", "))`.

## Generated scanners

Rows are read by reflection unless a generated scanner exists for the model, and with
one, reading costs what hand-written `database/sql` does. `nexus dev`, `nexus build`,
`nexus test` and `nexus vet` generate scanners for every `orm.For[T]()` and add them
through the build overlay: there is nothing to run and nothing lands in the tree.

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

## Not yet

Relation lookups (`author__name`), `SelectRelated` / `PrefetchRelated`, typed field
sets and migrations come next: see the
[design](https://github.com/paulmanoni/nexus/blob/main/docs/design/orm.md).
