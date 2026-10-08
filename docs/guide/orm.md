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

## orm.Model

A model that embeds `orm.Model[T]` (of its own type) is a Django model: its rows save,
refresh and delete themselves, and it has its manager with no package-level variable.

```go
type User struct {
    orm.Model[User]
    ID    int64  `gorm:"column:id;primaryKey"`
    Email string `gorm:"column:email"`
    Roles []Role `gorm:"many2many:user_roles;joinForeignKey:user_id;joinReferences:role_id"`
    Posts []Post
}

func (User) Meta() orm.Meta {
    return orm.Meta{Table: "accounts", Ordering: []string{"email"}}
}

u := User{Email: "ali@example.com"}
err := u.Save(ctx)                        // INSERT: u.ID is set, u is loaded
u.Email = "ali@example.org"
err = u.Save(ctx)                         // UPDATE of every field
err = u.SaveFields(ctx, "email")          // UPDATE of email (and auto_now fields)
err = u.Add(ctx, "roles", admin, editor)  // user_roles rows
err = u.Load(ctx, "roles", "posts")       // prefetched onto u

admins, err := User{}.Objects().Filter(orm.Q{"roles__name": "admin"}).All(ctx)
drafts, err := orm.Related[Post](&u, "posts").Filter(orm.Q{"draft": true}).All(ctx)
```

| Django | nexus |
| --- | --- |
| `class User(models.Model)` | `type User struct { orm.Model[User]; … }` |
| `User.objects` | `User{}.Objects()` or `orm.Objects[User]()` |
| `User.objects.using("legacy")` | `User{}.Objects(Legacy)`, `orm.Of[User](Legacy)` |
| `u.save()` | `u.Save(ctx)`: INSERT when new, UPDATE when loaded |
| `u.save(update_fields=["email"])` | `u.SaveFields(ctx, "email")` |
| `u.save(using="legacy")` | `u.Using(Legacy).Save(ctx)` |
| `u.refresh_from_db()` | `u.Refresh(ctx)` |
| `u.delete()` | `u.Delete(ctx)` |
| `u._state.adding` | unexported: rows read or written are loaded |
| `prefetch_related_objects([u], "roles")` | `u.Load(ctx, "roles")` |
| `u.roles.add(r)` / `remove` / `set` | `u.Add(ctx, "roles", r)` / `Remove` / `Set` |
| `u.posts.all()` | `orm.Related[Post](&u, "posts")` |
| `class Meta: db_table, ordering, indexes, managed` | `func (User) Meta() orm.Meta` |

- **Loaded rows:** a row read through a QuerySet, `Get`, `First`, `Raw` or
  `GetOrCreate`, written by `Create` or `Save`, or loaded as a related row
  (`SelectRelated`, `PrefetchRelated`, `Load`) is loaded, and remembers the schema it
  came from: `Save` updates it there. A new row is inserted, on the model's own schema
  unless bound with `Using`. A row bound to a schema other than its own is new there,
  so `Save` copies it. After `Delete` a row is new again.
- **The manager:** `Objects()` is the model's own manager, the same one its rows write
  through: its package-level `orm.For[T]()` (without `orm.Names`) when it has one, so
  `OnChange`, `Mirror` and nexus.toml's routes apply, else one made from its `Meta`.
  `Objects(schema)` is `orm.Of[T](schema)`. Queries stay on the manager.
- **Many-to-many links:** `Add`, `Remove` and `Set` take rows of the related model,
  pointers to them, or their keys. `Add` skips links that exist; `Set` removes the
  others and adds the missing ones in one transaction. The row must be loaded.
- **`orm.Related[R](row, relation)`** is a QuerySet of R: a foreign key's row, the rows
  holding its key, or those linked many-to-many, inverses included, on the row's schema.
- **Embedding:** embed `orm.Model[T]` once, by value, in `T`, directly or through an
  embedded struct of your own (`type Base[T any] struct { orm.Model[T]; ID int64 }`).
  Twice, through a pointer, or `orm.Model[X]` inside a type other than X fails boot with
  the reason. A struct embedding a model to add fields (`type UserRow struct { User;
  Posts int }`) is a `Values` target, not a model. GORM and JSON ignore the embedded
  state: it is unexported.
- **Registration:** `nexus dev`, `build`, `test`, `vet`, `lsp` and `makemigrations`
  generate an `orm.Register[T]()` for every type embedding `orm.Model[T]` in the
  packages the app links. A registered model is bound to the app's database and checked
  at boot without being passed to `nexus.Boot`, and `makemigrations` plans it with no
  `orm.For`. `nexus generate models` writes the code to disk for a plain `go build`
  (`--check` in CI); without it, `Objects` registers a model on first use.

### Meta

`func (T) Meta() orm.Meta` declares what Django's `class Meta` does:

| Field | |
| --- | --- |
| `Table` | the table |
| `DB` | the database `db.Bind` registered (as `orm.On`) |
| `Unmanaged` | never created or migrated (as `orm.Unmanaged()`) |
| `Indexes` | the indexes (as an `Indexes()` method) |
| `Ordering` | the order of a query that sets none, as `OrderBy` takes it |

**Precedence:** `orm.For`'s options (`orm.Table`, `orm.On`, `orm.Unmanaged`) win over
`Meta`, which wins over the model's `TableName()` and `Indexes()`. Under a names set,
`<Names>TableName()` (`LegacyTableName`) still names the table. `Ordering` orders rows
read for their own sake (`All`, `Iter`, `First`, `Values`, related rows); counts,
aggregates, grouped `Values` and subqueries are never ordered by it, and `OrderBy()`
with no field drops it.

## Models

- **Columns:** each field's column comes from its `orm:"column:…"` tag, else `db:"…"`,
  else GORM's `column:`, else the field name in snake_case. GORM's `primaryKey`, `-`,
  `embedded` and `autoCreateTime` are honoured too, so models written for GORM need no
  new tags. A field with an `orm:` tag of its own (a column, a path read through a
  relation) is the ORM's even when `gorm:"-"` hides it from GORM, so one field can serve
  GORM and every schema: `gorm:"-" orm:"profile__first_name" legacy:"name"`.
- **Embedded structs** are flattened into the table, by value or as a pointer (`*Base`
  is allocated while scanning). An outer field shadows an embedded one of the same name.
- **Table:** `orm.For[T](orm.Table("…"))`, else `Meta().Table`, else the model's
  `TableName()`, else the type's name, plural and in snake_case.
- **JSON columns:** `orm.JSON[T]` holds any T as JSON, Django's `JSONField` — `jsonb`
  on Postgres, `JSON` on MySQL, `TEXT` on SQLite. Read and write `.V`
  (`orm.JSONOf(v)` builds one); the field marshals as the value alone, so responses
  carry it unwrapped. A nil slice, map or pointer is NULL (the column is nullable,
  like `[]byte`), and `__isnull` finds it.

  ```go
  type Letter struct {
      ID       int64
      Placings orm.JSON[[]int64]
      Settings orm.JSON[map[string]string]
  }
  letter.Placings.V = append(letter.Placings.V, 7)
  ```
- **CSV columns:** `orm.CSV[int64]` (any int or string element) holds a list as
  comma-separated text (`"7,9,11"`), for legacy tables that keep lists in a string:
  read and write `.V` (`orm.CSVOf(7, 9)`), marshalled as the list, NULL when nil, `""`
  when empty; reading trims spaces around the commas.
- **Not columns:** relation fields like `Posts []Post`, unexported fields, and fields
  tagged `orm:"-"`.
- **Legacy tables:** `orm.For[T](orm.Unmanaged())` (or `Meta().Unmanaged`) marks a table
  the ORM must never create or migrate. NULLs read as zero values.

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
- **Conditions** combine with `orm.Or`, `orm.Not` and `orm.And`. An empty one
  (`orm.Q{}`, `orm.And()`) is no condition, as Django's `Q()`: it drops out of the
  combination.
- **A misspelt field** is an error on the first call, never SQL that silently
  matches nothing.

### Input from requests

Values are always sent as bound arguments: a filter value, a search term, a list
for `__in`, a vector, a `Case` result. `contains` and the other `LIKE` lookups match
`%`, `_` and backslashes literally on every database. Names are another matter:

- **Allowlist names a request picks.** A key, `OrderBy` field, `Values` name or
  relation path built from input is checked against the model, so it can't inject
  SQL, but it can name any field, through any relation: `?filter=author__password__startswith`
  probes a hash one character at a time, and `Values` of a path through rows held by
  many multiplies the rows. Map the request's names onto the few you allow, as
  `Paginate`'s `Sortable` and `Searchable` do.
- **Limits:** a name has at most 32 parts (fields, relations, transforms and the
  lookup), and a statement at most 65,535 arguments (32,766 on SQLite): a longer
  `__in` list fails before it is sent. Cap what a request may send below that.
- **Text is SQL:** `orm.SQL` templates, `orm.Function` names and `Template`s, `Raw`
  and `Exec` SQL and migrations' `RunSQL` are written into the statement as they are.
  Never build them from input; pass input as their arguments.
- **Errors:** the ORM's own and the violations it maps (`Conflict`, `InvalidInput`)
  name models and fields, never values. Any other database error, which may quote a
  value, is `nexus.Internal`, its message shown only under `nexus dev`; traces and the
  N+1 warning carry the SQL and the number of arguments, never their values.
- **Search text** is bound too, but its cost grows with its length: cap a `__search`
  or `Match` term from a request (`Paginate` reads 200 bytes).

### Values

`Values[R](names…)` reads some fields instead of whole rows, as Django's `values()` and
`values_list()` do. A name is a field, an annotation, or a path through relations,
joined in the same query:

```go
emails, _ := Users.OrderBy("id").Values[string]("email").All(ctx)          // a scalar
teams, _ := Users.Values[string]("team__name").All(ctx)                    // through a foreign key
rows, _ := Users.Values[map[string]any]("email", "roles__name").All(ctx)   // values(): keyed as written
rows, _ := Users.Values[[]any]("email", "team__name").All(ctx)            // values_list(): in order

type UserRow struct {
    Email string
    Team  string `orm:"team__name"` // the path this field takes
}
rows, _ := Users.Values[UserRow]().All(ctx) // no names: the struct's fields say what is read

// An aggregate among the names groups by the others.
perTeam, _ := Users.Annotate("n", orm.Count("id")).Values[[]any]("team__name", "n").All(ctx)
```

- **Targets:** a scalar for one name, a struct (fields matched by `orm:"path"` tag,
  name or column), `map[string]any` keyed by the names as written, or `[]any` in
  their order. A NULL is `nil` in a map or a list.
- **Through rows held by many** (`roles__name`), there is a row per related row, and
  one with `nil` when there is none, as in Django.
- **Grouping:** listing an annotation that is an aggregate (`orm.Count`, `orm.Sum`, …,
  `orm.AggOf`) groups the rows by the other names. Order by a grouped name: Postgres
  and MySQL refuse to order by one that isn't.
- **HAVING:** a `Filter` naming an aggregate annotation filters the groups, Django's
  way: `Annotate("n", orm.Count("books__id")).Filter(orm.Q{"n__gt": 0})`. Only a
  query reading rows takes it; `Count`, `Exists` and writes refuse it.
- **Distinct:** `orm.Count("books__id").Distinct()` is `COUNT(DISTINCT …)`, for a
  row reached more than once through joins.

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

**Write what the request may change.** `Save` and `Create` write every field of the
row, `Update` every field of its `Set`, and `GetOrCreate` every field of its
`Defaults`: a row decoded straight from a request body lets the client set any of them
(`IsAdmin`, `OwnerID`, the key). Decode into a type of the fields you allow, copy them
onto the row, and use `SaveFields` or a `Set` you build. Hooks run for `Create`,
`BulkCreate`, `Save` and `Remove` only: a QuerySet's `Update` and `Delete` and raw SQL
skip them, so a check that must hold for every write belongs in the database (a
constraint) or in each path. A `contains` of an empty string matches every row: guard
an `Update` or `Delete` filtered by input against empty input as `ErrUnfiltered` guards
against no filter.

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

## Conditional expressions

`orm.Case` is SQL's `CASE`, Django's `Case`/`When`: the result of the first branch whose
condition holds. `orm.Switch` compares one expression with values. Both are expressions,
so they go wherever one does.

```go
tier := orm.Case(
    orm.When(orm.Q{"orders__total__gte": 1000}, "gold"),               // through a relation
    orm.When(orm.And(orm.Q{"active": true}, orm.Q{"age__gte": 30}), "silver"),
    orm.When(orm.Q{"active": true}, "bronze"),
).Else("none")

Users.Annotate("tier", tier).Filter(orm.Q{"tier__in": []string{"gold", "silver"}}).OrderBy("-tier")
Users.Annotate("tier", tier).Values[map[string]any]("email", "tier")

points := orm.Switch(orm.F("status")).Case("paid", orm.F("amount")).Case("refunded", 0).Else(-1)
Orders.Annotate("points", points).Aggregate(ctx, orm.Sum("points"))

// One statement: SET age = CASE WHEN active THEN age + 1 ELSE age END
Users.Filter(orm.Q{"age__gt": 0}).Update(ctx, orm.Set{
    "age": orm.Case(orm.When(orm.Q{"active": true}, orm.SQL("{0} + {1}", orm.F("age"), 1))).Else(orm.F("age")),
})
```

- **Conditions:** any condition a `Filter` takes: `Q`, `Or`, `And`, `Not`, `Where`,
  `Exists`, paths through foreign keys (joined) and through rows held by many
  (`EXISTS`), under any schema's names.
- **Results:** a value (sent as a bound argument) or an expression: a field, a
  function, `orm.SQL` arithmetic, another `Case` or `Switch`. With no `Else`, and for a
  `nil` result, it is `NULL`. On Postgres a value is cast to its Go type's SQL type
  (`CAST($1 AS BIGINT)`), since Postgres reads an untyped argument in a `CASE` as text.
- **Where they go:** `Annotate` (a field tagged `orm:"computed"` receives it), every
  `Values` target, `Filter`/`Exclude`/`OrderBy` by the annotation's name, and
  `Update`'s `Set`. A condition in an `Update`'s `Case` may cross rows held by many,
  not a foreign key: `UPDATE` can't join.

**Conditional aggregates.** `Filter` on `Count`, `Sum`, `Avg`, `Min` or `Max`
aggregates only the rows where its condition holds, Django's `filter=`; `As` names it.

```go
r, _ := Users.Aggregate(ctx,
    orm.Count("id").Filter(orm.Q{"active": true}).As("active"),
    orm.Sum("age").Filter(orm.Q{"age__gte": 18}))       // r.Int("active"), r.Int("age__sum")

perTeam, _ := Users.Annotate("active", orm.Count("id").Filter(orm.Q{"active": true})).
    Values[[]any]("team__name", "active")                // grouped by team
```

It is `COUNT(id) FILTER (WHERE …)` on Postgres and SQLite, and `COUNT(CASE WHEN … THEN
id END)` on MySQL, which has no `FILTER`. `AggOf` takes no `Filter`: put the condition
in its expression with a `Case`.

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

Tags name what the convention can't, GORM's or the ORM's own:

- **GORM's relation tags**, all of them, by Go field names as GORM reads them:
  `foreignKey` and `references` on a belongs-to (`foreignKey:WriterID;references:Code`:
  this row's `WriterID` holds the author's `Code`), a has-one and a has-many (the related
  rows' field holding the key, and this model's field it refers to); `many2many:book_tags`
  with `joinForeignKey` and `joinReferences` for the columns of the table between.
- **The ORM's:** `orm:"fk:writer_id"`, `orm:"rel:writer_id"` for the reverse side (the
  related rows' column), and `orm:"m2m:book_tags"` for a **many-to-many** through a table
  (its columns default to `<model>_id`; `m2m:book_tags,book_id,tag_id` spells them out).

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

### Elements relations (keys inside a column)

A legacy table that keeps related ids *inside* a column — a JSON array
(`[7, 9]`) or comma-separated text (`"7,9"`) — declares the relation with
`elements:`, and it behaves as any to-many:

```go
type Letter struct {
    ID          int64
    PlacementID orm.JSON[[]int64]        // or orm.CSV[int64], or a plain string
    Placements  []Placement `orm:"elements:placement_id"`
}

Letters.Filter(orm.Q{"placements__status": "ACTIVE"})          // EXISTS, expanded + keyed
Letters.Annotate("n", orm.Count("placements__id"))             // joins through the elements
letters, _ := Letters.PrefetchRelated("placements").All(ctx)   // slices in the array's order
```

- **The SQL expands each row's array once** (`jsonb_array_elements_text` /
  `JSON_TABLE` / `json_each`; a text column is split on the commas, spaces forgiven)
  and joins the related key by index — the shape the hand-written lateral join takes,
  not a membership test per pair.
- `elements:col,other` joins a related column other than the primary key; a string
  column may hold string keys.
- **No constraint, no inverse, no `Add`/`Remove`** — the column is the truth: write it.
  A NULL or empty column holds no rows. For new tables prefer a real relation (a link
  table): only it gives the database a foreign key and an indexable join both ways.

### Filtered relations

`orm.FilteredRelation` is a relation limited to the related rows where a condition
holds, Django's `FilteredRelation`: annotate it under a name, then read and filter
through that name. The condition names the related model's own fields.

```go
published := Authors.Annotate("published", orm.FilteredRelation("books", orm.Q{"published": true}))

// LEFT JOIN books AS published ON … AND published.published = ?: authors with none keep a row.
rows, _ := published.Annotate("n", orm.Count("published__id")).
    OrderBy("name").Values[[]any]("name", "n").All(ctx)

published.Filter(orm.Q{"published__title__icontains": "go"}) // EXISTS a published book
published.Filter(orm.Q{"published__isnull": true})           // authors with none

Books.Annotate("poet", orm.FilteredRelation("author__profile", orm.Q{"bio__icontains": "poet"})).
    Values[[]any]("title", "poet__bio")
```

- **Reading** (`Values`, aggregates) joins with the condition in the `ON`, so rows
  without a match stay, with NULLs.
- **Filtering** through a relation holding many rows asks `EXISTS` one matching, as
  plain paths do: rows never repeat.
- **The path** may cross relations (`author__profile`); the condition applies to the
  last. It can't follow a foreign key of the related model (that would be a join inside
  the join); a relation holding many rows is fine, as `EXISTS`.

### Inverses

Every foreign key and many-to-many declared on a model implies its inverse on the
related model, as Django's reverse relations do, with nothing declared there:

```go
type Post struct {
    ID       int64
    AuthorID int64
    Author   *User `gorm:"foreignKey:AuthorID"`                 // User gets "posts"
    Tags     []Tag `gorm:"many2many:post_tags"`                 // Tag gets "posts"
}
type Profile struct {
    ID     int64
    UserID int64 `gorm:"uniqueIndex"`
    User   *User `gorm:"foreignKey:UserID" orm:"related:profile"` // one-to-one: User gets "profile"
}

Users.Filter(orm.Q{"posts__title__icontains": "go"})
Tags.Filter(orm.Q{"posts__author__email": e})
Users.SelectRelated("profile").OrderBy("profile__city")
```

- **Name:** `orm:"related:<name>"` on the forward relation, else the declaring model's
  name in snake_case, plural (`posts`), or singular for a foreign key unique by itself
  (one-to-one).
- **Where it works:** `Filter`, `Exclude`, `OrderBy` and `Values` paths, and
  `PrefetchRelated`; `SelectRelated` for a one-to-one.
- **Loading:** prefetched rows land in a field of the inverse's name when the model
  declares one (`Posts []Post`, `Profile *Profile`), with no tag. Without such a field
  the inverse is queryable but not loadable, and prefetching it is an error.
- **Declared both ways:** a has-many the related model declares itself over the same
  columns (`Posts []Post gorm:"foreignKey:AuthorID"`) is the inverse, not a second
  relation.
- **Clashes:** two relations to one model with the same inverse name (`From` and `To`
  both `*Team`) need `related:` names apart; `Schema.Check` fails boot naming them.

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

## Schemas

One set of models, relations declared on them, can be queried on several database
schemas that name things apart: a legacy database and its replacement, say. A schema
picks a **names set**, and each field's tag of that key overrides its default naming
there:

```go
var Legacy = orm.Schema{DB: "legacy", Names: "legacy", Unmanaged: true}

type User struct {
    ID        int64
    Email     string   `legacy:"email_address"`        // the column there
    Bio       string   `legacy:"-"`                    // no such column there
    Phone     string   `gorm:"-" legacy:"tel_no"`      // a column there only
    FirstName string   `legacy:"profile__first_name"`  // read through a relation there
    Profile   *Profile                                 // the inverse of Profile.User
    Groups    []Group  `gorm:"many2many:user_groups" legacy:"many2many:auth_user_groups;joinReferences:group_id"`
}

func (User) LegacyTableName() string { return "auth_user" }

users, err := orm.Of[User](Legacy).Filter(orm.Q{"groups__name": "staff"}).OrderBy("first_name").All(ctx)
```

- **Of:** `orm.Of[T](schema)` is the model's manager on that schema, made once per model
  and schema. It finds its database as `For`'s managers do (`Schema.DB` names the
  `db.Bind`). `orm.For[T](orm.Names("legacy"))` declares one package-level.
- **Names:** under a set, a field's tag of that key is its column, `-` for no such
  field, relation keys (`many2many`, `joinForeignKey`, `joinReferences`, `foreignKey`,
  `references`, `related`) merged over its default tags, or a path through a relation.
  The table is `<Names>TableName()` (`LegacyTableName`), else `TableName()`.
- **Queries don't change:** they name Go fields, default names and relation paths; the
  ORM writes them in the schema's names.
- **Read through a relation** (`legacy:"profile__first_name"`): the query joins it (one
  join, shared with `SelectRelated` on the same path), and `Filter`, `OrderBy` and
  `Values` use the joined column. It is read-only there: `Create` and `Save` skip it, and
  `Update(orm.Set{"first_name": …})` fails, naming the model to write.
- **Writes and change signals** use the schema's columns: `Columns`, `ColumnValues` and
  `PKColumn` of an `Of` manager are the schema's, so a feed of a legacy table's changes
  keeps its columns.
- **Check:** `nexus.Boot(…, Legacy.Check(User{}, Profile{}, Group{}))` fails boot when a
  model doesn't map onto the schema: a tag naming no field, a relation to a model by no
  field of it, a column read through no relation, two relations claiming one inverse
  name; and, once the schema's database connects (boot waits for it), a column, index
  or extension it can't make.
- **Verify:** `schema.Verify(ctx, app, models…)` is the check as a function, for an app
  that picks its schema at run time and mustn't wait at boot for a database it doesn't
  use:

  ```go
  nexus.Setup(func(ctx context.Context, app *nexus.App, cfg *Config) error {
      return schemaFor(cfg).Verify(ctx, app, User{}, Group{})
  })
  ```

A model with no tag of a set reads the same under it as under the default names.

## Raw SQL

When a query is beyond the QuerySet, write the SQL, as Django's `Manager.raw()` and
`cursor.execute()` allow:

```go
// Rows of a model: each column fills the field of that column under the manager's names set.
users, err := orm.Of[User](Legacy).Raw("SELECT * FROM auth_user WHERE tel_no LIKE ?", "255%").
    PrefetchRelated("groups").All(ctx)
u, err := Users.Raw("SELECT * FROM users WHERE email = ?", e).First(ctx) // nexus.NotFound for none

// Any shape, read as Values reads it: a scalar, a struct, map[string]any or []any.
// Raw, RawIter and Exec run on the default database; RawOn, RawIterOn and ExecOn on a schema's.
ids, err := orm.Raw[int64](ctx, "SELECT id FROM users WHERE age > ?", 18)
rows, err := orm.RawOn[map[string]any](ctx, Legacy, "SELECT tel_no, email_address FROM auth_user")
for row, err := range orm.RawIterOn[[]any](ctx, Legacy, "SELECT …") { … }

// Writes and DDL.
changed, err := orm.ExecOn(ctx, Legacy, "UPDATE auth_user SET is_active = ? WHERE last_login < ?", false, cutoff)
```

- **Placeholders:** `?` marks each argument on every database (written `$1, $2…` for
  Postgres); `??` is a literal `?`. Marks inside quotes and comments are left alone,
  read as the database reads them: MySQL's backslash escapes and `#` comments (its
  `--` only before a space), Postgres's `E'…'` strings, nested `/* */` comments and
  `$tag$` quotes.
- **Never put input in the SQL text,** only in the arguments: the text is sent as it
  is.
- **Like the ORM's own queries,** raw SQL runs inside the context's transaction, is
  traced, and counts toward the N+1 warning.
- **The SQL is the schema's:** written for the tables and columns of the schema you
  pass, not translated.
- **Raw writes are the database's alone:** `OnChange` hears nothing of them and no
  mirror repeats them.
- **Columns of a model** with no field are dropped.

## Search

Full-text, trigram and vector search, as Django's `contrib.postgres.search` and
pgvector's Django package have them, written for the database the query runs on.
Postgres does all of it (pg_trgm and pgvector for trigrams and vectors), MySQL searches
text with `MATCH … AGAINST` and has no vectors, and SQLite matches text with `LIKE` and
computes distances and similarity in Go functions `nexus/db/sqlite` registers, with no
index: enough for development and tests.

```go
type Post struct {
    ID        int64
    Title     string
    Body      string
    Search    orm.TSVector `orm:"generated"`   // computed by the database, never written
    Embedding orm.Vector   `orm:"vector:1536"` // pgvector's vector(1536)
}

// Generated columns, Django's GeneratedField: the expression of each orm:"generated" field.
func (Post) Generated() map[string]orm.Expr {
    return map[string]orm.Expr{
        "Search": orm.SearchVector("title").Weight("A").Add(orm.SearchVector("body").Weight("B")).Config("english"),
    }
}

// Indexes, Django's Meta.indexes: made and dropped by migrations.
func (Post) Indexes() []orm.Index {
    return []orm.Index{
        orm.GinIndex("search"),
        orm.GinIndex("title").Trigram(),
        orm.HnswIndex("embedding").Ops(orm.Cosine).M(16).EfConstruction(64),
    }
}

var Vectors = orm.CreateExtension("vector") // and pg_trgm for Trigram
```

Text:

```go
Posts.Filter(orm.Q{"title__search": `go -java "web server"`})   // web search syntax
Posts.Filter(orm.Q{"title__trigram_similar": "postgress"})        // typo-tolerant

doc := orm.SearchVector("title").Weight("A").Add(orm.SearchVector("body"))
q := orm.SearchQuery("web server")
Posts.Filter(orm.Match(doc, q)).
    Annotate("rank", orm.SearchRank(doc, q)).
    Annotate("excerpt", orm.Headline("body", q)).
    OrderBy("-rank")
Posts.Annotate("sim", orm.Similarity("title", "postgress")).OrderBy("-sim")
```

- **`__search`** matches in web search syntax (every word, `"a phrase"`, `or`,
  `-word`): on Postgres `to_tsvector(field) @@ websearch_to_tsquery(…)`, or the
  generated `TSVector` column holding a `SearchVector` of the field when the model has
  one (so `title__search` above searches the indexed `search` column); MySQL's
  `MATCH … AGAINST` in boolean mode (it needs a `FullTextIndex`; a word InnoDB doesn't
  index, shorter than 3 letters or a stopword, isn't required); elsewhere a `LIKE` per
  word.
- **`__trigram_similar`** is pg_trgm's `%`, the same similarity (0.3) on SQLite, and a
  `LIKE` on MySQL.
- **Expressions:** `SearchVector(fields…)` with `Weight`, `Config` and `Add`;
  `SearchQuery(text)`; `Match(doc, q)` (a condition); `SearchRank(doc, q)` (ts_rank, MySQL's
  relevance, else the weights of the fields each word is found in); `Headline(field, q)`
  (ts_headline, else the text around the first word); `Similarity(field, term)` (not on
  MySQL). Fields may be relation paths, and are the schema's names under a names set.

Vectors:

```go
near, err := Posts.Nearest("embedding", v, orm.Cosine).Limit(10).All(ctx) // annotated "distance"
Posts.Annotate("d", orm.L2Distance("embedding", v)).Filter(orm.Q{"d__lt": 0.5})

// Hybrid: reciprocal-rank fusion of the text rank and the distance.
Posts.Filter(orm.Match(doc, q)).
    Annotate("rank", orm.SearchRank(doc, q)).
    Annotate("distance", orm.CosineDistance("embedding", v)).
    Annotate("score", orm.Fuse("-rank", "distance")).OrderBy("-score").Limit(20)
```

- **`orm.Vector`** is `[]float32`, written as pgvector's text; `orm:"vector:N"` sizes the
  column. `L2Distance`, `CosineDistance` and `InnerProduct` (negated, as pgvector's `<#>`,
  so smaller is nearer) are expressions; an index serves the metric its `Ops` names.
- **`Fuse(orders…)`** scores each row by the sum of `1/(60 + rank)` over the orderings
  (a leading `-` ranks descending).

The schema side:

- **Indexes:** `GinIndex(fields or expressions…)` and `GistIndex` (with `.Trigram()` for
  pg_trgm's operator classes), `FullTextIndex(fields…)` (MySQL's FULLTEXT; on Postgres a
  GIN index of their tsvector by `.Config`, which `__search` of a field indexed alone
  uses, or of a `TSVector` field itself), `HnswIndex(field)` and `IvfflatIndex(field)` with `.Ops(orm.Cosine|orm.L2|orm.IP)`,
  `.M`, `.EfConstruction`, `.Lists`; `.Name` names any of them. A `SearchVector` in an
  index or a generated column needs a `Config`.
- **Generated columns** are stored: a `tsvector` on Postgres, the fields' text elsewhere
  (MySQL can index it with a `FullTextIndex`). Create and Save skip them; Update refuses
  them. On SQLite, a migration adding one rebuilds the table.
- **Where an index can't be made:** MySQL makes `FullTextIndex` only, and a model with
  another kind, or a `Vector`, doesn't map onto it; SQLite makes none, its searches
  unindexed.
- **Extensions are explicit:** `orm.CreateExtension("vector")` (or `"pg_trgm"`),
  package-level, has `nexus makemigrations` write `m.CreateExtension` into the next
  migration; passed to `nexus.Boot` it also fails boot while the database lacks it.
  `CreateTables` makes the ones declared for its models' database (`orm.On`). Nothing
  installs one otherwise.
- **`Schema.Check`** (after `orm.Migrate()`) fails boot when its database can't hold a
  model's columns or indexes, or lacks an extension they need, with the hint to add it.

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
  string can't order by arbitrary columns; each field sorts once however often it is
  named.
- **Size:** at most `MaxSize` (100), at least 1; a page past the end is empty, its rows
  never read. Search reads the first 200 bytes of `q`.
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

Migrations are Django's: Go files in the app's `migrations` package, each registering
the operations that take the schema one step on, written by `nexus makemigrations` and
yours to edit. The schema a database has is the replay of its migrations: no snapshot
is kept beside them.

```bash
nexus makemigrations              # writes migrations/0001_initial.go
nexus makemigrations add_age      # the next one, named
nexus makemigrations --check      # CI: fail when the models changed without a migration
nexus makemigrations --empty seed # a migration with an empty RunGo, for data
nexus migrate                     # apply every database's pending migrations
nexus migrate 0002                # take the default database to 0002 (later ones undone)
nexus migrate zero --db legacy    # unapply all of database legacy's
nexus showmigrations              # [X] applied, [ ] not, per database
nexus sqlmigrate 0002 --backwards # the SQL a migration runs
```

```go
// migrations/0002_add_field_users_age.go
package migrations

import m "github.com/paulmanoni/nexus/orm/migration"

func init() {
    m.Register(m.Migration{
        Name:         "0002_add_field_users_age",
        Dependencies: []string{"0001_initial"},
        Operations: []m.Operation{
            // NOTE: adds the NOT NULL column users.age: give the rows that exist a value with .Default(v)
            m.AddField{Table: "users", Name: "age", Field: m.Int().Default(0)},
        },
    })
}
```

- **How it plans:** `makemigrations` builds the app with a tool in its main package and
  runs it (main never does). It replays the migrations the app's migrations packages
  register, compares the result with the app's models (the types embedding
  `orm.Model`; managed ones, of the database), and writes the operations between.
  `--dry-run` prints the file.
- **Only models are planned:** an `orm.For` or `orm.Of` of a plain struct is a query
  over a table someone else owns, never a table to make. A model's database and
  `Unmanaged` come from its `Meta()`, or from its own `orm.For[T](…)` (no `Names`).
- **Renames:** a column (or table) gone where one alike appeared may have been renamed.
  On a terminal it asks, `Did you rename users.mail to users.email? [y/N]`; yes writes
  `m.RenameField`, keeping the data. `--noinput`, or no terminal, writes a removal and
  an addition with a `// NOTE:` saying so.
- **Operations:** `CreateModel` (fields, `Constraints`, `Indexes`), `DeleteModel`,
  `RenameModel` (`AlterModelTable`), `AddField`, `RemoveField`, `AlterField`,
  `RenameField`, `AddIndex`/`RemoveIndex`, `AddConstraint`/`RemoveConstraint`
  (`m.Unique(name, cols…)`, `m.Check(name, expr)`), `CreateExtension`, `RunSQL{SQL,
  ReverseSQL, Dialect}` and `RunGo{Forward, Backward}`. Each writes its SQL for the
  database's dialect, forwards and back.
- **Fields:** `m.BigAuto()`, `m.Auto()`, `m.BigInt()`, `m.Int()`, `m.Float()`,
  `m.Bool()`, `m.Text()`, `m.Varchar(n)`, `m.Time()`, `m.Bytes()`, `m.Custom()`,
  `m.Vector(n)`, `m.TSVector()`, `m.JSON()`, with `.Null()`, `.PK()`, `.Unique()`, `.Index()`,
  `.Type(sql)`, `.Default(v)`, `.Generated(m.Dialects{…})`, `.FK(table, column)` and
  `.OnDelete(m.Cascade)`. Declared indexes and generated expressions are SQL per
  dialect (`m.Dialects{Postgres: …, MySQL: …, SQLite: …}`): a dialect with none for an
  index makes none (SQLite's searches are unindexed).
- **Defaults and checks are the migrations':** models don't declare them, so
  `makemigrations` keeps a `.Default` or `m.Check` you wrote.
- **Data migrations:** `m.RunGo` runs in the migration's transaction. `tx` runs SQL in
  it, and the ORM's queries on its `ctx` do too. It sees the schema the operations
  before it leave, so prefer SQL, or models that match that schema. A `RunGo` without
  `Backward`, or a `RunSQL` without `ReverseSQL` (`m.Noop` for nothing to undo), can't
  be unapplied.
- **SQLite** can't alter a column or a constraint: the migration rebuilds the table
  (made again, rows copied, the old one dropped), foreign keys off meanwhile.
- **Other databases:** `--db legacy` writes `migrations/legacy` (`DB: "legacy"`).
  A dependency on another database's migration is `"legacy:0001_initial"`.
- **Unmanaged tables:** `orm.Unmanaged()` or `Meta().Unmanaged` keeps a model out.

Apply them at boot: import the migrations package, and pass `orm.Migrate()`.

```go
import _ "example.com/shop/migrations"

nexus.Boot(db.BindFromConfig[DB]("main"), orm.Migrate())
```

- **When:** after the databases connect and before the app serves, each migration after
  its dependencies, across databases. `orm.MigrateOn("legacy")` applies one database's.
- **Recording:** each applied migration is recorded in its database's
  `nexus_migrations`; `nexus migrate <target>` unapplies by the operations' reverses and
  removes the record.
- **Transactions:** each migration runs in one on Postgres and SQLite. MySQL commits
  DDL as it goes, so a migration that fails halfway there needs fixing by hand.
- **Replicas** booting together take turns under a lock (a Postgres advisory lock,
  MySQL `GET_LOCK`).
- **The commands** run the app the same way: `nexus migrate`, `showmigrations` and
  `sqlmigrate` connect with nexus.toml's `[databases]` blocks, and the app must link
  its driver.

For tests and tools, `orm.CreateTables(ctx, Users, Posts)` creates the tables
directly, and `orm.ApplyMigrations(ctx, db, target, migrations…)` applies a list.

## Moving a table to another database

`orm.Mirror` keeps a table current on two databases while it moves, say from a
legacy MySQL to Postgres: reads and the first write go to the model's database, and
every write is repeated on the mirror.

```go
var Categories = orm.For[Category](orm.On("legacy"), orm.Mirror("main"))
```

nexus.toml routes a table without a code change, over the options:

```toml
[orm.models.categories]
db = "legacy"    # read, and written first: the source of truth
mirror = "main"  # every write repeated here
```

- **What's mirrored:** `Create`, `BulkCreate`, `Save`, `Remove`, and a QuerySet's
  `Update` and `Delete`.
  - **New rows:** the mirror gets them with the keys the primary gave.
  - **Updates:** an `Update` runs on the mirror as written, with `auto_now` times
    stamped once so both sides hold the same value.
- **When:** after the write's transaction commits; a rolled-back write never
  reaches the mirror.
- **When the mirror fails:** the write still succeeds, because the primary is the
  truth. The failure is logged and passed to `orm.OnMirrorError`, the place to queue
  the table for a re-sync.
- **Cutting over:** swap `db` and `mirror`. The old database then follows as the
  mirror (your way back), and you drop `mirror` when you're done.
  - **Keys:** the first insert that takes a key from Postgres moves the key sequence
    past the rows the mirror (or a bulk copy) wrote with their own keys.
- **Times** go to every database as UTC, so a row reads back the same instant from
  MySQL and Postgres.
- **Tests:** `ormtest.Mirror(t, ctx, models...)` mirrors a test's writes to a second
  database, MySQL to Postgres with `ORMTEST_MIRROR_DRIVER` / `ORMTEST_MIRROR_DSN`.

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
`ORMTEST_DSN` to run the same tests on a real server: each test gets a schema (in the
DSN's Postgres database) or a MySQL database of its own, named `orm_test_…`, dropped
when the test ends. The ORM's own suite runs this way too:

```sh
ORMTEST_DRIVER=postgres ORMTEST_DSN='postgres://postgres:secret@localhost:5432/orm_test?sslmode=disable' go test ./...
ORMTEST_DRIVER=mysql ORMTEST_DSN='root:secret@tcp(127.0.0.1:3306)/' go test ./...
```

Its search tests skip on MySQL, and on a Postgres without pgvector or pg_trgm to
install.

## Dashboard

The Resources tab lists each database's models, and whether a generated scanner reads
them. Every query is a span on its request's trace.

One manager passed to two apps in one process queries the database of the app
serving the request.
