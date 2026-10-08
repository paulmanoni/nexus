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
  `orm.AggOf`) groups the rows by the other names.

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
  name.

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
ids, err := orm.Raw[int64](ctx, orm.Schema{}, "SELECT id FROM users WHERE age > ?", 18)
rows, err := orm.Raw[map[string]any](ctx, Legacy, "SELECT tel_no, email_address FROM auth_user")
for row, err := range orm.RawIter[[]any](ctx, Legacy, "SELECT …") { … }

// Writes and DDL.
changed, err := orm.Exec(ctx, Legacy, "UPDATE auth_user SET is_active = ? WHERE last_login < ?", false, cutoff)
```

- **Placeholders:** `?` marks each argument on every database (written `$1, $2…` for
  Postgres); `??` is a literal `?`. Marks inside quotes and comments are left alone.
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
  `MATCH … AGAINST` (it needs a `FullTextIndex`); elsewhere a `LIKE` per word.
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
  uses), `HnswIndex(field)` and `IvfflatIndex(field)` with `.Ops(orm.Cosine|orm.L2|orm.IP)`,
  `.M`, `.EfConstruction`, `.Lists`; `.Name` names any of them. A `SearchVector` in an
  index or a generated column needs a `Config`.
- **Generated columns** are stored: a `tsvector` on Postgres, the fields' text elsewhere
  (MySQL can index it with a `FullTextIndex`). Create and Save skip them; Update refuses
  them. SQLite can't add one to an existing table: rebuild it.
- **Where an index can't be made:** MySQL makes `FullTextIndex` only, and a model with
  another kind, or a `Vector`, doesn't map onto it; SQLite makes none, its searches
  unindexed.
- **Extensions are explicit:** `orm.CreateExtension("vector")` (or `"pg_trgm"`),
  package-level, has `nexus makemigrations` write `CREATE EXTENSION` into the next
  migration; passed to `nexus.Boot` it also fails boot while the database lacks it.
  Nothing installs one otherwise.
- **`Schema.Check`** (after `orm.Migrate`) fails boot when its database can't hold a
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
`ORMTEST_DSN` to run the same tests on a real server: each test gets a schema or
database of its own, dropped when the test ends.

## Dashboard

The Resources tab lists each database's models, and whether a generated scanner reads
them. Every query is a span on its request's trace.

One manager passed to two apps in one process queries the database of the app
serving the request.
