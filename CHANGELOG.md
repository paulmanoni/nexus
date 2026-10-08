# Changelog

All notable changes to nexus are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [2.27.4] - 2026-10-08

### Fixed

- A live page's patches (a link's `?query` change, `PushPatch`) and broadcasts (`Info`) ran
  inside the trace of the socket's upgrade request, so the ORM's N+1 warning added up every
  patch since the page opened and warned of repeats that were really separate visits. Each
  now runs as a trace of its own, like a browser event.

## [2.27.3] - 2026-10-08

### Security

- `Static` on the default stdlib router and on chi listed the contents of any directory
  without an `index.html`, so every folder under a served directory (uploads, media) could
  be browsed. A directory is now served only through its `index.html`, and is a 404
  otherwise — what gin's `Static` already did.

## [2.27.2] - 2026-10-08

### Fixed

- `nexus.DecoratedModules(...)` dropped the view runtime's own routes (`/_view/*.js`)
  and the component kit's assets along with the filtered annotations, so a live page
  in a scoped test boot never connected. Framework packages now register that plumbing
  with `nexus.RegisterBuiltinOptions`, which the filter never touches.
- The decorators guide ended mid-sentence.

## [2.27.1] - 2026-10-08

### Fixed

- Live forms: an event sent while a form's change still waited for typing to pause
  (150ms) reached the server before that change, and the change — carrying the
  browser's fields from before the event's reply — could then undo it (a choice added
  right after ticking an answer disappeared on a slow connection). A live page now
  sends its pending form changes first, so the server sees events in the order the user
  made them; a submit no longer has its own form's change arrive after it.

## [2.27.0] - 2026-10-08

### Added

- **Database session settings.** `db.Config.Session` / `[databases.<name>.session]` set
  server settings on every connection the pool opens — MySQL system variables
  (`foreign_key_checks`, `sql_mode`), Postgres run-time parameters (`search_path`,
  `statement_timeout`), SQLite pragmas — through the connection string, so no pooled
  connection misses them. Values are written plainly and quoted per driver; a key the
  driver would read as its own option fails boot. `db.Config` holds a map now, so it is
  no longer comparable with `==`.

### Fixed

- ORM (`orm/v0.3.1`): `Save` on MySQL reported "not found" when the row it saved was
  unchanged — MySQL counts changed rows, not matched ones. It now checks the row exists.

## [2.26.0] - 2026-10-07

### Added

- `view.CurrentURL(ctx)`: the URL of the page being rendered — the request's
  for a page, the one the browser shows for a live page (kept current as
  `view.Link`/`PushPatch` patch it), the page a shard sits on for its
  re-render. Mount, Update, Render and context processors read it — the path
  breadcrumbs or an active menu entry follow.

## [2.25.0] - 2026-10-07

### Added

- Context processors: `view.ContextProcessor(func(ctx, deps…) (T, error))`
  registers a value every template reads with `view.FromContext[T](ctx)` —
  the layout's user, counts, navigation. Computed from the request (or a live
  page's connection) with DI dependencies, only when read, at most once per
  render; a live page's tracked parts that read it always render again.

- `view.SetCookie(name, value, view.MaxAge(d))` and `view.Reload()` JS
  commands: set a cookie the server reads on the next request (`Path=/`,
  `SameSite=Lax`, `Secure` on https, value percent-encoded; `MaxAge(0)`
  deletes) and load the page again —
  `view.JS(view.SetCookie("lang", "sw", view.MaxAge(365*24*time.Hour)).Reload())`.
- `[runtime.browser] config = ["shop.currency", …]`: the nexus.toml keys a
  page's script may read, with `__nx.config(key)`. `view.Script()` sends their
  values; boot fails on a key under `[databases]`, `[secrets]` or
  `[extensions]`, one named like a password, secret, token, key or credential,
  one that names nothing, or a table. `config.BrowserValues()` returns them.

- `ElementRead.Eq(value)`: a condition comparing a read as text —
  `view.If(view.This().Attr("aria-pressed").Eq("true"))`, a field's
  `Value().Eq("m")`, `Checked().Eq("false")`. An attribute the element lacks
  never equals, not even `""`.

## [2.24.0] - 2026-10-07

### Added

- `view.Element` and `view.El(sel)`: an element of the page, as a command's
  target, a value read as the event is sent (`view.Send(p.Search,
  view.El("#q").Value())`) and a condition. `view.This()` is the Element the
  event is on.
- JS command conditions: `view.If(cond)`, `view.ElseIf(cond)`, `view.Else()`
  chain steps — `view.JS(view.If(view.El("#agree").Checked()).Push(p.Go).Else().Show("#hint"))`.
  A condition is a read of an element (`Checked()`, a non-empty `Value()`, an
  `Attr(name)` present), `Is(css)`, or an Element (its selector matches
  anything); it reads the page when the chain reaches it. `view.JS` refuses an
  `Else` or `ElseIf` without an `If` before it, or after an `Else`.
- viewtest's in-process browser matches `:placeholder-shown`, `:valid`,
  `:invalid` (required, minlength/maxlength, pattern, type=email/number),
  `:focus`, `:focus-within` and `:empty`.

### Changed

- JS commands take their target as a `view.Element` instead of `any`
  (`view.Target`, now deprecated, is an alias of it). A selector written in
  place still compiles; one built from a string at render time is wrapped:
  `view.Show(view.El("#row-" + id))`. Anything else is a compile error rather
  than a panic at render. `view.ThisElement` and `view.ThisValue` are
  deprecated aliases of `view.Element` and `view.ElementRead`.

## [2.23.0] - 2026-10-07

### Added

- JS commands chain: `view.JS(view.Show("#m").FocusFirst("#m").Push(p.Load))`.
  Every command is a method of the one before; a chain is a value — extending
  it never changes it, so shared steps are built once
  (`closing := view.Hide("#m").PopFocus()`) — and `a.Then(b, c)` joins chains.
  `view.JS(a, b)` and spreading a `[]view.JSOp` keep working.
- New commands: `view.Confirm(msg)` (the steps after it run only on yes),
  `view.SetValue(v, sel)` (as if typed: `input`/`change` fire, so a
  `view.Change` hears it), `view.Copy(sel)` / `view.CopyText(text)` (clipboard;
  the clicked element carries `data-copied` for a moment), `view.ScrollTo(sel)`.
- `view.Debounce(d)` / `view.Throttle(d)`: chain steps timing the steps after
  them (as `Confirm` gates them; steps before run at once) —
  `view.JS(view.Debounce(300*time.Millisecond).Push(p.Search, view.This().Value()))`.
  Debounce runs them once the event stops firing for `d`; throttle runs the
  first at once and drops the rest within `d`, keeping a field's last input.
  A debounced chain waiting in a form runs before that form's submit.
- `view.This()`: JavaScript's `this` — the element the event is on. A JS
  command's target (`view.AddClass("on", view.This())`; commands' selector
  parameters are now `view.Target`: a selector string or `view.This()`, so
  `""` is no longer needed — it still works), and event arguments read when the
  event is sent: `view.Send(p.Toggle, id, view.This().Checked())`, `.Value()`,
  `.Attr(name)`, in `view.Send`, `view.SendTo` and `view.Push`. An event's
  number or bool parameter now also takes a string holding one ("42").
- `data-nx-busy`: an element whose event goes to the server carries
  `aria-busy="true"` from the click until that event's reply, ignores clicks
  meanwhile, and keeps the marker through re-renders; a dropped connection
  releases it. Spinners are CSS (`aria-busy` variants), no script.
- `@view.Behaviors()` (also loaded by the kit's `ui.Script()`): style-free
  `data-nx` behaviors — `data-nx-filter` (a box filtering `[data-nx-filter-item]`s,
  with `[data-nx-filter-empty]`), `data-nx-check-all` (+ `data-nx-checks`,
  `[data-nx-checked-count]`; set before the form's `view.Change` reads the
  boxes), `data-nx-valid` (submit enabled while the fields pass the browser's
  checks). Each is applied again after a live re-render.

### Fixed

- A form being submitted kept its `aria-busy` (the double-submit guard) only
  until the next re-render; a re-render meanwhile no longer clears it.

## [2.22.3] - 2026-10-07

### Fixed

- A live patch matched elements by `id` without comparing tags, so an element
  that changed tag but kept its id (a disabled `<span id="print">` that
  became `<a id="print" href>`) stayed the old element with the new one's
  attributes: a link that looked enabled but couldn't be clicked. It is now
  replaced.

## [2.22.2] - 2026-10-07

### Fixed

- A live-validated `view.Form` (`view.LiveValidation` or a change method)
  now publishes its errors to `view.Errors(ctx)` as it checks — a field's
  once it's left, every field's after a submit, as `F(name)` shows them.
  Markup of your own around `view.RenderForm` showed errors only on submit.

## [2.22.1] - 2026-10-07

### Fixed

- A live page's struct that embeds `view.LiveView` itself (a shared list
  block, say) was taken for a single Assign — the LiveView's methods are
  promoted to it — so its forms were never bound (`Load` panicked "not
  bound to a page") and its Assigns weren't tracked. Embedded structs are
  now always walked as part of the page.
- Tag binding (`form:`, `query:`, `header:`, path) skipped embedded
  structs; their fields now bind as the outer struct's, as `encoding/json`
  treats them. A `view.Form` can take its fields from a struct it embeds.

## [2.22.0] - 2026-10-07

### Added

- Named routes. Every REST route takes a name: its handler's
  (`(*Users).Show` → `show`) in its module's namespace (`users:show`), or
  the stacked namespace of the routers that include it
  (`v1:billing:invoices:show`); `nexus.Name("…")` overrides it, and two
  routes named alike explicitly fail the boot. `nexus.URL(ctx, "users:show",
  7)` / `nexus.Reverse` build a route's path in the app serving ctx, with the
  module `Path`, router prefixes and `route_prefix` applied and id
  parameters masked when maskid is on. Parameters fill positionally, by name
  (`nexus.P`), from a struct's `path:` / `query:` fields, and `nexus.Query`
  adds a query. An unknown name or a missing parameter panics under `nexus
  dev` and in tests (with a did-you-mean), and logs and returns `"#"` in
  production.
- Route handles. `nexus.AsRest` returns a `*nexus.Route` — still an Option
  — whose `URL(ctx, params…)`, `Reverse` and `Method()` build the route
  wherever it ended up mounted, so a link is a compile-time reference rather
  than a string. `inertia.Page` and `view.Page` return one too;
  `view.Live[T]` and `ControllerRouter` have `URL` (a controller takes the
  action: `users.URL(ctx, (*UsersController).Show, 7)`), and `Router.URL`
  resolves names relative to the router.
- `nexus.DefaultName` (an extension's default name for the routes it
  registers: `view.Page` names a page after its component, a live page
  after its type, auth's endpoints `login`, `logout`, `me`, …) and
  `nexus.NoName` (plumbing nothing links to). `App.Routes()` lists the named
  routes; the manifest, `GET /__nexus/routes`, the dashboard's endpoint page
  and `nexus routes` (a NAME column and `--name`) show them.
- `orm.Mirror(name)` and nexus.toml's `[orm.models.<table>] db / mirror`: dual
  writes for moving a table between databases. Every write is repeated on the
  mirror after it commits, with the primary's keys; reads stay on the
  primary; a failed mirror write goes to `orm.OnMirrorError` and the log.
  Cutting over is swapping the two. `ormtest.Mirror` for tests.
- The ORM moves a Postgres table's key sequence past the keys written into
  it before taking a key from it, so a table cut over doesn't reuse keys.
- `view.JS`: Phoenix-style JS commands for live pages — `onclick={
  view.JS(view.Show("#menu"), view.AddClass("is-open", "#menu"),
  view.Push(p.Load)) }`. Commands: `Show`, `Hide`, `Toggle` (with
  `view.Display`), `AddClass`, `RemoveClass`, `ToggleClass`, `SetAttr`,
  `RemoveAttr`, `ToggleAttr`, `Focus`, `FocusFirst`, `Push` / `PushTo`; a
  selector matches the page, the element itself (`""`), its nearest ancestor
  (`view.Closest()`) or inside it (`view.Inner()`). What a command changes
  survives the page's re-renders until the server renders that attribute
  differently. viewtest runs them and gains `Expect(…).HasClass` / `NoClass`;
  `Visible` / `Hidden` read `display: none` too.
- More JS commands: `view.Animate(during, from, to)` and `view.Time` make
  `Show`, `Hide` and `Toggle` transitions; `Transition` adds classes for a
  while; `PushFocus` / `PopFocus` keep a focus stack; `Exec` runs the
  commands (`view.Commands`) or live script an attribute holds, so a dialog
  keeps its closing steps once; `Dispatch` fires a `CustomEvent` with
  `view.Detail`.
- `view.Form`: Django-seamless forms for live pages. A form is a struct
  embedding `view.Form` — the whole declaration, as a Django form class:
  `form:`, `validate:`, `label:`, `help:`, `placeholder:`, `span:` and
  `input:` tags, completed by optional methods on it: `<Field>Choices(ctx)`
  (the field renders as a select; the receiver is the current values, so
  choices can depend on other fields), `Validate(ctx)` (Django's clean(),
  gating the submit, which only ever sees valid values), `Init(ctx)`
  (defaults, run once before the page's Mount) and `Save(ctx) error` (takes
  the submit when `ui.Form` is given no method). The page holds it as a
  field or embeds it, reads and writes the fields directly (`p.Edit.Load(v)`
  copies a record in), and `@ui.Form(p.Edit, p.Save)` — or `@ui.Form(p.Edit)`
  for the form's own Save — renders every field and a Save button from the
  struct; children and `ui.Field("name", opts…)` take over layout, `ui.Live`
  or a change method make it check as the user types (`Changed(field)`,
  `Update(func())` for dependent fields). The browser's values merge over
  the loaded ones — an unrendered field keeps its value, an unticked
  checkbox arrives false — errors show once a field is left (all after a
  submit), success resets to the loaded values, `Load`/`Reset` replace even
  typed values, a busy form drops a second submit, and `Dirty()` with
  `view.ConfirmLeave` guards leaving. `view.RenderForm` is the kit-free
  element; `nexus generate views` fails a misspelled `F("name")` with a
  did-you-mean.
- `view.CSRF()` renders the request's CSRF token as a `csrf_token` hidden
  field, for a form posted over plain HTTP without the view runtime;
  `secure.CSRFToken(ctx)` returns it, including the one the first request
  seeds and the one `RotateCSRF` makes.
- `view.Within(container)` scopes a JS command's selector to the element's
  nearest `container` (an accordion section's panel, a tab strip's tabs).
  `FocusFirst` prefers an `[autofocus]` field and skips `data-nx-nofocus`.
- The component kit's dialogs and tabs run as JS commands. A browser-side
  `ui.Dialog` keeps its closing steps in `data-cancel`, which its close
  button, `data-ui-close`, Esc and the backdrop run; it opens on its first
  field (not the close button) and closes back to its opener. A tab's choice
  — and a `Panel` tab's panel — now survives the page's re-renders.
  viewtest runs the kit's `ui.js`.
- `LiveView.PushJS(ops…)` and `PushEvent(name, payload)`: an event, `Info`
  or connected `Mount` runs JS commands, or dispatches a window
  `CustomEvent`, once its reply is applied — close a dialog after a save,
  tell an island. An event that fails sends none of them. viewtest gains
  `Page.Eval`.
- viewgen passes `view.JS`, `view.SendTo`, `view.SubmitTo` and
  `view.ChangeTo` inside `templ.Attributes` as attribute text, as it did
  `view.Send` — templ dropped them.
- ORM has-one relations: a pointer field whose foreign key sits on the other
  table (`Settings *Settings` with `Settings.AccountID`) loads with
  `PrefetchRelated`.
- ORM composite primary keys: a model with several `primaryKey` fields is
  filtered, inserted and deleted through its QuerySet.

### Fixed

- ORM `CreateTables` makes byte-slice columns nullable, and keeps foreign
  keys only between the tables it creates — one to a table that isn't there
  failed the create.
- ORM: times are sent as UTC. Postgres read a `TIMESTAMP` written from a
  machine not on UTC back shifted by its offset.

## [2.21.0] - 2026-10-06

### Added

- ORM relations: foreign keys (`Author *User` + `AuthorID`, or tagged),
  reverse foreign keys and many-to-many (`orm:"m2m:book_tags"`), followed by
  lookups (`author__name`, `books__title__icontains`, `tags__name`), orders and
  `Values`. Foreign keys join, many-row relations are `EXISTS`, so rows never
  repeat. `SelectRelated` loads foreign keys in one query; `PrefetchRelated`
  (and `orm.Prefetch` with a custom QuerySet) loads any relation with one query
  per level.
- ORM subqueries: `Q{"id__in": qs}`, `orm.Exists`, `orm.Subquery` as an
  annotation, and `orm.OuterRef` to the enclosing row.
- Typed lookups, generated beside the row scanners: `UserFields.Age.Gte(18)`,
  `BookFields.Author().Name.IContains("a")`, checked by the compiler. `nexus
  lsp` serves the generated ORM files to the editor.
- `Manager.GraphRelation[C](name)`: a batched GraphQL field for a relation, one
  query per level.
- Migrations: `nexus makemigrations` writes `migrations/NNNN_name.sql` from the
  difference between the app's models and the last snapshot (`--check` for CI,
  `--db`, `--dialect`, `--empty`); `orm.Migrate(fsys)` applies them at boot,
  recorded in `nexus_migrations`, under a lock. `orm.CreateTables` for tests and
  tools. New model tags: `unique`, `index`, `size`, `type`, `ondelete`.
- `orm.Paginate` with `PageFrom`, an allowlist of `Sortable` columns,
  `Searchable` fields and size limits.
- `Manager.OnChange`: hear a model's writes once their transaction commits.
- `orm/ormtest`: `Open` a test database with the models' tables (SQLite, or
  Postgres/MySQL through `ORMTEST_DRIVER`/`ORMTEST_DSN`), `Seed`, `Exec`,
  `CountQueries`; `orm.WithObserver` sees each statement.
- Under `nexus dev`, a query repeated in one request logs an N+1 warning naming
  the `SelectRelated`/`PrefetchRelated` that fixes it.
- The dashboard lists each database's ORM models (`db.Describe` adds details to
  a database's resource).
- Generated insert writers: `Create` and `BulkCreate` read field values without
  reflection.
- `App.SetRequestValue(key, val)`: a value on every request context the app
  serves. The ORM uses it so a manager passed to two apps in one process queries
  the database of the app serving the request.
- `nexus docs orm`; `nexus doctor` checks the ORM (Go 1.27, the generated code,
  models read by reflection) and counts migrations.
- `nexus release --module dir=vX.Y.Z` tags a module versioned on its own (the
  v0 orm beside the v2 root); modules move in waves, each after the modules it
  requires are tagged and pushed.

- `orm.Cast(v, t)`: convert a field, expression or value to `orm.AsInt`,
  `AsFloat`, `AsText`, `AsDate`, `AsDateTime`, `AsDecimal(p, s)` or
  `AsType("…")`, spelled for each database.
- ORM: an `Update` or `Delete` with no condition (no `Filter`, or an empty
  `Q`) fails with `orm.ErrUnfiltered` instead of changing every row;
  `qs.Unfiltered()` asks for every row on purpose.

### Fixed

- Postgres connection strings quote their values: an empty password no longer
  swallows the next keyword (`password= dbname=app` connected to the wrong
  database), and a password can't add keywords. A MySQL database name is
  escaped, so it can't add driver parameters.
- ORM: an `OnChange` signal from a nested `Atomic` that rolled back no longer
  fires when the outer transaction commits.
- ORM: `Paginate` answers a page past the end without querying and caps the
  search length; `BulkCreate` batches stay under the databases' argument
  limits; migrations split Postgres strings ending in a backslash correctly.
- ORM: `Exclude` (and `Not`) keep rows whose compared column is NULL, as
  Django's `exclude` does.
- ormgen finds `orm.For` under any import name of the orm package.

## [2.20.0] - 2026-10-06

### Added

- `github.com/paulmanoni/nexus/orm` (new module, v0.1.0, Go 1.27): a Django-style
  ORM. `var Users = orm.For[User]()` passed to `nexus.Boot` binds a model to the
  database `db.Bind` registered; QuerySets are lazy and immutable
  (`Filter`/`Exclude` with Django lookups, `OrderBy`, `Limit`/`Offset`, `All`,
  `Get`, `Count`, `Iter`, `Values[R]`, `Aggregate`, `Update`, `Delete`), writes
  (`Create`, `BulkCreate`, `Save`, `Remove`, `GetOrCreate`) with hooks and
  auto timestamps, `orm.Atomic` with savepoints, errors as `nexus.Error`s, and
  custom database functions (`orm.Function`, transforms in `Q` keys, `Annotate`,
  `orm.SQL`). Existing GORM models need no new tags. See the ORM guide.
- Generated row scanners for the ORM: `nexus dev`, `build`, `test` and `vet`
  overlay one per `orm.For[T]()` model when the project requires the ORM, so
  reading rows costs what hand-written `database/sql` does; `orm/cmd/ormgen`
  writes them for builds without the CLI.
- `db.Lookup(app, name)`: the Manager `db.Bind` registered under a name (the
  default one for an empty name).
- `App.ExemptCSRF("/api/devices/*")`: a path ending in `/*` exempts every
  path under it, so a token or device API with path parameters can opt out
  of the CSRF check without listing each route.
- GraphQL input objects: `graphql:"name,items=required"` makes a list's
  elements non-null (`[T!]`).

### Fixed

- GraphQL input objects (`RegisterGqlType`, structs in args) honour
  `graphql:"name,required"`: the field is non-null in the schema, as it
  already was for top-level arguments. It was parsed and ignored.

## [2.19.1] - 2026-10-06

### Fixed

- view: **random ids from component libraries no longer change every
  render.** templUI and shadcn-templ name an element rendered without an id
  `id-` + `crypto/rand.Text()`; a live render now renames each after its place
  on the page. Before, every event changed those ids in the browser, a page
  with such a component in its layout never skipped a part (each event logged
  "rendered differently … the full render was sent"), and a page never joined
  its HTTP render, so connecting sent it again. A live page's HTTP render is
  now recorded like the connection's.
- views compiler: **a struct a live page embeds by value is looked into**, as
  the runtime does since 2.19.0. A page embedding a list helper whose fields
  are Assigns (or `view:"-"`) — from its own package or another of the
  module — was warned about as "keeps the live page rendering in full"; now
  only a plain field inside an embedded struct is, at that field.

## [2.19.0] - 2026-10-05

### Added

- view: **a struct a live view embeds is part of it.** Pages that share state
  and events embed one struct: its `view.Assign` fields are tracked, its
  `view.LiveView` is the page's, and its methods are the page's events.
- `[runtime.tailwind] imports`: stylesheets of Go modules, as
  `"<module>/<file>.css [conditions]"`, which `nexus dev`/`nexus build` @import
  into `sources.generated.css` from wherever Go keeps the module — a component
  library imported as a package brings its styles, uncopied.
- httpx: a repeated field (checkboxes of one name) binds to a slice of any
  scalar — `[]int64`, `[]bool`, `[]float64` — not only `[]string`.
- `view.Assets(prefix, handler, gates…)`: gates make an asset tree private.
- The CLI installs as a project tool:
  `go get -tool github.com/paulmanoni/nexus/cmd/nexus/v2@latest`, then
  `go tool nexus …`.

### Changed

- `nexus dev` writes the compiled views (`*_templ.go`, `view_gen.go`,
  `view_imports_gen.go`) beside the `.templ` files, so any editor sees them;
  `--no-view-files` keeps them in memory. `nexus new` gitignores them, and
  `nexus dev` says when a git project doesn't. `--view-files` is deprecated.
- The CLI module is leaner: `nexus dev --tui` runs on a small built-in terminal
  UI instead of bubbletea/lipgloss, and `nexus config check` reads `[cache]`
  from `extension/cache/cacheconfig` without linking the cache. A project
  adding the CLI as a tool gets 9 modules instead of 27.

### Fixed

- A router at `"/"` registering its own path — `view.Live[*T]("/")` — panicked
  at boot ("host/path missing /"); it serves `"/"`.
- view: assets (`view.Assets`) and the runtime's scripts answered 401 to a
  signed-out visitor under auth's deny-by-default, so a sign-in page lost its
  stylesheet; they are public.
- viewgen: a code example in a doc comment (`//	@button.Button(…)`) failed
  the build as a v1 annotation; only `page`, `auth` and `use` are.
- `nexus config check`/`lint` missed a section declared with an inferred type,
  `config.Section("name", T{…})`.

## [2.18.0] - 2026-10-05

### Changed (breaking)

- view: **one kind of live view.** A live view is a struct that embeds
  `view.LiveView` (by value); routed it is a page — `view.Live[*T](path, gates…)`
  or `//nexus:live <path>` on the type — and embedded it is a part of a page —
  `@view.Component[*T](id, props)`, with no registration unless it takes
  dependencies (`view.Live[*T]("")`, or `//nexus:live` with no path).
  - Input is one props struct: `Mount(ctx, deps…, props)`; routed, bound from
    `path:`/`query:` tags and checked by `validate:` (422 on failure); embedded,
    the parent's. `Update(ctx, deps…, props)` runs on a URL patch or new props
    (else Mount again). `Params` is gone.
  - `view.LiveView` gives `Connected`, `Subscribe`, `Track`/`Untrack`,
    `PushPatch`/`PushNavigate`, `PutFlash`/`Flash` and `ID`. The `*view.Socket`
    parameter and `view.PushPatch(ctx, …)` / `view.PushNavigate(ctx, …)` are gone.
  - Embedded views subscribe (their own `Info`) and track presence on their
    own; one the page stops rendering takes them along.
  - `view.LiveComponent` is gone. `.Provide` on `view.Live` is optional: without
    a provider the type starts from its zero value.

## [2.17.0] - 2026-10-05

### Added

- view: **streams** (LiveView's streams). A `view.Stream[T]` field shows a
  list the server doesn't keep: `Configure(id)`, `Insert`, `Prepend`,
  `InsertAt`, `Delete`/`DeleteID`, `Reset`, `Limit`; the template spreads
  `Attrs()` on the list and renders `Items()` — the latest change only — each
  with its `ID`. The browser applies each change once and keeps its rows.
- view: **presence** (Phoenix Presence). `sock.Track(topic, key, meta)` makes
  a live page present while it is open, `sock.Untrack` ends it early,
  `view.Presences(topic)` lists who is there, and subscribed pages get
  `view.PresenceDiff{Joins, Leaves}` through `Info`. With `view.UseRelay` it
  spans replicas: joins and leaves travel, each replica restates its
  presences every 10s, and one quiet for `view.PresenceTTL` (30s) is dropped.
  `view.Presences` builds a topic's list once per change and every page shares
  it.

  Measured (5,000 users posting to a 1,000-message chat every ~5s): a stream
  answers at p99 6 ms on ~25% of a CPU, where a plain list re-rendering its
  rows saturates the machine; 153 KB per page against 942 KB; a broadcast
  reaches 2,000 pages in 26 ms against 1 s. A join reaches a room of 500 in
  66 ms.

## [2.16.0] - 2026-10-05

### Added

- view: **change tracking for live pages** (LiveView's assigns). A page whose
  fields are `view.Assign[T]` (`Get`, `Set`, `Update`) renders only what
  changed: the view compiler opens a spot (`Rec.Guard`) around each loop,
  branch and component call that uses none of the template's own variables,
  and a spot whose `Assign`s (and `view.Errors`) didn't change since the
  browser's tree is not run; the reply carries no change for it. Pages with
  other fields render everything, as before. Under `nexus dev` and in tests
  each skipping render is checked against a full one; a spot that read
  something untracked is logged and the full render sent
  (`NEXUS_VIEW_VERIFY`). The example adoption board uses it.
- view: the compiler warns where a live page that uses `view.Assign` isn't,
  or can't be, tracked — a field that isn't an `Assign` (on the field, and on
  each template read of it), and on a tracked page a service called from a
  template (`view.Use` of a non-state type). `nexus lsp` shows them as editor
  warnings; `nexus generate views`, `nexus dev` (when they change) and
  `nexus doctor` print them. They never fail a build. `viewgen.Plan.Warnings`,
  `viewgen.WriteModule`.
- view: **live components** (LiveView's LiveComponents).
  `view.LiveComponent[*T](ctors…)` registers one; `@view.Component[*T](id, props)`
  places it on a live page, which keeps an instance per id while it renders it.
  `Mount(ctx, deps…, props)` on the first render, `Update` (or `Mount` again)
  when the props change, `view.UpdateComponent[*T](ctx, id, props)` from a page
  event or `Info`. Its events — `view.Send(c.M)`, `Submit`, `Change` — reach the
  instance they were sent from (Send names the method's component type; the
  browser finds the nearest instance of it). With `view.Assign` fields a
  component's event renders only the component and the spots around it, and a
  component re-rendered with the same props is skipped. The example board has
  one (`Cheer`).
- view: a field tagged `view:"-"` — one `Render` doesn't read, or that doesn't
  change once the page is mounted — leaves a page tracked (and unflagged by the
  compiler); generic page types (`Page[R, I]`) get the compiler's warnings too.
- view: **live uploads** (LiveView's `allow_upload`). A `view.Upload` field
  (`Allow(view.UploadConfig{Accept, MaxEntries, MaxSize})` in Mount) is a file
  input: `<input type="file" { p.Avatar.Input()... }/>`. Files are checked when
  chosen, sent at once — one request each to the page's route, its gates
  applying, bound to the user and good once — and streamed to temporary files
  while `Entries()` show `Progress`/`Done`/`Err` and the page re-renders.
  `Consume(fn)` hands finished files to an event and deletes them;
  `view.CancelUpload(&u, ref)` stops one; what a page leaves is deleted when it
  ends. Works in live components too.
- docs: "Coming from Phoenix LiveView", a table of LiveView's API and nexus's.

### Changed

- view: a render tree records an empty dynamic between two statics where a
  value rendered as nothing, so a frame's statics no longer depend on its
  values (an empty `value=""` used to resend the whole frame).
- view: less memory and CPU per live page. A connection's shadow tree keeps a
  dynamic's 128-bit hash inline and shares one value for every empty dynamic
  (−8% memory per page on a 200-row page); `view.Send`, `Submit` and `Change`
  cache what they read from a method value, and `LiveKey` caches per type.

## [2.15.0] - 2026-10-04

### Added

- view: **cross-replica broadcasts.** `view.UseRelay(r)` makes
  `view.Broadcast` reach the live pages of every replica: each broadcast is
  delivered locally as before and published, as JSON, through a `view.Relay`;
  every replica delivers the others'. `extension/cache/redis/viewrelay` is a
  Redis pub/sub relay (`viewrelay.New(viewrelay.Config{URL})`);
  `view.NewMemoryRelay()` works within a process. `view.Message.Decode(&v)`
  reads a message's data the same whether it came from this replica or another.

## [2.14.0] - 2026-10-04

### Changed

- view: live pages handle much more traffic. Measured on one 10-core machine
  with 20,000 connected users each clicking every ~5s: p99 latency from 880ms
  to 7ms; idle memory per page from ~100KB to ~61KB; event throughput up ~55%
  (about 30,000 events/s on a small page).
  - A page runs on a goroutine of its own once its socket is upgraded: the
    request that opened it returns, with its deep stack and buffers; the app's
    stop closes its live connections.
  - The tree a connection keeps for diffing is a shadow — fingerprints and
    hashes of dynamics, long markup only when over 1KB — and is let go when the
    page sits idle for `view.LiveIdleTrim` (2m).
  - Write buffers come from a pool, the read buffer is 1KB.
  - Fingerprints and loop-item keys hash with maphash (structurally, without
    rendering items), and the runtime's asset versions and the twins script are
    computed once instead of on every render.

## [2.13.3] - 2026-10-04

### Fixed

- view: parking a page reads `view.ResumeGrace` under the parking lock, so
  changing it while pages disconnect is not a data race (CI's `-race` run of
  the view tests failed since 2.12.0).

## [2.13.2] - 2026-10-04

### Changed

- view: a nested frame that only passes one dynamic through (a component or
  branch with no markup of its own) or holds nothing is folded into its parent,
  so changes travel without its nesting — a page change on a 10-row table is
  about 14% smaller.

## [2.13.1] - 2026-10-04

### Changed

- view: navigating to another live page sends the change from the page the
  browser holds — pages of one layout share its frames — instead of the new
  page's whole tree; a page that joined over HTTP fetches its tree once the
  socket is quiet (without patching), so its first navigation is a change too.

## [2.13.0] - 2026-10-04

### Added

- view: **live navigation**, as Phoenix LiveView's patch and navigate. From a
  live page, `view.Link` (and back/forward) moves over the page's socket: the
  same page with another query runs its new optional `Params(ctx, deps…, u
  *url.URL) error` and sends the tree's change; another live page is opened on
  the same connection through its route (gates, DI, path parameters) and sends
  its tree against the statics the connection holds; anything else loads over
  HTTP. `view.PushPatch` / `view.PushNavigate` from events; `Params` also runs
  after `Mount`; the runtime fires `nx:navigate` on `window` after navigating.

## [2.12.0] - 2026-10-04

### Added

- view: live pages update as a **render tree**, as Phoenix LiveView does. The
  view compiler has templ's generated Go record statics, dynamics, loops,
  branches and component renders (`viewgen.Instrument`, `view.Record`); a
  connection is sent each template's statics once and then only the dynamics
  that changed, loop items are kept, inserted or changed in place, and long
  markup shown again travels by reference. Plain `templ generate` output takes
  the same recorder with `go run …/view/viewgen/cmd/instrument <dir>`; the
  `view/ui` kit has it (`make view-ui`).
- view: a live page **resumes** after a dropped connection: the server keeps
  its state and subscriptions for `view.ResumeGrace` (30s) and a reconnect from
  the same page and user carries on with them. When the state is gone, the
  browser re-sends its `view.Change` forms before anything queued and holds the
  fresh render until they are answered (form recovery).

### Changed

- view: the live socket's replies are trees (`tree`, `full`, `reset`) instead of
  HTML or token patches; each connection starts with a `resume` token. The trace
  attribute `live.render` is `diff` or `full`.

## [2.11.1] - 2026-10-04

### Fixed

- `nexus generate handlers` (and the `nexus dev`/`nexus build` overlay): a
  `//nexus:use nexus.X(…)` no longer imports the nexus package twice.
- `nexus migrate v2` declares each undeclared nexus.toml section once — in the
  first package that reads it, or `main.go` when nothing does — instead of in
  `main.go` and every reading package, which panicked at init.

## [2.11.0] - 2026-10-04

### Added

- Auth: **an area can keep its own sign-in** — `[auth.areas.<n>] session = "<scheme>"`
  names a `session` scheme holding only that area's sign-in, in its own cookie scoped
  to the prefix, so one browser can be signed in to `/admin` and the app as different
  users. `SignIn` under the area uses it by default.
- Auth: **the identity as a handler parameter** — `*auth.Identity` (nil when
  anonymous) or `auth.Identity` (401 without a sign-in), on every transport.
  `nexus.RequestParam[T](fill)` lets any package add such a parameter type.
- `extension/auth/authdb`: tokens, keys and session records in your database
  (`authdb.Bind[DB]()`, table `nexus_auth_tokens`) — they survive restarts and are
  shared by replicas. `auth.Module` takes a `TokenStore` from DI.
- `nexus doctor` warns when an app with auth keeps its tokens in memory.

- Auth: **"Sign in with …"** — an `oidc` scheme (OpenID Connect
  authorization code + PKCE; discovery, ID token verified via JWKS, `state` and
  `nonce`); the account comes from `Users.FindLogin` by email, or the optional
  `Provisioner` (`Provision(ctx, scheme, claims)`) creates it.
- Auth: **roles** — `[auth.roles]` and `Identity.Roles`, expanded into `Perms`.
- Auth: an optional **permission catalogue** (`[auth] perms`): a gate or role
  naming an undeclared permission fails boot; Can/Gates/Check log one.
- Auth: `revocable = true` on a jwt scheme — `RevokeUser` reaches its tokens.
- Jobs enqueued while impersonating record the real user beside the
  impersonated one: `Record.Impersonator`, `Run.Impersonator()` (a jobsdb
  column, added by its migration). `nexus.RegisterRequestImpersonator` /
  `nexus.RequestImpersonator`, which extension/auth implements.

## [2.10.0] - 2026-10-04

### Added

- Auth: a `Users` optional method with the wrong signature (`SetPassword`,
  `CheckLogin`, `Public`) fails boot; a sign-in or forbidden page no route
  serves is logged at boot.
- `nexus.Error.RetryAfter`, sent as `Retry-After` on REST; the sign-in
  throttle and the OAuth2 token endpoint set it.
- `jobs.AsSystem()`: a job that runs without its enqueuer's identity.
- The Inertia `auth` prop is typed in `client.d.ts` (`NexusSharedProps["auth"]`).
- Dashboard Auth tab: a user's sessions and keys with Revoke, the registered
  policies; `dashboard.RegisterPageData` for plugins' per-request page data.
  The canvas's inspector and auth drawer show the setup.
- `nexus lint` warns about `[auth] default = "public"`.
- A production app keeping sessions in memory gets a boot warning.
- `viewtest.Browser`: a view page in headless Chrome (a built-in DevTools
  protocol client, no new dependency) — real clicks, typing, `Box`/`Visible`,
  `Eval`, `Viewport`, `Screenshot`, retrying `Expect`. Skips without Chrome
  (`NEXUS_CHROME`).

## [2.9.0] - 2026-10-04

nexus 2 has no users yet, so extension/auth's v1 API is removed now rather than
deprecated until v3: `auth.Module(auth.Config{Users: …})` is the only way to set
up auth. See [Migrating to v2 — Auth](docs/guide/migrating-to-v2.md#auth).

### Removed

- From `extension/auth`: `Single`, `Config.Authentication` / `Authorization` /
  `Backend` / `Endpoints` / `OnResolve` / `OnFail` / `OnError` /
  `LoginTokenField` / `CSRFCookie` / `CSRFHeader`, `Scheme`, `Resolver`,
  `Authentication`, `Authorization`, `Authority`, `Wildcard`, `Decision`,
  `Permit`, `Authenticated`, `PermissionFn`, `AnyOf`, `AllOf`,
  `DefaultPermissions`, `ExactAuthority`, `Backend`, `BackendOption`,
  `UseBackend`, `StaticBackend`, `Credentials`, `Authenticate`, `UserStore`,
  `ModelBackend`, `MemoryUserStore`, the extractors (`Bearer`, `Cookie`,
  `APIKey`, `Chain`, `Extractor`, `ExtractorFunc`, `ExtractorInfo`,
  `InspectExtractor`, `Describable`), `SessionCookie`, `CacheFor`,
  `CacheOption`, `CachedIdentity`, `Manager`, `Endpoints`, `LoginHandler`,
  `LogoutHandler`, `LoginIssuer`, `LogoutRevoker`, `LoginRequest`,
  `ErrorHandler`, `Optional`, `IdentityFrom`, `Subject`, `SubjectPtr`,
  `SubjectID`, `Principal`, and `Identity.Roles` / `Scopes` / `Extra`.
- `extension/oauth2` (the token endpoint and OAuth2 clients are in
  extension/auth) and `extension/inertia/iauth` (areas redirect page visits).
  The root module no longer depends on go-oauth2.

### Changed

- `Identity.Extra` is `Identity.User`; `Password.Username` is `Password.Login`.
- `viewtest.As` takes an `*auth.Identity`.
- `nexus new --auth` scaffolds a `Users` stand-in with `[auth]` (session + bearer
  + endpoints) instead of an oauth2 server.
- The dashboard's Auth tab shows the setup: schemes, areas, every endpoint's
  gate (Public ones flagged), throttle locks with unlock, and a sign-out-everywhere
  form.
- `nexus migrate v2` renames `auth.Subject` → `auth.ID` and `auth.Optional` →
  `auth.Public`, and flags every other use of the removed API.

## [2.8.0] - 2026-10-04

### Added

- **Auth: OAuth2 clients** at the token endpoint:
  - `[auth.oauth2.clients.<id>]` (`secret` or `secret_hash`, `grants`,
    `perms`, `kind`), or `Config.Clients` (`auth.Clients`) for clients in a
    database.
  - Client authentication by HTTP Basic or `client_id`/`client_secret`;
    `invalid_client` / `unauthorized_client` per RFC 6749.
  - The `client_credentials` grant: a token for the client itself (identity
    `client:<id>`, its perms and kind, no refresh token).
  - `[auth.oauth2] require_client`: the password and refresh_token grants
    need a known client too.

## [2.7.0] - 2026-10-04

### Added

- **Auth stage 4** on the `Config.Users` path:
  - Impersonation: `auth.Impersonate` / `auth.StopImpersonating`,
    `Identity.Actor`, `[auth.impersonation]` (`permission`, `endpoint`). No
    escalation past the actor's permissions, no nesting; the credential stays
    the actor's. `me` and the Inertia `auth` prop carry `actor`.
  - Per-object rules: `auth.Policy[T]`, `auth.Check(ctx, perm, obj)`,
    `auth.Allowed`.
  - Jobs run as the user who enqueued them, loaded through `Users.Load` at
    start; a job whose user is gone fails without retrying.
  - `nexus auth check [nexus.toml]`, from `auth.Explain` / `auth.ExplainTOML`.
  - `extension/auth/authtest`: `As`, `AsUser` (a test-binary-only credential)
    and an in-memory `Users`.
- `nexustest`: `App.With(header)`.
- `config.DecodeTable`: one nexus.toml table decoded as a `Section` would.
- `nexus.RegisterIdentityRestorer` / `nexus.RestoreIdentity`.

### Fixed

- The docs gave refresh lifetimes as `"30d"`, which Go durations don't parse;
  they read `"720h"`.

## [2.6.0] - 2026-10-04

### Added

- **Auth stage 3c: devices and API keys** on the `Config.Users` path:
  - `auth.Sessions(ctx, userID)` lists where a user is signed in — sessions
    and bearer tokens, with when, User-Agent, IP and which one is this
    request's — and `auth.RevokeSession` ends one. Sessions signed in from
    this version get a record in the token store for it.
  - `auth.Keys.Create/List/Revoke`: named API keys for an apikey scheme.
  - `auth.TokenLister`, an optional `TokenStore` method the memory store and
    `CacheTokens` implement (the cache store keeps a per-user index);
    `StoredToken` gains `Name`, `Created`, `Agent` and `IP`.

## [2.5.0] - 2026-10-04

### Added

- **Auth stage 3b: tokens** on the `Config.Users` path:
  - Refresh tokens: a bearer scheme's `refresh` lifetime; `Credential.RefreshToken`;
    `auth.RefreshToken(ctx, rt)` rotates the pair. Refused as an access token,
    and after `RevokeUser`.
  - `[auth.endpoints] token` — an OAuth2 token endpoint (password and
    refresh_token grants, RFC 6749 errors) — and `revoke` (RFC 7009). Both skip
    CSRF.
  - A `jwt` scheme verifying tokens issued elsewhere: `secret` (HS256),
    `public_key` (PEM, RS256/ES256) or `jwks` (cached, refetched for an unknown
    `kid`); `issuer`, `audience`, `subject`, `leeway`. The algorithm is pinned
    to the key. It shares the Authorization header with a bearer scheme by token
    shape.
- `App.ExemptCSRF(path)`: exempt one endpoint that sets no cookie from CSRF.

## [2.4.0] - 2026-10-04

### Added

- **Auth stage 3a: ending sessions** on the `Config.Users` path:
  - A per-user epoch, kept in the `TokenStore`: every session and token
    records the one it was issued under (`StoredToken.Epoch`).
  - `auth.RevokeUser(ctx, userID)` — sign out everywhere; `auth.Revoke(ctx,
    token)` — end one token or API key.
  - `[auth.sessions]`: `single` (a sign-in ends the user's other sessions),
    `end_on_password_change` (default true; the session changing it stays),
    `idle`.
  - WebSocket (`AsWS`) and live-view connections check before each message
    and close with `Unauthenticated` once the user's epoch moves.
- `nexus.RegisterConnectionCheck` / `nexus.CheckConnection`: checks run
  before each message on long-lived connections.

## [2.3.0] - 2026-10-04

### Added

- **Auth stage 2b** on the `Config.Users` path:
  - Inertia pages get an `auth` prop, `{user, can}` — the `me` endpoint's
    shape — on every render; `[auth] page_prop` renames it, `"-"` turns it off.
  - `auth.SignIn` and `auth.SignOut` rotate the CSRF token.
  - `forbidden` (per area, or in `[auth]`): the page a refused page visit is
    sent to; API calls still get 403.
  - `Config.Throttle` with `auth.CacheThrottle(cache)`: the sign-in throttle
    shared between replicas.
- `nexus.RegisterSharedPageProp`: a prop every page renderer shares; the
  app's own shared prop of the same key wins. extension/inertia applies them.
- `secure.RotateCSRF(ctx)`: a new CSRF token with this response.

## [2.2.0] - 2026-10-04

### Added

- **Auth stage 2 (sign-in flows)** on the `Config.Users` path:
  - **Areas** — `[auth.areas.<name>]` with `prefix`, `kinds`, `login`, `home`:
    endpoints under the prefix need a sign-in of one of the kinds; an
    unauthenticated page visit goes to the area's sign-in page with `?next=`
    (302, or 409 + `X-Inertia-Location` for Inertia), outside areas to
    `[auth] login`; signing in under an area refuses other kinds.
  - **`next`** — one validator (no `//host`, backslash, scheme or control
    character, as given and after each decoding round); `Credential.Next`,
    `auth.Next(ctx)`, `auth.ReturnTo(next)`, `[auth] home` and `next_param`.
  - **Login throttling** — `[auth.throttle]` per account and per client IP,
    with a lockout; 429 past the limit.
  - **Built-in endpoints** — `[auth.endpoints]` `login`, `logout`, `me`; `me`
    answers `{user, can}`, the user from an optional `Users.Public` method.
  - `auth.OpGates` reports ops the visitor can't call (not signed in, wrong
    area kind) as false on this path.
- `nexus.Defer(fn)`: an option built at boot, after nexus.toml is loaded.

### Fixed

- A failed `auth.Login` answers "invalid login or password" as the message,
  not only under `errors._global`.
- A production app keeping bearer tokens or API keys in memory gets a boot
  warning.

## [2.1.0] - 2026-10-04

### Added

- **Auth: permissions with wildcards, user kinds, any-of gates.** The first slice
  of the v2 auth design, shipped additively (docs/design/v2-auth.md, "Shipping in
  2.x"):
  - `Identity.Perms`, matched with wildcards (`orders.*` grants `orders.view`,
    `*` grants everything); `Roles` and `Scopes` keep exact matching.
  - `Identity.Kind` and the `auth.Kind("staff")` gate; `auth.RequiresAny(…)`
    passes with any one permission. Both imply sign-in, combine with `Requires`,
    and count in `auth.OpGates`. Directives: `//nexus:auth Kind staff`,
    `//nexus:auth RequiresAny a b`.
  - `auth.Current(ctx)` and `auth.ID[T](ctx)`, the new spellings of
    `IdentityFrom` and `Subject`.
  - `middleware.Middleware.Tags`: registry tags a bundle stamps on every
    endpoint it is attached to.
- **Auth: accounts and sign-in (`Config.Users`).** The config-driven path of
  the v2 auth design, beside the resolver path, which is unchanged:
  - The app implements `auth.Users` (`FindLogin`, `Load`; optional
    `SetPassword`, `CheckLogin`) and passes `auth.UseUsers(NewUsers)`; a type
    that doesn't implement it fails boot naming the method.
  - Schemes come from `[auth.schemes.*]`: `session` (the default; installs
    `extension/session` and turns CSRF on), `bearer` (opaque 256-bit tokens,
    stored as SHA-256) and `apikey`. `Config.Tokens` stores them — memory by
    default, `auth.CacheTokens(cache)` for Redis.
  - `auth.Login`, `auth.SignIn` (`auth.Using(scheme)`), `auth.SignOut`,
    `auth.SetPassword`, `auth.Refresh`, `auth.Public`, `[auth.passwords]`.
  - Every endpoint requires a sign-in unless it is `auth.Public()`;
    `[auth] default = "public"` turns that off. `Users.Load` is cached per user
    id (`[auth] cache`, 5 minutes).
  - A credential that arrives but fails leaves the request anonymous; the 401
    names the reason under `nexus dev`, and the trace carries it.
  - `Identity.Scheme` names the scheme that authenticated the request.
- `session.Install(app, cfg)` and `session.Present(ctx)`; a second
  `session.Module` is a no-op with a warning instead of a second middleware.

## [2.0.1] - 2026-10-04

### Fixed

- **View pages work with CSRF on.** Shard re-renders are POSTs, and they
  carried no token, so an app that turned CSRF on (cookie sessions, a cookie
  auth scheme, Inertia, or `csrf = true`) got a 403 for every shard. The view
  runtime now sends `X-XSRF-TOKEN` with each re-render, and gives a plain
  same-origin POST form a `csrf_token` field as it submits (a form with its
  own field keeps it). Live-page events are unchanged: the WebSocket is
  same-origin checked.
- The security guide's Go example used the v1 field `EnableCSRF`; it is
  `CSRF: new(true)` in v2.

### Added

- `viewtest` pages expose their cookie jar as `document.cookie`.

## [2.0.0] - 2026-10-04

nexus 2.0 collects every breaking change in one major version. Most of the
move is mechanical: `nexus migrate v2` rewrites imports, renamed symbols,
annotations, tags and misplaced nexus.toml keys, and leaves a
`// TODO(nexus v2): …` comment where a step needs a person. Step-by-step
notes: [Migrating to v2](docs/guide/migrating-to-v2.md). Design and
decisions: [docs/design/v2.md](docs/design/v2.md).

### Breaking

- **Module path `github.com/paulmanoni/nexus/v2`.** Every import moves to
  `/v2`; the separate modules (`cmd/nexus`, `di/fxcontainer`,
  `httpx/ginrouter`, `extension/cache/redis`, `extension/jobs/jobsredis`,
  `extension/jobs/jobsamqp`) take `/v2` at the end of their own path. `view`
  is now a package of the root module (`…/nexus/v2/view`), no longer a module
  of its own. *Codemod: imports and go.mod.*
- **Annotations are Go directives: `//nexus:x`.** `//@rest`, `//@query`,
  `//@auth`, `//@controller`, `//@page`, custom `//@pkg.Func` and the rest are
  spelled `//nexus:rest`, …; the grammar after the prefix is unchanged. The v1
  spelling (`//@x`, gofmt's `// @x`) is a `file:line` error, and an unknown
  `//nexus:` keyword is always an error. *Codemod: `.go` and `.templ`.*
- **An op is named after its handler as written.** v1 dropped a `New` prefix
  (`NewListPets` → `listPets`); v2 uses the method or function name
  (`newListPets`), or `nexus.Op("…")`. On GraphQL that name is the field name.
  *Codemod: adds `nexus.Op("xxx")` to `AsQuery`/`AsMutation`/`AsSubscription`
  registrations of `NewXxx` handlers (and `//nexus:use nexus.Op("xxx")` to
  annotated ones) so GraphQL field names don't move.*
- **The config package.** Runtime config types, the nexus.toml loader,
  `Get` and its store, dotenv and env bridging leave the root:
  `nexus.Config` → `config.Runtime`, `ServerConfig` → `config.Server` (and the
  other `*Config` types), `MustLoadConfig`/`LoadConfig` → `config.MustLoad` /
  `config.Load`, `nexus.Get`/`MustGet` → `config.Get`/`config.MustGet`,
  `HasConfig` → `config.Has`, `OnConfigChange` → `config.OnChange`,
  `BindConfig` → `config.Bind`, `ConfigVersion` → `config.Version`,
  `ConfigError` → `config.Error`, `LintRuntimeFile` → `config.LintFile`,
  `nexus.Cache` → `resource.Cache`. *Codemod: symbols (full table in
  `nexus migrate v2 --help`).*
- **The dev and notify packages.** `PreserveDev` → `dev.Preserve`,
  `PreserveDevJSON` → `dev.PreserveJSON`, `DevStateDir` → `dev.StateDir`,
  `DevState` → `dev.State`, `IsDev` → `dev.Enabled`, `NexusDevEnv` /
  `NexusDevRootEnv` → `dev.Env` / `dev.RootEnv`; `Notifier` / `NewNotifier` /
  `Bus` → `notify.Notifier` / `notify.New` / `notify.Bus`. *Codemod: symbols.*
- **nexus.toml is strict.** An unknown key, a key in the wrong table (a
  top-level `environment`, an `addr` under `[runtime]`), a section nobody
  declared, or an `[extensions.x]` block without a registered decoder fails
  boot with its line and a did-you-mean. v1 warned and ignored them. App
  sections are declared with `config.Section[T]`. *Codemod: moves keys the
  check pins to one table; declares each undeclared app section free-form
  (`config.Section[map[string]any]`) with a TODO to type it; typos are left
  for `nexus config check`.*
- **One error model.** `nexus.Error{Code, Message, Fields, Cause}` with eight
  codes (`InvalidInput`, `Unauthenticated`, `Forbidden`, `NotFound`,
  `Conflict`, `TooMany`, `Unavailable`, `Internal`), built with
  `nexus.Err` / `nexus.Errf` / `nexus.Invalid()`. Every transport renders an
  error through one table: REST answers the code's status with
  `{code, message, errors}`, GraphQL carries `extensions.code`, WebSocket
  sends `{type, code, message, errors}`, Inertia flashes or renders its error
  page. An uncoded error is `Internal`, its message hidden outside
  `nexus dev`. `nexus.Errors`/`NewErrors` → `nexus.Error`/`nexus.Invalid()`,
  `ErrForbidden` → `nexus.Forbidden`, `MapCRUDError` removed; the boot option
  `nexus.Error(err)` is `nexus.FailBoot(err)`; `ErrCRUDValidation` is a 422.
  *Codemod: renames; `MapCRUDError` call sites get a TODO.*
- **`validate:` tags run on every transport**, REST included; a failure is an
  `InvalidInput` error with per-field messages, and so is a binding failure.
- **App-wide middleware is `nexus.Middleware(…)`.** `Config.Middleware.Global`
  is gone; `nexus.Middleware` takes middleware values or DI constructors, placed
  by `middleware.Stage` (`Edge`, `Session`, `Auth`, `App`) and declaration
  order. *Codemod: a TODO on each `Middleware.Global` line.*
- **`middleware.Middleware.Gin` is `HTTP`.** *Codemod: composite literals;
  a field read through a variable is left to the compiler.*
- **`ServeFrontend` is `nexus.Frontend`**, which also provides `*nexus.Document`
  (the page shell; Vite's live one under `nexus dev`) into DI. The CLI finds
  the frontend dir from a `nexus.Frontend(...)` call. *Codemod: symbols.*
- **Logging is `log/slog`.** `App.Logger()` returns `*slog.Logger` and the
  framework provides it into DI; `nexus.Managed`'s build func takes
  `*slog.Logger`; `db.WithLogger`/`Provide`/binders and `extension/cache`'s
  `NewManager`/`Provide`/`Bind`/`Manager.Logger()` use `*slog.Logger`;
  `resource/connlog`'s `Transition.Fail`/`OK` return `[]slog.Attr`. Replace
  the logger with `nexus.WithLogger(l)`; providing a second `*slog.Logger` by
  hand fails boot. nexus links no zap.
- **`MustLoadExtensions` returns one `nexus.Option`**, and
  `LoadExtensionOptions` is `LoadExtensions`, returning `(Option, error)`.
  *Codemod: drops the `...` spread.*
- **Request bodies are capped at 32MB by default** (`max_body_bytes`; `-1`
  turns it off). An over-limit JSON body is a 413, not a 400.
- **CSRF follows what the app uses.** `[runtime.middleware.security] csrf` is
  tri-state: unset turns CSRF on once extension/session, a cookie-reading
  auth scheme or Inertia asks for it (`App.RequireCSRF`), and
  leaves a token-only API without it; `true`/`false` force it.
- **`AsCRUD` is removed** — a resource is a controller:
  `nexus.Resource[*PetsController]("/pets")` registers the same routes
  `nx.crud` calls. *Codemod: a TODO on each call.*
- **Client IP is built in.** `nexus.ClientIP(ctx)` reads the caller's address
  (honouring `trusted_proxies`) on REST, GraphQL and WebSocket;
  `ClientIPFromCtx` → `nexus.ClientIP`, and `WithClientIP` (root and
  `extension/ratelimit`) is removed. *Codemod: symbols; a TODO for
  `WithClientIP`.*
- **Dotenv is loaded by nexus.toml.** `MustLoadDotenv` / `LoadDotenvIfPresent`
  options are gone: the loader applies `.env` beside nexus.toml, or the files
  `[runtime] dotenv` lists (`!` marks one required), before `${VAR}`s expand.
  `config.LoadDotenv` / `config.RequireDotenv` return errors. *Codemod: drops
  the option with a TODO.*
- **Listeners start last and stop first**, after every resource and worker
  has started; `manifest.StartupTask.Run` takes the boot context.
- **`uri:"x"` struct tags are no longer read**; use `path:"x"`. *Codemod: tags.*
- **GraphQL sits behind a seam.** No graphql-go type is in the public API:
  `graph` and `transport/gql` are internal, and the surface is the new `gql`
  package — `Params[T].Info` is a `gql.Info`, and `GraphMiddleware` and
  `middleware.Middleware.Graph` take a `gql.Middleware` over a `gql.Field`.
  `RegisterGqlType` declares an enum or input object from a Go type
  (`RegisterGqlType[T](name, values…)`). Removed: `Service.MountGraphQL`,
  `WithArgValidator` and the graph validators (use `validate:` tags),
  `GqlField`/`GqlFieldGroup`, `graph.GetRootInfo`/`GetRootString`.
  *Codemod: `graph.FieldMiddleware`/`FieldResolveFn`/`ResolveParams` →
  `gql.Middleware`/`Resolver`/`Field`, `SetStatusCode` →
  `nexus.SetGraphStatus`; TODOs for the removed names.*

### Added

- **`AsRest` takes a handler factory**: a function taking only DI deps and
  returning `httpx.HandlerFunc` is built once at boot and mounted — what
  `AsRestHandler` did. *Codemod: `AsRestHandler` → `AsRest`.*
- **`nexus.Setup(fns…)`** — pre-serve work (migrations, roles, indexes,
  seeds) with DI parameters and the boot context, run after resources start
  and before the listeners open; the first error stops boot. *Codemod: a TODO
  on `nexus.Invoke` of `Ensure*`/`Migrate*`/`Seed*`/`Backfill*` functions.*
- **`nexus.MaxBody(n)` and `nexus.Timeout(d)`** — per-endpoint body cap and
  deadline.
- **Error helpers:** `nexus.ErrorOf`, `nexus.CodeOf`, `nexus.WriteError` (a raw
  `*httpx.Ctx`), `nexus.Validate`, `middleware.ErrorBody` and
  `middleware.Rejection`.
- **`config.Section[T](name, default…)`** declares and decodes an app's own
  nexus.toml table.
- **`nexus config check`** (CI; `--json`) and **`nexus config schema`**; the
  framework's JSON schema is published as `nexus.toml.schema.json` and
  scaffolds carry a `#:schema` line. `config.Check`, `config.JSONSchema` and
  `config.DeclareSchema` for tools.
- **`nexus migrate v2`** — the v1 → v2 codemod (`--dry-run` prints every
  change; re-running is a no-op).
- **OpenTelemetry export** — `[runtime.telemetry] otlp_endpoint` posts
  finished spans to a collector as OTLP/HTTP JSON (package `trace/otlp`),
  without linking the OpenTelemetry SDK.
- **`nexus lsp`** — proxies gopls and opens the generated Go (compiled views,
  `//nexus:` registrations) as editor buffers, with `.templ` diagnostics,
  definition, hover, completion and references mapped through templ's source
  map.
- **`nexus test` / `nexus vet`** run `go test` / `go vet` through the build
  overlay, so no generated file is needed on disk.
- **`nexus doctor`** with no argument checks the project: Go against go.mod,
  the module on nexus v2, strict nexus.toml, Node / the package manager / Vite,
  the Tailwind CLI and generated view files (a manifest on stdin is
  `nexus doctor -`).
- **`nexus release`** — the multi-module release: version and CHANGELOG
  checks, root tag, every dependent module moved, tagged and pushed in order,
  then the CLI install check (plan only without `--yes`).
- **`nexus/view/ui`** — a component kit for views (Button, Field and inputs,
  Tabs, Dialog, Dropdown/RowActions, DataTable with server paging/search/sort,
  Toast, Loader, PageHeader, Badge), and **`nexus add ui <component>`** to
  vendor one into the app.
- **`view/viewtest`** — drive view pages end to end in a Go test without a
  browser (`viewtest.Mount[*T]`, `viewtest.Get`; Fill/Select/Check/Click/
  Submit; retrying `Expect`).
- **`view.Value(v)`** marks a form field server-owned.
- **Pages and live events on the dashboard** — pages, live pages and shards
  are listed (PAGE / LIVE / SHARD) with a live page's events; each live event
  is its own trace. `auth.OpGates` keys a page under its component too.
- `viewgen.GenerateWith` reads editor buffers and keeps source maps; a templ
  syntax error is a `PositionError`.

### Changed

- **`nexus dev` compiles views in memory** and overlays them into the dev
  build; `*_templ.go`, `view_gen.go` and `view_imports_gen.go` are no longer
  written (`--view-files` keeps the old behaviour for an editor on plain
  gopls).
- **View form fields follow documented rules**: a textarea follows the
  server's value like an input; a focused field keeps what it shows; a
  checked checkbox without a value binds as `true`.
- **The dev log view reads slog JSON** (and zap's shape, for apps that still
  log with zap).
- The Vite hot-file and manifest readers are public as `frontend/vitehot` and
  `frontend/vitemanifest`; the connection-state logger is `resource/connlog`.
- The root package is consolidated into files named by topic.
- Scaffolds write handlers as methods and take the framework's
  `*slog.Logger`.

### Removed

- `auth.LoginEndpoint` / `auth.LogoutEndpoint` — use `auth.Config.Endpoints`
  (`auth.LoginHandler` / `LogoutHandler` stay exported).
- `auth.Describe` — `auth.InspectExtractor`. *Codemod: symbols.*
- `nexus.UseVolume` / `App.UseVolume` — `nexus.DeclareVolume`. *Codemod: the
  function form.*
- `AppFromGin`; the `extension.Plugin.Generate` driver slot,
  `extension.Generate`, `App.RegisterGenerateDriver` / `GenerateDrivers` and
  `PluginRecord.HasGenerate` (nothing read them).
- The `crud` marker package, its `MemoryResolver`/store adapters and
  `storage/gorm`; the unused `multi` package.
- `nexus dev --go-run` (the legacy loop), `nexus dev --frontend-cmd`,
  `nexus new --tooling`, the viteless migration hints and the
  `NEXUS_VITE_DEV` fallback.

## [1.80.0] - 2026-10-04

The bridge release to 2.0: nothing changes behaviour; the app tells you,
while still on v1, everything nexus 2.0 removes, renames or changes, so
`nexus migrate v2` holds no surprises. See
[Preparing for 2.0](docs/guide/v2.md).

### Added

- **v2 notices at boot under `nexus dev`.** Every v1 API that 2.0 removes or
  renames records its use where the app reaches it, and the dev boot prints
  one block: the API, the file:line that called it, and the 2.0 replacement.
  Covered: `nexus.Config` via `Run`, `LoadConfig`/`MustLoadConfig`,
  `Get`/`MustGet`, `ServeFrontend`, `AsCRUD`, `AsRestHandler`,
  `NewErrors`, `MapCRUDError`, `ErrForbidden`, `Config.Middleware.Global`,
  `IsDev`, `PreserveDev`/`PreserveDevJSON`, `DevStateDir`, `NewNotifier`,
  `MustLoadDotenv`/`LoadDotenvIfPresent`, `UseVolume`, the `nexus.Error`
  boot option, `RegisterGenerateDriver`, `AppFromGin`,
  `WithClientIP`/`ClientIPFromCtx`, `auth.Describe`,
  `auth.LoginEndpoint`/`LogoutEndpoint`, `uri:` tags, a `*zap.Logger` in
  the DI graph, unknown nexus.toml keys and an unset `max_body_bytes` — and
  each GraphQL op named from a `NewXxx` handler without `nexus.Op`, with the
  name 2.0 gives it and the `nexus.Op` that keeps the wire name. Reported
  once per process; calls from nexus's own packages don't count; production
  binaries record nothing. `NEXUS_V2_NOTICES=0` silences them.
- **`nexus lint --v2 [dir]`**: the same list found statically, by file:line —
  root symbols moving to `config`/`dev`/`notify`, removed APIs, `//@`
  annotations in `.go` and `.templ` files, `uri:` tags, `NewXxx` op names,
  `middleware.Middleware{Gin: …}`, zap imports, and the nexus.toml keys and
  undeclared sections 2.0's strict config rejects. Advisory (exit 0);
  `--json` for tooling.
- **`//nexus:x` annotations.** Every annotation also reads in Go's directive
  form — `//nexus:rest GET /users/:id`, `//nexus:controller`,
  `//nexus:module`, `//nexus:inertia.Page`, `//nexus:page` in `.templ` — the
  only spelling 2.0 reads. `//@x` keeps working and is reported once per
  `nexus dev` / `nexus build` / `nexus generate handlers` run. An unknown
  `//nexus:` keyword, or a spaced `// nexus:rest`, is an error.

### Changed

- `cmd/nexus` requires deco v0.20.0 (`transpiler.ScanWith`).

## [1.78.2] - 2026-10-02

### Added

- **Events on generic live pages.** `view.SendTo(recv, "Method", args…)`,
  `view.SubmitTo` and `view.ChangeTo` name an event by its method name on the
  live page, checked against the page's type when it renders. Go builds a
  generic type's method values as closures without their method's name, so
  `view.Send` couldn't name them; it now explains that instead of saying the
  argument isn't a method value.
- **A form event may take `url.Values`**: the fields as sent, for an editor
  that binds them itself.

## [1.78.1] - 2026-10-02

### Fixed

- **The architecture canvas stays legible in big apps.** A layer holding
  dozens of modules was one column, so the graph was a tall sliver whose
  cards fit-to-view shrank past reading; tall layers now wrap into balanced
  columns. The canvas's toolbar, highlight and zoom buttons are compact
  again (its scoped element styles had started to outrank the components'
  own), and the inspector labels its count of groups Modules.

## [1.78.0] - 2026-10-02

### Changed

- **The dashboard is a templ + templUI console.** `/__nexus` is now
  server-rendered: one tab per surface — Architecture, Endpoints, Services,
  Resources, Workers & Crons, Traces, Auth and Runtime — under a slate top
  bar and a live status bar, with dense tables in light or dark. The
  Architecture tab keeps the Vue topology canvas, embedded and re-skinned to
  match; its bundled web fonts are gone.
- **Large apps stay fast.** Endpoints is scoped by a module (or service)
  rail, and big lists are searched, sorted and paged on the server, with the
  view kept in the URL; an 850-service app's Endpoints page went from 3.8 MB
  of HTML to under 150 KB. Live updates re-render what is on screen in place
  and cost a 304 when nothing changed.

### Added

- **An endpoint page** with its input schema, recent errors (with stacks), a
  REST/GraphQL/WebSocket tester and live rate-limit overrides.
- **Auth's rejection stream on the console**: the 401/403s still in the trace
  buffer, newest first, each linked to its trace, updating as they happen.
- `dashboard.Config.SchemaRefs`, `trace.Bus.Recent` and `trace.Bus.Spans`.
- `make dashboard` regenerates the console's templ code and Tailwind CSS
  (both committed; a plain `go build` needs neither tool).

### Dependencies

- `github.com/a-h/templ` and `github.com/Oudwins/tailwind-merge-go`. The
  templUI components are vendored, so templUI's own module graph is not
  pulled in.

## [1.77.3] - 2026-10-02

### Fixed

- **A `<select>` follows the server's choice on live pages.** Once a user
  picked an option, a browser stops letting the options' `selected`
  attributes move the selection, so a select kept the user's old choice
  after the server rendered a different one (a form reset to new defaults,
  another record loaded) and the next form event sent the stale value back.
  A select now takes the option the server marks whenever that changed and
  the select doesn't have focus, the rule text fields already follow; a
  user's choice the server agrees with is left alone.

## [1.77.2] - 2026-10-01

### Fixed

- **In-app navigation only patches in view pages.** `view.Link` and the Back
  button patched whatever HTML a fetch returned into the page, so a redirect
  (an expired session, a page another app serves) could put a foreign page,
  such as an Inertia shell, into a templ document without the styles its
  server adds on a real page load. A page that doesn't load the view runtime
  now gets a full page load, and Back is handled in place only for history
  entries the runtime made; the browser restores every other entry itself.

## [1.77.1] - 2026-10-01

### Added

- **`data-nx-ignore` for live pages.** An element with an `id` and
  `data-nx-ignore` is left as the browser has it when a live page or in-app
  navigation patches the page, until a render gives it another `id`. Use it
  for markup a script owns, such as a chart's canvas (the patch would strip
  the size the chart library set) or an app shell's open menus.

## [1.77.0] - 2026-10-01

### Added

- **Islands in templ views: `view.NewIsland`.** An island mounts a component
  of the project's Vite frontend into a templ page, for widgets a JavaScript
  framework does better (editors, charts, drag and drop).
  - Declare one with its props type:
    `var Chart = view.NewIsland[ChartProps]("Chart")`.
  - Place it like a component: `@Chart(props, view.Visible()) { fallback }`.
    The children are the server-rendered fallback until it mounts.
  - Each file under `web/src/islands` is an island named by its path:
    `.vue` mounts with Vue, `.tsx`/`.jsx` with React, and a `.ts`/`.js`
    module exports `mount(el, props, ctx)`.
  - It mounts on load, or later with `view.Idle()`, `view.Visible()` or
    `view.Media(query)`. Each island is its own lazily loaded chunk.
  - **Typed props.** The client SDK types each island's props as
    `NexusIslandProps['Chart']`, from the Go type.
  - **Page signals.** A `*view.Signal` field stays live: the island gets the
    value and is updated when it changes. A Vue island sets it back with
    `update:<prop>` (so `defineModel` works), a React island with
    `set<Prop>`, and a `mount` module with `ctx.set`.
  - **Server rendering.** `view.SSR()` renders the island on the islands
    server, and the browser hydrates it. `nexus({ islands: { ssr: true } })`
    builds `web/dist/ssr/islands.js`, a Node server with its dependencies
    inside; `view.IslandServer(url)` points elsewhere. Under `nexus dev`,
    or when the server is down, the island renders in the browser.
  - **Live pages and navigation.** On a live page a re-render updates a
    mounted island's props without remounting it. `view.Link` and shard
    re-renders unmount islands that leave the page.
  - **Dev and build.** `nexus dev` loads islands from Vite with hot reload;
    `nexus build` adds the islands loader to the Vite build, and the binary
    reads it from the manifest. A frontend made only of islands needs no
    `index.html`.
  - **Without a build.** In a Go test, or with a build that lacks the island,
    the page still renders and the island's `data-error` says why.
- **nexus-vite-plugin `islands` option.** It takes a directory, `false`, or
  `{ dir, ssr }`; the default is `src/islands`.
  - It builds the `nexus-islands` entry and tells the app, through the dev
    hot file, that Vite serves islands.
  - It warns in dev, and fails the build, when an island declared in Go has
    no file.
  - `web/src/islands/_setup.ts` installs Vue plugins or wraps React islands
    in providers.
- **`registry.SchemaAs`.** A type can say it appears to clients as another
  type; `view.Signal[T]` types as `T`.

## [1.76.0] - 2026-10-01

### Added

- **Reactive templ views: `github.com/paulmanoni/nexus/view`.** This is a new
  module. It renders templ components and keeps them reactive without a
  JavaScript build. What updates depends on what each template reads.
  - Component state: `view.State(ctx, v)`.
  - Shared page state through DI: a struct of `*view.Signal` fields, read
    with `view.Use[*T](ctx)`. Each page render gets its own copy.
  - Text and attributes that read a signal update in the browser.
  - `Set` and `view.Do` are the actions.
  - An `if` on a signal is decided in the browser.
  - Shards are components re-rendered on the server. They are nexus ops that
    inherit their pages' gates.
  - `//@page`, `//@auth` and `//@use` directives above a component register
    it, with no wiring.
- **`nexus dev` compiles views.** On start and on every `.templ` save, the
  generated Go is written to disk for gopls, and the usual rebuild follows.
  - It also runs the Tailwind standalone CLI for stylesheets that import
    Tailwind.
  - It writes `sources.generated.css`, covering Go dependencies that ship
    `.templ` files, so imported component libraries such as templUI get
    their classes.
- `nexus build` compiles views through the build overlay and builds
  Tailwind stylesheets minified.
- New command: `nexus generate views [--check]`.
- **Live pages: `view.Live[*T](prefix, gates…).Provide(NewT)`.** Each
  connected page holds its own state on the server, and events travel over
  a WebSocket. It is declared like a Resource.
  - Conventions: `Mount`, a templ method component `Render()`, and events,
    which are exported `func(ctx, deps…, args…) error` methods.
  - `view.Send(x.Method, args…)` sends an event.
  - The page is re-rendered and patched in place, so focus and typing
    survive.
  - Signals keep working for browser-only state.
  - **Server push:** `sock.Subscribe(topics…)` in `Mount`, then
    `view.Broadcast(ctx, topic, data)` from anywhere. Each subscribed page
    runs its optional `Info(ctx, deps…, msg view.Message) error` and
    re-renders. Delivery is in-process, so it reaches only the pages
    connected to that replica.
  - **Forms:** `view.Submit(x.Add)` and `view.Change(x.Validate)` send a
    form's fields to an event whose last parameter is a `form:`-tagged
    struct.
    - `view.Change` sends them as the user types, debounced.
    - Returning `nexus.Errors` re-renders the page with
      `view.Errors(ctx).Field(name)`.
    - A successful submit resets the form.
  - A panic in a live page is reported to it as an error, and the
    connection stays up.
  - **The first render arrives over HTTP only.** The connecting socket sends
    nothing unless its own mount renders differently.
  - **Updates send only what changed.** A patch carries text nodes,
    attribute values and `view.Send` arguments as values, reuses markup
    already on the page, and refers by index to markup the connection has
    seen before.
    - On a 100-row board: 15 B for a count change, 92 B for a new row.
    - Messages are compressed.
  - **Reconnects** use jittered backoff, retry at once when the network or
    the tab comes back, and queue events while disconnected. The server
    mounts fresh.
- **`view.Link`** is in-app navigation.
  - It fetches the target page and patches it into the current one, with no
    document reload.
  - Live sockets connect and close with the page.
  - The title and any stylesheets or scripts the new page needs are updated.
  - Back and forward work.
- `view.Assets(prefix, handler)` serves a component library's files.
  `templ.Attributes{…}` literals in `Props.Attributes` accept reactive
  entries.

### Fixed

- Handler codegen ignores `//@` directives that templ copies from a view's
  doc comment into its generated `*_templ.go`.

## [1.75.1] - 2026-09-30

### Fixed

- `//@use` expression identifiers that name a top-level declaration of the
  annotated package are recognised as values and skipped before the import
  cascade — previously such an identifier missed every layer and forced a
  module-graph rebuild (`go list -deps`) on every scan, adding ~1s per save
  on a large app; it could also synthesize an import that shadowed the
  declaration in the generated file.

## [1.75.0] - 2026-09-30

### Added

- `nexus.DecoratedModules(names...)` scopes which `//@`-registered modules a
  boot accepts — for test binaries whose files link several annotated packages,
  an `InProcess` boot of one module no longer trips over the others' providers.
  With no names it drops every decorated registration; without the option,
  everything drained participates as before.

### Changed

- `decorate.Drain` now returns a snapshot without clearing the registry, so
  every boot in a multi-boot process (a test binary) sees the same
  registrations instead of only the first; `decorate.Reset` remains the
  explicit clear. A production process boots once, so nothing changes there.

## [1.74.0] - 2026-09-30

### Fixed

- Package selectors inside `//@use` expressions now resolve through the full
  import cascade (file imports → sibling files → `[decorators.imports]` → the
  module graph) instead of only the annotated file's imports — previously a
  selector imported nowhere in the file was silently skipped and the generated
  file failed to compile with `undefined: pkg`; a blank import was the
  workaround. Identifiers the cascade cannot place (package-level values used
  in the expression) are still left alone.
- The module-graph layer now prefers the main module's own packages over
  dependency packages sharing the same name: `utils` resolves to the project's
  `utils` even when several dependencies ship one (three packages named `utils`
  is normal in a real build graph). Only a tie inside the module, or between
  foreign packages with no local candidate, still reports ambiguity.

## [1.73.0] - 2026-09-30

### Added

- **Annotated actions on Go-declared controllers.** Annotate a type's methods
  and leave `//@controller` off the type. A `nexus.Controller[*T]` or
  `nexus.Resource[*T]` declared in Go then serves those actions, and the
  module, `nexus.Path` and gates stay in code. For example,
  `nexus.Module("admin", nexus.Path("/admin"), nexus.Resource[*T]("/"))` needs
  no package-level `//@path`.
  - The generator emits one `nexus.ControllerActions(...)` per type, with
    paths as written. You can call it by hand too.
  - When no Go declaration exists, the actions register on their own, under
    the package's module.
- **`inertia.AsPage()`.** It renders a controller action as the page
  `<Folder>/<Method>`, the Go form of a `//@page` without a component. It's
  built on the new `nexus.ActionOption`, a REST option resolved against the
  action it's given to.
- `di.Defer` resolves an option when the container collects the option tree,
  not when it's constructed.

### Changed

- A `Resource` with none of the conventional methods no longer fails at
  construction. It fails at boot only if it ends up with no actions at all,
  so annotated or custom actions are enough.
- `nexus.Resource[T]("/")` is the same as `("")`: the module's path itself.
- Mounting the same router twice in one app is now a boot error.

## [1.72.0] - 2026-09-29

### Added

- **Shared job drivers.** A `jobs.Store` interface sits behind the jobs
  runtime, and two new drivers let several processes share one queue:
  - `jobsdb.Bind[DB]()` (`extension/jobs/jobsdb`): any GORM database, with
    version-checked atomic writes and tables created on first use.
  - `jobsredis.Bind(jobsredis.Config{})`: a new separate module,
    `extension/jobs/jobsredis`, using Lua claims and optimistic
    transactions.
- **Leases on shared stores.** A claimed job is leased, and the lease is
  renewed while the job runs.
  - A dead process's jobs are taken over once their leases lapse.
  - Every write checks that the attempt still owns the job
    (`jobs.ErrLostOwnership`).
  - Cancelling reaches a job running in another process.
  - New settings: `[jobs] lease`, `poll`, `driver = "db" | "redis"`.
- **RabbitMQ driver.** `jobsamqp.Bind(jobsamqp.Config{})` is a new separate
  module, `extension/jobs/jobsamqp`, implementing the new `jobs.Broker`
  interface.
  - Jobs are persistent messages in quorum queues, with a `.failed`
    dead-letter queue and TTL delay queues.
  - Publishes wait for broker confirms, and a job is acknowledged only after
    it finishes.
  - Queues get `x-consumer-timeout` (default 8h) so jobs can run for hours,
    and a delivery limit ends crash loops.
  - Shutdown republishes interrupted jobs with their checkpoints.
  - The broker keeps no per-job state, so `Manager.Get`, `Cancel` and `List`
    return `jobs.ErrUnsupported`.
- **Scheduled jobs.** `Job.Schedule(spec, args)` enqueues on a cron
  schedule (`0 7 * * *`, `@every 15m`, `CRON_TZ=…`). Each tick is enqueued
  once across replicas.

### Changed

- **`jobs.Manager` methods take a context and return an error.** `Get`,
  `Cancel` and `List` now read a store that may be a database.
- **Each app binds a job's receiver separately.** Previously a job's DI
  receiver was bound on its process-wide definition, so a second app in the
  same process rebound it.

### Fixed

- **Failing providers of optional dependencies are reported.** A dependency
  tagged `optional:"true"` whose provider exists but fails now fails the
  boot, as in dig/fx. The built-in container used to pass the zero value
  silently.

## [1.71.0] - 2026-09-29

### Added

- **Background jobs (`extension/jobs`), phase one.** A job is a method whose
  receiver comes from DI and whose arguments are a JSON struct.
  - Define it with `jobs.Define((*Svc).M, opts…)` or `//@job`, then enqueue
    it through the returned handle or with `jobs.Enqueue(ctx, (*Svc).M,
    args)`.
  - Options: queues with their own worker counts, timeouts, retries with
    backoff, `jobs.Permanent` errors, `Unique` enqueues, and `Delay`/`At`.
  - `*jobs.Run` reports progress, stores a result and checkpoints state for
    the next attempt.
  - `*jobs.Manager` gets, cancels and lists jobs.
  - On shutdown, running jobs get a grace period, then are cancelled and
    requeued.
  - The memory driver keeps jobs in the process and carries them across
    `nexus dev` rebuilds.
  - A `jobs` queue resource shows live counts on the dashboard.
- **`nexus.RequestIdentity(ctx)`** exposes the authenticated subject the
  registered identity sources report. Jobs use it to record who enqueued
  them.
- **Per-cache Redis switch.** `cache.Config.Driver` (the `driver` key in
  `[cache.<name>]`, `CACHE_DRIVER` or `CACHE_<NAME>_DRIVER`) picks the store
  for one cache:
  - `memory` keeps it off Redis even in production with the Redis backend
    imported.
  - `redis` uses Redis in every environment.
  - `auto`, the default, is the existing environment rule.

  An unknown driver, or `redis` without the backend import, fails the boot.
  The dashboard shows the effective driver.

## [1.70.0] - 2026-09-29

### Added

- **Controllers in decorator form.** `//@controller <prefix> [trailing-slash]`
  on a type turns its annotated methods into one `nexus.Controller[*T]`
  chain. `//@auth`, `//@session` and `//@use` on the type are shared by every
  action.
  - Methods take `//@page`, `//@rest`, `//@query` and `//@mutation`.
  - Paths are relative to the prefix.
  - An action may carry several routes.
- **`//@page <METHOD> <PATH> [Component]`.** An Inertia page annotation. On a
  plain function it becomes `inertia.Page`. On a controller method it becomes
  an action rendered through `inertia.Component`, and the component defaults to
  `<Folder>/<Method>`.
- **`inertia.Component(name)`** renders any REST action as that Inertia page.
  It works on plain controllers, and on an `inertia.Resource` it overrides the
  conventional component.
- **`ControllerRouter.TrailingSlash()`** registers every action at `/p` and at
  `/p/`, for apps whose links use both forms.
- **`nexus.RestOptions(...)`** bundles several REST options into one.

### Changed

- **A controller's GraphQL actions stay on the enclosing endpoint.** They now
  serve on the enclosing module's GraphQL endpoint (or the app's `/graphql`),
  not on `<prefix>/graphql`, so a controller's prefix is REST-only. Plain
  `nexus.Router`s keep mounting GraphQL at `<prefix>/graphql`.
- **`ActionDefaults` calls add up.** Each function's options apply after the
  previous one's. Previously the last call replaced the others, so defaults set
  on an `inertia.Resource` replaced its page rendering.

### Fixed

- **Routers and controllers inside a module mount under its `Path`.**
  `nexus.Module("x", nexus.Path("/x"), ctrl)` used to register the
  controller's routes at the root.
- **Annotated methods generate valid code.** They now emit the method
  expression `(*T).M`. Previously they emitted the bare method name, which did
  not compile.

## [1.69.0] - 2026-09-29

### Added

- **Controllers.** `nexus.Controller[T](prefix, gates...)` is a router bound to
  one controller type: a struct whose methods are the actions, constructed once
  through DI and shown as one dashboard module (`UsersController` → `users`).
  Actions are method expressions (`.Get("/:id", (*UsersController).Show)`), and
  their bare scalar parameters bind from the route's path parameters by
  position, with no `nexus.Arg`. `nexus.Resource[T](prefix)` registers the
  conventional actions the controller defines (Index, Show, Create, Update on
  PUT and PATCH, Destroy), plus `Member` and `Collection` for custom ones;
  nested prefixes bind every parameter.
- **`ActionAuthorizer`.** A controller with `Authorize(ctx, action string)
  error` has it run before each action; the error ends the request through the
  action's normal error path.
- **`nexus.ErrForbidden`**, mapped to 403 by `MapCRUDError`.
- **`nexus.Arg` body mode.** A handler's trailing struct is now accepted as the
  request body, with the named scalars before it:
  `Update(ctx, id int64, in UserInput)` + `Arg("id")`. The body's fields merge
  into the arguments, and on REST a name that is a route segment binds from the
  path only, so a JSON body cannot override it.
- **Custom controller actions with defaults.**
  `ControllerRouter.ActionDefaults(fn)` sets the options every REST action on
  a controller starts from, chosen by verb, path and method name. Explicit
  options still win, and `nexus.NoActionDefaults()` exempts one action.
- **Inertia resources.** `inertia.Resource[T](prefix)` is a controller whose
  actions are pages and forms:
  - `Index`, `New`, `Show` and `Edit` render `<Folder>/<Method>`.
  - `Create`, `Update` (on PUT and PATCH) and `Destroy` answer with a 303 to the
    new record's page, the record or the list.
  - Custom actions follow suit: a GET is a page, and any other verb redirects
    back.
  - `nexus.Errors` from a write goes back to the form.
  - `inertia.ResourceAs[T](folder, prefix)` names the component folder.
- **`pageAction` in `nexus-client/pages`.** `pageAction('Articles/Update',
  { id })` returns the action's `[method, url]`, ready for `form.submit(...)`
  or `router.visit`. It is typed by `NexusPageActions`, and resource write
  routes (`registry.PageActionTag`) no longer appear among the REST SDK's
  endpoints.
- **`nexus.EmptyRenderer`.** A renderer can answer for handlers that return
  only an error.

### Fixed

- **Map props in Inertia.** A handler returning a string-keyed map as its
  props now renders them. Previously they were dropped.
- **CSRF with Inertia forms.** The CSRF middleware mirrors its token into an
  `XSRF-TOKEN` cookie and accepts `X-XSRF-TOKEN`, the pair axios uses on its
  own. Scaffolded Inertia apps enable CSRF, so their forms were refused with
  403 until now.

## [1.68.0] - 2026-09-29

### Added

- **Inertia error pages.** `inertia.Config{ErrorPage: "Error"}` names a
  component to render when a page handler returns an error nothing else
  claims, or when a `Defer`/`Optional` prop fails to resolve. Previously
  those fell through to the plain `{"error": …}` JSON response, which the
  Inertia client shows as an "invalid response" modal. Page visits (GET)
  now get the error page with `inertia.ErrorProps{Status, Message}` and the
  error's status (404/409/400 for the CRUD sentinels, else 500), shared
  props included; form submits redirect back with the message under
  `errors._global`, which `useForm` reads. Redirects and validation errors
  are unchanged, and the error is still recorded on the request trace. Off
  by default.

## [1.67.0] - 2026-09-29

### Added

- **Typed page URLs for Inertia apps.** `import { pageUrl } from
  'nexus-client/pages'` builds a page's URL from its Go route, keyed by the
  Inertia component (`pageUrl('Users/Show', { id, tab })` →
  `/users/42?tab=…`). Path parameters are required, the handler's
  `query:`-tagged arguments are optional query parameters, and anything else
  is a type error, so a renamed component, a missing id or a misspelled
  parameter fails to compile and a changed Go path updates every link. When
  several routes render a component, the most specific one whose path
  parameters are given wins; `{ route }` picks one and `{ query }` adds
  parameters the handler reads without declaring. Generated as
  `web/sdk/pages.js` + `pages.d.ts` by the development dump and `nexus client
  --out`, and served at `/__nexus/client/pages.js`; nexus-vite-plugin aliases
  `nexus-client/pages` and the tsconfig merge maps it.
- The SDK manifest's field schema records each field's URL binding name
  (`path` for `path:`/`uri:` tags, `query` for `query:`/`form:` tags).

### Fixed

- An ineffectual assignment in the `//@router` parser that failed the
  golangci-lint gate.

## [1.66.2] - 2026-09-29

### Changed

- **`nexus dev` rebuilds are ~30% faster.** The default dev build now
  compiles OPTIMIZED: the old default paired `-gcflags=all=-N -l` with the
  DWARF-stripping `-ldflags=-w -s` — a binary too stripped for delve that
  still paid deoptimized-size link times (and forced a private build cache).
  Optimized objects shrink the binary (~14% on a large app), which shrinks
  the link — the step that dominates every rebuild — and the cache is now
  shared with your own `go build`/`go test` runs. `--debug` flips the whole
  trade at once: DWARF + symtab + `-N -l`, for delve. Dev builds also pass
  `-buildvcs=false` (no per-build git stamping), and the per-rebuild
  annotation scan is incremental (deco v0.19.0 ScanCache: only changed files
  re-parse). Measured on a ~100MB app: 0.60s → 0.40s per warm rebuild.

### Added

- `nexus dev --time-build` prints a per-rebuild breakdown
  (`⏱ codegen 42ms · build 380ms · prewarm 210ms`) so a slow rebuild can be
  diagnosed instead of guessed at.

## [1.66.1] - 2026-09-29

### Changed

- `nexus dev`: the status strip's entries are self-contained. A missing page
  pins the file to create (`⚠ page 'Users/Index' missing — create
  src/Pages/Users/Index.*`) or the fix for an invalid name, several list
  their names — instead of pointing back into the scroll; the highlighted
  `[web] ⚠` line drops the redundant `[nexus]` prefix; and a resource's
  attempt count only shows once an outage has a history (no `(1×)`).

## [1.66.0] - 2026-09-29

### Added

- `//@router <prefix>` (no name) declares a PACKAGE-NAMED router — the same
  default `//@module` uses (package name, `main` → `app`) — and every op in
  the declaring package joins it automatically, no `//@on` needed
  (`//@on <other>` still wins). It subsumes `//@module`/`//@path` for that
  package; mixing them is a positioned error. The explicit
  `//@router <name> <prefix>` form is unchanged.

## [1.65.1] - 2026-09-29

### Added

- **Resource logs are state transitions, not retry spam.** A database or
  redis outage now logs the moment it goes down (with the fix hint), then
  widening still-down heartbeats (10s → 1m → 5m; a NEW error kind always
  breaks through), then one recovery line with the outage's shape
  (`db: reconnected … attempts=47 down_for=2m10s`) — instead of one
  identical line per retry tick. Backed by `internal/logx.Transition`,
  adopted by `db.Manager` (connect, ping-lost) and the redis cache
  supervisor (connect, health check); every line carries structured
  `resource`/`state`/`attempts`/`down_for` fields.
- **`nexus dev` status strip.** Abnormal state stays pinned to the bottom of
  the console while logs scroll above (the buildkit/npm pattern): down
  resources (`✖ db:main: down 2m0s (24×)`, from the fields above) and
  unresolved Inertia page components (`⚠ 2 page components unresolved`,
  retired by the plugin's new `[nexus] pages ok` all-clear once the files
  exist). Renders nothing while everything is healthy; disabled off-tty,
  under `--raw-logs`, and with `NO_COLOR`, so piped output stays clean.

### Changed

- `nexus dev`: nexus-vite-plugin diagnostics in the `[web]` stream — a missing
  page component, a broken `[env]` key, a hot-file problem — now render on a
  highlighted `[web] ⚠` line in amber instead of ordinary cyan passthrough,
  so they stand out when the console is busy. The plugin's informational
  lines are unchanged.

## [1.65.0] - 2026-09-29

### Added

- **FastAPI-style routers.** `nexus.Router` is a first-class registration
  group: `NewRouter(name, prefix, shared...)` carries a URL prefix and shared
  per-op options (any `MiddlewareOption` — auth/session gates, `nexus.Use`),
  collects `.Rest/.Query/.Mutation/.Subscription/.WS/.Worker/.Provide`
  registrations, and nests through `Include` — prefixes stack, shared options
  inherit downward and run ahead of each op's own. A `*Router` is an
  `Option`: pass only the root to `Boot`/`Run` (an included router, a double
  mount, or an `Include` cycle is a boot error). Each router is its own
  dashboard module.
- **Decorator form: `//@router` and `//@on`.** A package doc comment declares
  routers — `//@router billing /billing parent=v1 auth=Requires(ADMIN)` —
  and `//@on billing` beside any primary registers that op there instead of
  the package module, from any package. Generated code records through the
  new `RouterDecl`/`OnRouter` options; a deferred source assembles the tree
  after every package init. Scan-wide validation with `file:line` errors:
  conflicting re-declarations name both positions, unknown parents and
  parent cycles, unknown `//@on` names with a did-you-mean over the declared
  routers. Ops without `//@on` stay on their package module, unchanged.

## [1.64.0] - 2026-09-28

### Added

- **Package-level decorators: `//@module`, `//@path`, `//@routeprefix`.** On
  the package doc comment, `//@module <name>` names the generated
  `nexus.Module` group (default stays the package name), and
  `//@path <prefix>` / `//@routeprefix <prefix>` emit `nexus.Path` /
  `nexus.RoutePrefix` as the module's leading options. Scope is enforced both
  ways with `file:line` errors (a package directive on a function, or a
  function directive / custom decorator on the package doc); conflicting
  values across a package's files name both locations, agreeing duplicates
  dedupe; prefixes must start with `/`. The keywords join the scanner's
  did-you-mean list. Rides deco v0.18.0 (package-level `Scan` hits).

## [1.63.0] - 2026-09-28

### Added

- **`session.Required()` and the `//@session` decorator.** The session
  extension gains its per-op surface: `Session.Established()` reports whether
  the request arrived with a live session (valid cookie, unexpired store
  entry), and `session.Required()` is a cross-transport per-op gate that
  rejects unestablished requests with 428 Precondition Required — flow
  continuity for the later steps of a multi-step form or checkout, distinct
  from authentication (`auth.Required`). Fails closed without
  `session.Module`. Decorator form `//@session Required` follows the
  normalized grammar (bare or call form, case-insensitive capability,
  `file:line` errors with a did-you-mean); the generated file imports
  `extension/session` only when used.

## [1.62.0] - 2026-09-28

### Added

- **Seamless `//@auth` decorator grammar.** Bare tokens, case-insensitive
  capability, unquoted permissions: `//@auth Required`,
  `//@auth Requires ADMIN HR` (→ `auth.Requires("ADMIN", "HR")`), and the new
  `//@auth Public` (→ `nexus.Public()`, the deny-by-default opt-out, added
  without pulling in the auth import). The legacy call form
  (`//@auth Requires("A", "B")`) keeps working, parse-checked as before.
  Mistakes fail at the annotation with `file:line`: a missing capability,
  `Requires` with no permissions (previously a vacuously-passing gate),
  `Required`/`Public` with stray arguments, and unknown capabilities with a
  did-you-mean suggestion.

## [1.61.0] - 2026-09-28

### Added

- **Seamless `//@inertia.Page` decorator.** Annotations read naturally — bare
  tokens instead of hand-quoted Go strings
  (`//@inertia.Page GET /users Users/Index`,
  `//@inertia.Page get,post /login Login`). The codegen gains a
  known-decorator normalization seam keyed on the resolved import path
  (aliased imports included): tokens are auto-quoted, verbs case-normalised
  with the comma multi-verb form kept, and a wrong arg count, non-HTTP verb,
  or path without `/` fails at the annotation with `file:line` instead of
  emitting invalid Go into the generated file. Quoted tokens still work, and
  `.Page` decorators from other packages keep the verbatim contract.
- **`inertia.Page` validates at boot.** An invalid verb, a path not starting
  with `/`, an empty component, or a nil handler returns `nexus.Error`, so a
  bad direct call fails the boot with a message naming the page instead of a
  route that never matches or a client-side "component not found".

## [1.60.5] - 2026-09-28

### Changed

- CLI: the annotation scanner moves to deco v0.17.1 (its `deco version`
  self-report and single-file guard; no behaviour change for nexus's own
  scanning).

## [1.60.4] - 2026-09-28

### Added

- **`extension/proxy`: `Config.UpstreamHost`** for upstreams behind name-based
  virtual hosting (shared hosting, cPanel/ISPConfig, a CDN). The upstream sees
  its own host as the `Host` header so the right site answers, and the
  response is pointed back at the proxy: an absolute `Location` redirect to
  the upstream origin becomes path-only, and a `Set-Cookie` `Domain` naming
  the upstream host is dropped so the browser keeps the cookie on the proxy's
  domain. The default (preserve the inbound host for `ALLOWED_HOSTS`/CSRF) is
  unchanged.
- **CLI: stricter, positioned `//@` decorator diagnostics.** Every codegen
  error now points at the annotation in `file:line` form. A typo'd keyword
  (`//@quer`, `//@Rest`) errors with a did-you-mean suggestion (conservative
  matching, so other tools' keywords stay ignored); `//@rest` validates the
  HTTP method (normalising case) and requires a `/`-prefixed path (ditto
  `//@ws`); `//@query`/`//@mutation`/`//@subscription`/`//@provide` reject
  stray arguments; `//@auth` and `//@use` expressions are parse-checked at the
  annotation instead of failing inside the generated file.

### Changed

- CLI: the annotation scanner moves to deco v0.17.0, and the `cmd/nexus`
  module's go directive follows it to 1.27.1.

## [1.60.3] - 2026-09-25

### Fixed

- **SMTP connection deadlines use the wall clock.** `extension/mail`'s SMTP
  mailer set the socket deadline from its injectable clock, which exists to
  stamp the message's `Date` header. A mailer built with a fixed clock (as
  the package's tests do) got a deadline in the past once that date had
  gone by, so every send timed out immediately.

### Security

- `github.com/rabbitmq/amqp091-go` → v1.13.0 (GO-2026-6372: a broker could
  send an oversized payload to exhaust memory) and `golang.org/x/text` →
  v0.39.0 (GO-2026-5970: infinite loop on invalid input). Both were
  reachable from this module.

### Changed

- **Documentation site.** The guides and reference now live at
  <https://paulmanoni.github.io/nexus/>, built from `docs/` with VitePress
  and published by a GitHub Pages workflow. The README is a short landing
  page that links into it, and `nexus docs --web` opens the site.
- CI: golangci-lint moves to v2.14.0 (v1 cannot read Go 1.26 export data),
  and the deprecated `reflect.Ptr` is replaced with `reflect.Pointer`
  throughout.

## [1.60.2] - 2026-09-25

### Fixed

- **WebSocket handlers see the connection's authentication.** A handler's
  context (`Params.Context`, `WSSession.Context`) started from
  `context.Background()`, so `auth.IdentityFrom`, `auth.User[T]` and
  `auth.Can` found no identity inside a WS handler — only
  `sess.UserID()` was set. Each connection now has a base context built
  from its upgrade request, and every message's handler derives from it;
  `extension/auth` carries its identity and state (which `Can` and
  permission backends read). The identity is the one the connection was
  opened with.

### Added

- `nexus.RegisterWSCarrier(func(upgrade, conn context.Context) context.Context)`
  — copies chosen values from a WebSocket upgrade request onto the
  connection's base context. Carry only values that may outlive the
  request; per-request state (a `Scoped` memo, a session handle) stays
  behind. `ws.Hub.OnContext` and `ws.Connection.Context` are the
  transport-level hooks underneath.

## [1.60.1] - 2026-09-25

### Security

- **A WebSocket connection's user is what the server authenticated.** The
  identify hook fell back to a `?userId=` query parameter, and nothing in
  nexus supplied the authenticated identity, so every connection's user id
  was the client's claim: `EmitToUser` delivered to whoever named the id.
  It now comes from the upgrade request's identity — `extension/auth`
  registers it through the new `nexus.RegisterRequestIdentity` — and the
  query fallback is gone. Apps without extension/auth register their own
  source, or set a `"user"` value with a `GetID()` method on the context.
- **The built-in `authenticate` message no longer sets a connection's
  user.** It reports the identity the upgrade established (or an error
  when there is none); a client sending `{"type":"authenticate",
  "userId":"…"}` used to receive that user's events.
- **A client can no longer join any room.** The built-in `subscribe`
  message joined whatever room the client named, so a client could listen
  to any audience the server addresses with `EmitToRoom` (another user's
  conversation, say). It is refused unless the path opts in with
  `nexus.ClientRooms(func(userID, room string) bool)`; server code joins
  rooms with `WSSession.JoinRoom` after checking the caller, as before.
  **Upgrading:** a frontend that sends `subscribe` itself needs
  `ClientRooms` on its `AsWS` (or a handler that joins the room).

### Fixed

- `nexus.AsWS` on a built-in message type (`ping`, `authenticate`,
  `subscribe`, `unsubscribe`) registered a handler that never ran; it is
  refused at boot.

## [1.60.0] - 2026-09-25

### Added

- **`nexus dev` and `nexus build` drive a real Vite project.** A frontend is
  a directory with a `package.json`. Both commands install its dependencies
  when `node_modules/.bin/vite` is missing (`npm ci` with a lockfile, else
  `npm install`; a clear error without npm), write
  `<web>/sdk/nexus-vite-plugin.{js,d.ts}` so a fresh checkout's
  `vite.config` loads, and run the project's own Vite. `nexus dev` learns
  the dev server's origin from the hot file, never Vite's stdout, and
  always prints (and with `--open` opens) the **app's** origin; Vite's
  `Local:`/`Network:` banner is hidden and its other output prefixed
  `[web]` (`--verbose` shows it all). `--tui` starts Vite too. `nexus build` runs `vite build`, then `vite build --ssr
  src/ssr.ts --outDir dist/ssr` when `src/ssr.ts` exists, requires
  `dist/.vite/manifest.json` or `dist/index.html`, then `go build`.
  `nexus dev --dist` rebuilds with `vite build` (and the SSR build) on its
  debounce.
- **The `[env]` bridge reaches real Vite.** `nexus dev`/`nexus build` pass
  nexus.toml's `[env]` table to Vite as `NEXUS_FRONTEND_ENV` (JSON of
  dotted keys), and `nexus-vite-plugin` replaces each exact member reference
  (`import.meta.env.client.id`) in frontend source with its value, in both
  `vite dev` and `vite build`. Only referenced keys reach the browser: the
  values are never added to the `import.meta.env` object, so whole-object
  access (`{...import.meta.env}`) and the bracket form see none of them. An
  `[env]` value whose `${VAR}` is unset where `nexus build` runs is dropped
  with a warning; the rest of nexus.toml's `${VAR}`s don't concern the
  frontend build. Under viteless the values reached only its own engine,
  never an installed Vite.
- **`NEXUS_ENVIRONMENT` is read** and overrides nexus.toml's `environment`,
  so a deployment that keeps the scaffold's `environment = "development"`
  says `NEXUS_ENVIRONMENT=production` without editing the file. It was
  documented as an environment source, but nothing read it. It also wins
  over a `Config.Environment` set in Go, and an app with environment
  overrides must declare the name it sets. SQL logging follows it too
  (`nexus.ActiveEnvironment`).
- **Vite scaffolds.** `nexus new --frontend vue|react` (with `--inertia`,
  `--ssr`) and `nexus init --frontend vue|react` write an npm-managed Vite
  project: `package.json` with ranges verified to install, type-check and
  build together (vite ^6.4.3, @vitejs/plugin-vue ^5.2.4 or
  @vitejs/plugin-react ^5.2.0 with React 19, typescript ~6.0.3, vue-tsc
  ^3.3.11 — vue-tsc 3.3 does not run on TypeScript 7), a `vite.config.ts`
  with `nexus()` and no proxy block, `web/sdk/nexus-vite-plugin.*`, a
  strict `tsconfig.json` without `baseUrl`, a typed Inertia page glob, and
  a `typecheck` script. The `.gitignore` keeps `web/sdk` (commit it) and
  the next steps say `nexus dev` installs the dependencies and prints the
  app's URL. `nexus init --force` adds the project files to an existing
  `web/` while keeping `index.html` and `src/`, which is how a viteless-era
  directory moves to Vite; a replaced `package.json`, `vite.config.ts` or
  `tsconfig.json` that differs is kept as `<file>.orig`. `nexus init`
  also adds the frontend's `.gitignore` entries.
- **Dependencies install with the project's own package manager**:
  `packageManager` in package.json, else the lockfile (npm, pnpm, yarn,
  bun), frozen when a lockfile exists; an interrupted install is retried.
  Yarn Plug'n'Play is refused with the setting to change.
- **Vite handshake: the plugin tells, the app reads.** `nexus-vite-plugin`
  writes `<outDir>/.vite/nexus-hot.json` — the dev server's real origin,
  base, entries and pid — once `vite dev` is listening, and removes it on
  shutdown. `ServeFrontend` and the Inertia engine read it per request, so
  pages load modules from wherever Vite actually bound (including when 5173
  is taken), a Vite restart on a new port applies with no Go restart, and
  `npm run dev` + `go run .` with `environment = "development"` is a complete
  dev setup without `nexus dev`. The file is read from disk only, never
  served, and followed only while the dev server it names is live (running
  pid, or an origin that answers); one left by a killed dev server reads as
  absent and is logged once. The
  origin is the socket's bound address, not `localhost`: two dev servers can
  hold the same port on 127.0.0.1 and ::1, and `localhost` reaches either.
  `NEXUS_VITE_DEV` remains as a fallback.
- **Inertia pages render into `index.html`.** The engine used to synthesise its
  own document, so everything an app put in `index.html` — title, meta,
  stylesheets, a loader — was missing on server-rendered pages. It now puts the
  page object on the mount element of the real document (Vite's transformed
  page in dev, the built page in production) and adds no asset tags of its own.
  A module-only build still gets a synthesised document, with tags under the
  path the bundle is actually served at (`App.FrontendMount`), not `/`.
- **Dev reload that doesn't fight HMR.** Under `nexus dev` a `.vue` save was
  applied in place by Vite and then thrown away by a full reload ~130ms later.
  The reload shim now reloads only when a new server process is serving (a
  boot ID), or when the dev server starts, stops or moves to another port —
  never for files Vite handles. SPA pages carry the shim too.
- **`public/` files in development** are proxied from the app's origin to the
  Vite dev server (loopback clients naming a loopback host, not through a
  proxy or tunnel; Vite's own routes are never forwarded), so runtime paths like
  `fetch('/config.json')` and template `<img src="/logo.png">` stay live.
- **`nexus({ input })`** declares an Inertia app's entry module once; the
  plugin also forces `build.manifest: true` and sets `server.origin` so CSS
  `url()` and asset imports resolve against the dev server when the page is
  served from the app's origin.
- **Asset caching from the build, not from path conventions.** A file is
  `immutable` only when the Vite manifest lists it as build output *and* its
  name carries a content hash; everything else is revalidated with an ETag.
  Embedded files had no modification time, so non-hashed files could never
  be answered with a 304 before.
- **`nexus.toml` keys nothing reads are reported** at boot and by
  `nexus lint`, with a hint naming the right table (`did you mean
  [runtime.server] addr?`) or listing what the table accepts.
- `db.Config.Address()` — host:port safe to log, unlike `DSN()`.
- `nexus --version`; command groups in `nexus --help`; a `--help` pointer on
  command-line errors.
- **Typed Inertia pages and shared props.** `inertia.Page` tags its route with
  the component (`registry.PageTag`), and the client SDK emits
  `NexusPageProps` (component → the handler's props type; a union when one
  component has several props types) and `NexusSharedProps` in `client.d.ts`.
  A page reads its props with `defineProps<NexusPageProps['Users/Index']>()`.
  `inertia.ShareScoped` now records its type, and `inertia.ShareTyped[T]` is
  the typed sibling of `Share`; untyped `Share` keys ride an index signature.
  Typed shares are optional (`can?:`): the engine omits a key whose compute
  fails, so the type does not promise what the page may lack.
  A generated `inertia.d.ts` (referenced from `client.d.ts`, served at
  `/__nexus/client/inertia.d.ts`, written by the dump and `nexus client`)
  types `usePage().props` through `@inertiajs/core`'s `InertiaConfig`. The
  manifest gains `endpoints[].page` and `sharedProps` (additive, `client.v1`).
  `inertia.Prop` fields are typed as optional `unknown` instead of an empty
  `Prop` interface (`registry.SchemaOpaque`).
- **`import type { NexusPageProps } from 'nexus-client'` resolves.** The
  tsconfig merge maps `nexus-client` to the SDK's `client.d.ts` (the docs
  promised the name; nothing mapped it), and `nexus-vite-plugin` aliases it to
  `client.js` for Vite (a project's own mapping or alias for the name wins).
  When the config lists `include`, the SDK's `client.d.ts` joins it, so the
  `inertia.d.ts` augmentation applies even in a component that only calls
  `usePage()`. A solution-style root (`files: []` + `references`, the
  create-vue / create-vite layout) is left alone and the referenced config
  covering `src/` gets the mapping. Every dev mount wires it, including the
  implicit `nexus dev` one; `Client.TSConfig = client.Off` opts out.
- **`nexus({ pages })` checks page components exist.** Every component the
  manifest names (default dir `src/Pages`) must have a file, matched
  case-exactly as `import.meta.glob` keys are: `vite dev` warns once per
  missing one, `vite build` fails listing them — a typo in `inertia.Page` is
  a build error instead of a blank NotFound render. `pages: false` turns it off.
- **`nexus.Tag(key, value)`** — the exported cross-transport option for
  stamping an endpoint's registry tags, for extensions that mark the
  endpoints they register. Keys this package's options own (`auth.public`,
  `auth.flow`, `auth.requires`, `dashboard.*`, `nexus.envelope`) panic at
  registration, naming the option to use. `App.RegisterSharedProp(key, reflect.Type)` records
  a typed page-wide shared prop.

### Changed

- **Breaking: Vite is the only frontend engine; viteless is removed.**
  `nexus dev` and `nexus build` no longer embed a zero-Node engine, fetch
  dependencies from a CDN, or serve the SPA on :5173 behind a proxy.
  **Node.js 20+ and npm are required to develop and build a frontend**;
  the built binary still embeds `web/dist` and runs without Node. A
  viteless-era `web/` (`viteless.config.*` or `viteless-env.d.ts`, no
  `package.json`) gets a migration hint — a warning in `nexus dev`, an
  error in `nexus build`; `nexus init --frontend vue --force` writes the
  Vite files and keeps the sources.
- **`nexus dev` no longer sets `NEXUS_VITE_DEV`**: the hot file carries
  the dev server's origin, and Inertia's SSR-over-HTTP renderer reads it
  from `App.ViteHot()` too (the variable stays a fallback).
- **No Inertia mode in `nexus dev`.** It no longer scans `go list -deps`
  for the inertia extension or mutes Vite's output for Inertia apps: every
  app opens on its own origin, so `[runtime.inertia] enabled` in
  nexus.toml has nothing left to switch and is ignored.
- The SSR scaffold's `ssr.ts` imports `createServer` from
  `@inertiajs/vue3/server` (it depended on `@inertiajs/server`, whose
  published versions stop at 0.1.0) and bundles its dependencies
  (`ssr.noExternal`), so `node web/dist/ssr/ssr.js` runs without
  `node_modules`. Inertia scaffolds pass an empty `inertia.Config`: the
  engine finds the bundle through `ServeFrontend`.
- **Breaking (behaviour): production binaries no longer write `web/sdk`.**
  The boot-time client SDK dump ran in every mode, so a production binary
  with the SDK enabled, started in a directory holding a
  `web/vite.config.ts`, wrote `./web/sdk` (and edited `web/tsconfig.json`)
  on every boot. It now runs only under `nexus dev` or with
  `environment = "development"` — the rule the Vite hot file follows — and
  a production binary writes nothing, silently. Vendor the files at build
  time with `nexus client --out`.
- **One SDK location: `web/sdk`.** `nexus-vite-plugin`'s `sdkDir` defaults
  to `sdk` (the Go app's dump target) instead of `src/sdk`; a project with only
  `src/sdk/manifest.json` keeps reading it. The tsconfig merge no longer adds
  `baseUrl` (TypeScript 6 rejects it as deprecated); an existing one is
  honoured, and mapped paths are now computed relative to it.
- **An explicit "no SDK dump" is honoured.** The frontend-dir detection
  filled any empty `client.Config.OutDir`, so "no dump" (`frontend.Plugin`
  with `RuntimeSDK: false`) became a dump into `web/sdk`. New
  `client.Off` on `OutDir` / `TSConfig` / `ViteConfig` means "none, don't
  detect one"; the detection fills only unset fields, and `frontend.Plugin`
  maps an empty `SDKOutDir` to `client.Off`.
- The `nexus dev` SDK auto-mount now dumps into `web/sdk` on purpose (the
  page-props types and the manifest `nexus-vite-plugin` reads) and merges the
  `nexus-client` mapping into tsconfig (Vue's compiler resolves page types only
  through it); `Client.TSConfig = client.Off` keeps tsconfig untouched,
  `Client.OutDir = client.Off` keeps the routes without the files, and
  `Client.DevDisabled` still closes both.
- **Inertia pages are no longer emitted as REST calls** in the SDK's
  `RestEndpoints` or `extension/frontend`'s `index.ts`: a page is rendered,
  not called. Its props type lives in `NexusPageProps`.
- **A missing frontend build is loud.** An Inertia page with no dev server
  and no manifest renders an error page naming both paths in development
  (500), and logs once in production — previously both shipped a blank page.
- **`ServeFrontend` boots a build with no `index.html`** when a Vite manifest
  proves it built (a module-only Inertia build); unknown routes then 404. A
  bundle with neither still fails fast in production, and serves a
  placeholder in development.
- A hot file whose dev server has exited reads as absent (after checking its
  pid, then whether its origin answers) instead of turning every page into an
  error; a build into the same outDir restores a live dev server's hot file.
  Boot leniency for an unbuilt bundle now needs `nexus dev` or a live dev
  server — `environment = "development"`, which scaffolds ship, no longer
  skips the production fail-fast.
- `/.vite/` (the manifest and the hot file) is never served, and the bundle is
  never directory-listed.
- The Vite plugin's CORS allowlist covers loopback, `*.localhost`, `*.test`,
  this machine's addresses and `nexus({ appOrigin })`; a `--host` bind writes a
  network-reachable origin, so LAN and mobile testing work.
- The production `index.html` is `Cache-Control: no-cache` with an ETag
  (304 when unchanged) instead of `no-store`.
- **Dependency outages are reported once.** A database or Redis that is
  down logs one line per resource — name, address, and a `fix` — instead of
  a line per retry attempt every few seconds; retry attempts are Debug,
  Redis falling back to memory is a Warn, and GORM's own logger goes through
  the app's zap logger instead of stdout.
- **Wiring errors name the consumer** (`needed by main.NewHandler
  (handler.go:24)`), diagnose pointer/value mismatches, name both colliding
  constructors on a duplicate provider, and render as a structured block
  with a `fix` line; the fx backend no longer exits 1 with empty stderr.
  A port already in use names the port and the two ways out.
- Output flags are one spelling: `--out`/`-o` everywhere (`--output` still
  parses). `--tsconfig` is a real alias of `--jsconfig`.
- The SDK auto-dump no longer prints a line per unchanged file on every
  restart.
- `nexus dev` opens the app's own URL once a Vite hot file exists.

### Deprecated

- `nexus new --tooling` (Vite is the only engine) and `nexus dev
  --frontend-cmd` (already ignored): both still parse, warn, and are
  ignored.
- The in-process codegen driver: `extension.Plugin.Generate`,
  `extension.Generate`, `nexus.GenerateDriver`, `App.RegisterGenerateDriver`,
  `App.GenerateDrivers`. Nothing ever read a registered driver back —
  frontend codegen runs in the CLI — so `extension.Use` no longer registers
  one and `frontend.Plugin` no longer declares it. A set `Generate` is still
  validated and flags the plugin on the dashboard; a second one no longer
  panics.
- `nexus build --package` and `nexus init --dir` (pass the positional
  argument), `nexus dev --fast` (it is the default; `--debug` is the
  inverse), `nexus pki --dns`/`--ip` (now `--dns-name`/`--ip-address`).

### Removed

- The viteless dependency of `cmd/nexus`, the viteless scaffold files
  (`viteless.config.ts`, `viteless-env.d.ts`), the vestigial second embed
  `nexus build` generated (`embed_gen.go`), and the dead `islands.src`
  layer, `.env` loader and `package.json` editor in the CLI.
- `nexus generate dockerfile` from the docs — the command never existed.
- `--json-in` on `lint`, `doctor` and `routes` (JSON is the default input).
- `nexus routes --deployment` and the `DEPLOYMENT` column — they filtered a
  field no real app populates since `DeployAs` was removed.

### Fixed

- Closing the terminal (SIGHUP) left `nexus dev`'s Vite and app running;
  `nexus dev`, `nexus build` and the TUI now stop them on SIGHUP too, and a
  cancelled dependency install takes its children with it. On Windows the
  whole process tree is stopped.
- `nexus build` looked only at `web/` (or `NEXUS_FRONTEND_DIR`) while
  `nexus dev` also followed `ServeFrontend`'s directory, so a `frontend/`
  project built with a stale bundle; both resolve it the same way, and
  `nexus build --frontend` exists.
- `nexus build` never ran the Inertia SSR build: `dist/ssr/ssr.js` existed
  only if you ran `npm run build` yourself.
- `nexus dev` stopped Vite with SIGKILL, so the plugin could not remove its
  hot file; it now sends SIGTERM to Vite's process group, SIGKILL after 2s,
  and waits for the exit.
- A top-level value in nexus.toml's `[env]` table panicked at boot.
- `ServeFrontend` never serves `dist/ssr`: an embedded Inertia SSR bundle
  (`//go:embed all:web/dist`) is not a public file.
- `nexus dev <dir>` resolved the detected frontend dir against the working
  directory instead of the package (`nexus dev ./examples/inertia` from the
  repo root looked for `./web`), and ignored `NEXUS_FRONTEND_DIR`, which
  `nexus build` honoured. Precedence is now `--frontend` (cwd-relative) >
  `NEXUS_FRONTEND_DIR` (project-relative) > the `ServeFrontend` scan >
  `web/` with a `package.json`.
- The Vite scaffold's `vite.config.ts` proxied the nexus routes to a
  hard-coded `:8080` and did not load `nexus-vite-plugin`, so its pages had
  no hot file and its builds no forced manifest.
- The development placeholder page recommended `nexus add` and an islands
  pipeline, neither of which exists.
- `nexus routes|lint|doctor --binary` failed on every app: print mode wraps
  the manifest in markers the parser never stripped.
- `routes --kind http|graphql|websocket` and `--auth none` matched nothing
  and exited 0; filter values are now normalized and validated.
- The SDK dump found the frontend dir only by `vite.config.ts`; a
  `vite.config.mjs`/`.js`/`.mts`/`.cjs` project got no SDK and no tsconfig
  mapping.
- The Inertia scaffold's `main.ts` failed strict `vue-tsc` (TS2769): its page
  glob now names the module shape.
- The frontend scaffold's README told you to `npm install` a project with no
  `package.json`; `nexus dev` announced "ready" after the app had died.

### Release notes

- **Upgrading a frontend project.** Frontends are Vite projects now: Node 20+
  and a `package.json` in the frontend dir. A viteless-era `web/` gets a
  migration hint from `nexus dev`/`nexus build`; `nexus init --frontend vue
  --force` (or `react`) writes `package.json`, `vite.config.ts` and
  `tsconfig.json` beside your `index.html` and `src/`, keeping any file it
  replaces as `<file>.orig`.
- **Upgrade both halves.** `go install github.com/paulmanoni/nexus/cmd/nexus@v1.60.0`
  for the CLI and `go get github.com/paulmanoni/nexus@v1.60.0` in each app —
  the hot file, the index.html shell and typed pages are app-side.
- **Deployments** that ship the scaffold's `nexus.toml`
  (`environment = "development"`) set `NEXUS_ENVIRONMENT=production`.
- `cmd/nexus` and `extension/cache/redis` now use internal packages added in
  this release (`internal/vitehot`, `internal/logx`). Release the parent
  module first, then bump the submodules' requirement — the usual
  "submodules require parent" step — before they build outside go.work.

## [1.59.1] - 2026-09-18

### Fixed

- **`auth.OpGates` docstring** — the example predated
  ShareProvide/ShareScoped and showed a `inertia.Share("can", fn)`
  call that never existed (Share takes one provider) closing over an
  `app` that isn't available at declaration time. The doc now shows
  the canonical CanGates Scoped + `inertia.ShareScoped` wiring and
  states the key-space boundary explicitly: op names only,
  registration-stamped permissions only — a codename that gates no
  op is invisible to OpGates by construction (check holdings via
  auth.Can/Gates or an identity-derived Scoped instead).

## [1.59.0] - 2026-09-18

### Added

- **`inertia.ShareScoped(key, handle)` — one declaration serves
  handlers and pages.** The bridge between a request-scoped fact
  (`nexus.NewScoped`) and the frontend: handlers call
  `handle.Get(ctx)`, pages read `props.<key>`, and the Scoped memo
  guarantees one compute per request for both. The key is declared
  at registration; a failed derivation omits the key from the render
  (pages degrade) while handler-side Gets still surface the error.
  Permissions are the first instance —

      var CanGates = nexus.NewScoped[map[string]bool](
          func(app *nexus.App) nexus.Compute[map[string]bool] {
              return func(ctx context.Context) (map[string]bool, error) {
                  return auth.OpGates(ctx, app), nil
              }
          })
      nexus.Boot(CanGates, inertia.ShareScoped("can", CanGates), ...)

  — but any named request fact (features, quota, tenant state)
  rides identically.

## [1.58.0] - 2026-09-18

### Added

- **Client SDK: envelope-aware `nx.op`, same-tick query batching, op
  composables.** Ops registered with `nexus.Envelope` are marked in
  the manifest and the generated `GraphqlOps` map;
  `await nx.op('usersList', vars)` picks query/mutation from the
  manifest and unwraps `{status, message, data}` — resolving `data`
  (typed `Promise<GqlData<K>>`) and throwing `NexusOpError` with the
  envelope's message on `status:false` (`{unwrap:false}` for the raw
  envelope). Independent queries issued in the same microtask
  coalesce into ONE aliased GraphQL request per path, with per-alias
  errors rejecting only their own caller. Vue gains
  `useOpQuery(name, args)` (in-flight dedupe per op+args) and
  `useOpMutation(name, {refresh, latest, onSuccess(data, message)})`
  — `refresh` refetches every mounted `useOpQuery` of the named ops
  after success; `latest` is the auto-save race guard. Verified end
  to end from Node against a live app; emitted d.ts passes
  `tsc --strict`. `nexus docs clientops`.

## [1.57.0] - 2026-09-18

### Added

- **Scoped handles auto-provide into DI.** A handler can declare the
  request-scoped fact it reads as an ordinary dependency —
  `func NewUsersPage(ctx context.Context, scope *nexus.Scoped[Scope], ...)`
  — instead of touching the package-level handle. Providers are lazy,
  so nothing injects → nothing runs; the request path is unchanged.
  `*Scoped[A]` and `*Scoped[B]` are distinct DI slots; two handles of
  the SAME T in one app fail boot with the container's
  duplicate-provider error — mark extras `.NoProvide()`, or give each
  fact its own named type.

## [1.56.0] - 2026-09-18

### Added

- **`nexus.NewScoped` — request-scoped derived values.** A fact
  derived from the request (identity + DB, tenant state, a feature
  evaluation) computed at most once per request, on first ask, and
  shared by every handler, service and prop that asks after:

      var delegatedScope = nexus.NewScoped[Scope](
          func(svc *services.UserMgmtService) nexus.Compute[Scope] { ... })
      nexus.Boot(delegatedScope, ...)
      scope, err := delegatedScope.Get(ctx)

  Lazy (never asked → never computed; no Scoped registered → the
  store middleware is never installed), singleflight per request
  (parallel GraphQL resolvers share one compute), error memoized
  alongside the value. Per-request lifetime ONLY — by design no TTL,
  no cross-request cache, no invalidation: staleness-tolerant facts
  belong on the identity, longer-lived facts in extension/cache.
  Unit tests inject with `nexus.WithScopedValue(ctx, handle, v)`.
  Full-request overhead with one handle and one Get: noise against
  the bare-handler baseline. `nexus docs scoped`.

## [1.55.0] - 2026-09-18

### Performance

- **`nexus.Arg` ops call the method directly.** The v1.54 adapter
  invoked the original method through a `reflect.MakeFunc` trampoline
  — double reflection plus argument re-packing, measured at +18% /
  +5 allocs on a trivial REST op. The synthesized args struct now
  exists only for binding and schema; the shape inspector feeds the
  named scalar slots straight from the bound struct's fields into the
  original function, in the one reflective call every handler already
  pays. An Arg op now costs exactly what the hand-written wrapper
  did (same allocations, time within noise).

- **`nexus.Envelope` is generic — no reflect.Call per request.**
  `Envelope[T, W](wrap func(T, error) (W, error))` captures the wrap
  in a typed closure (one type assertion + a native call) instead of
  invoking it reflectively; `reflect.TypeFor` still supplies the
  schema types. Source-compatible — call sites passing a typed func
  infer T and W — and the wrap's shape check moves from boot to the
  compiler. Overhead over a bare handler: +295ns/+3 allocs before,
  ~+110ns/+1 alloc after (the wrap call and the envelope value it
  actually builds).

- **`*Form` machinery is gated on a precomputed shape flag** — ops
  that don't declare a `*Form` parameter no longer pay a per-request
  slot scan.

  Benchmarks live in perf_newfeatures_test.go so regressions on
  these paths show up in `go test -bench`.

## [1.54.0] - 2026-09-18

### Added

- **`nexus.Arg` — scalar parameters become named wire arguments, no
  wrapper structs.** A registration option that kills the one-line
  args-struct adapter a scalar-taking service method used to force:

      nexus.AsQuery((*UserService).GetUser, nexus.Arg("id"), nexus.Op("userShow"))
      nexus.AsRest("GET", "/users/:id", (*UserService).GetUser, nexus.Arg("id"))
      nexus.AsMutation((*UserService).Move, nexus.Arg("id", "employerId"))

  Names map positionally onto the handler's last len(names)
  parameters (Go reflection cannot see parameter names). The args
  struct is synthesized at registration — fields tagged
  json/query/uri/graphql — so binding, the GraphQL schema and the
  generated SDK see exactly what a hand-written wrapper declared;
  non-pointer parameters are required arguments, pointer parameters
  optional, and the op name still derives from the method. One or
  two scalars ride Arg well; three or more deserve a dto. Misuse is
  a boot error: struct parameters ("register it directly"),
  Params[T] handlers, arity or name mismatches.

## [1.53.0] - 2026-09-17

### Added

- **`*nexus.Form` — raw form input, source-unified.** Declare it as a
  handler parameter (framework-filled, like `*httpx.Ctx`) or reach it
  below the handler via `nexus.FormFrom(ctx)`:

      func NewUploadCv(svc *Svc, ctx context.Context, fm *nexus.Form) (any, error) {
          title := fm.Get("title")
          cv, err := fm.File("cv")   // streams; never fully buffered
      }

  `Get / Lookup / All / Int / Bool / File / Files / Value / Bind` read
  the same fields whether the client sent a JSON body (Inertia
  `useForm`'s default), multipart/form-data (what useForm switches to
  when a file is attached), urlencoded, or — lowest precedence — the
  URL query, so a working form doesn't break the day a file input is
  added. `Bind(&dto)` bridges back into the typed world. Typed dtos
  remain the primary shape (schema, SDK, validation tags, and maskid
  ride them — raw reads bypass maskid). REST/Inertia only; on
  GraphQL/WS the param is a typed nil whose methods no-op.
  `nexus docs forms`.

- **`nexus.Errors` — field + global validation errors, one type, per-
  transport rendering.** `Field("email", "taken")` accumulates;
  `Global("provider unreachable")` writes under the reserved
  `_global` key on the same object a form already watches. Returned
  as the handler's error: Inertia pages flash + 303 back into the
  `errors` prop (the useForm convention; `X-Inertia-Error-Bag`
  honored; `inertia.Invalid` shares the path), REST answers
  `422 {"message", "errors": {field: [msgs]}}` (a validation failure
  is a normal outcome, not a 500), GraphQL carries the field map in
  the error's extensions. Field keys should match the dto's json tags
  so `useForm` binds messages onto the right inputs.

## [1.52.0] - 2026-09-17

### Added

- **Op gates: permissions declared once, on the registration.**
  `auth.Requires("add_user")` now stamps its permission list onto the
  endpoint's registry entry, and `auth.OpGates(ctx, app)` evaluates
  every registered op's own declaration for the current identity —
  returning `map[opName]bool` keyed by the op names the frontend
  already calls. No permission string exists outside the
  registration, and no page hand-builds a parallel "can" table that
  can drift from the gates. App-wide wiring is one Inertia option via
  the new `inertia.ShareProvide` (a DI-built shared prop):

      inertia.ShareProvide(func(app *nexus.App) inertia.SharedProvider {
          return func(ctx context.Context) (string, any) {
              return "can", auth.OpGates(ctx, app)
          }
      })
      // SPA: v-if="can.saveUser"

  Ops without `Requires` are always true (it reports permission
  gates, not authentication); evaluation runs server-side through the
  configured `PermissionFn` / `Backend.Authorize`, so superuser
  bypasses and custom logic hold. Built for per-render use: the
  registry compiles once per registry version (new
  `registry.Version()` mutation counter) into a table grouped by
  unique permission set — one authorize call per set, not per op;
  ~4µs / 4 allocs for 200 ops.

## [1.51.0] - 2026-09-17

### Added

- **Service methods register as handlers directly — no wrapper.** A
  method (or free function) shaped `func(ctx context.Context, args T)
  (R, error)` is a valid handler as-is: the method expression's
  receiver becomes a DI-injected dep, the trailing struct binds like
  `Params[T].Args`, and the op name derives from the method name
  (`CreateUser` → `createUser`):

      nexus.AsMutation((*UserService).CreateUser, auth.Requires("add_user"))
      nexus.AsRest("POST", "/users", (*UserService).CreateUser)

  A bound method value (`svc.CreateUser`) works too and names the
  same op — the runtime's `-fm` wrapper suffix is now stripped in
  name extraction. This removes the one-line `NewXxx` delegation
  wrapper per endpoint; the registration list becomes the single
  declaration of op + route + permission. Use pointer receivers: a
  zero-arg value-receiver method expression would read the receiver
  struct itself as the args container.

- **`nexus.Envelope(wrap)` — per-op response envelopes.** Apps whose
  wire contract wraps every result (`{status, message, data}`-style)
  no longer convert in each handler. The wrap is the app's own
  `func(T, error) (W, error)`, instantiated per registration:

      nexus.AsQuery((*UserService).ListUsers, nexus.Envelope(Wrap[[]UserRow]))

  GraphQL declares W in the schema (introspection and the generated
  SDK describe the real contract); REST serializes W. An error the
  wrap converts into a value ships as a normal 200/data response; an
  error the wrap returns follows the standard error path. Binding and
  validation failures are never enveloped, and a wrap whose input
  type doesn't match the handler fails at boot naming both types.

- **`[runtime.server] strip_trailing_slash = true`** routes
  `"/users/"` as `"/users"` — an internal rewrite at the App
  boundary, not a redirect, so non-GET bodies survive and every
  router backend behaves identically. Off by default.

- **`auth.Can(ctx, perm)` / `auth.Gates(ctx, perms...)`** evaluate
  UI permission toggles through the same `PermissionFn` /
  `Backend.Authorize` the `Requires()` endpoint gate consults, so a
  page's "can" props share one rulebook with the gates. `Gates`
  returns `map[string]bool` with every requested permission present;
  anonymous is always false.

- **`inertia.AlwaysFunc(fn)`** — `Always` for a render-time
  computation: same inclusion rule (sent on every visit, partials
  included), but `func() (T, error)`, so the error propagates through
  the render instead of being swallowed by an immediately-invoked
  closure in the props literal.

## [1.50.0] - 2026-09-16

### Changed — BREAKING

- **SQL drivers are opt-in blank imports (database/sql style).**
  `nexus/db` linked all three engines unconditionally, so a Postgres
  app shipped the ~5MB pure-Go SQLite engine it never opened, and
  vice versa. `nexus/db` now links NO engine; import the one(s) your
  app actually opens:

      _ "github.com/paulmanoni/nexus/db/postgres"
      _ "github.com/paulmanoni/nexus/db/mysql"
      _ "github.com/paulmanoni/nexus/db/sqlite"

  A `Config` naming an unlinked driver fails at wiring time with the
  exact import line to add, so the migration is one line per driver
  and cannot fail silently. `nexus new --db` scaffolds emit the right
  import; custom GORM dialects can `db.RegisterDriver` their own name.

### Fixed

- **File-backed SQLite gets a real connection pool.** The old
  `MaxOpen=1` default applied to ALL SQLite, so WAL bought nothing
  and every query in a file-backed process queued behind one
  connection. Only `:memory:` keeps the single shared connection
  (its schema dies with the connection); file DSNs now get a small
  read pool — writes still serialize inside SQLite itself, so keep a
  `busy_timeout` pragma in the DSN, as the scaffolds do.

## [1.49.0] - 2026-09-15

### Performance

- **The trace event bus is sharded by trace ID.** With the dashboard
  enabled, every request published 2-3 events through one
  process-wide mutex — the last hot-path serialization point in the
  framework. The ring and fan-out now shard by TraceID (one trace,
  one shard, so per-trace ordering is preserved; the global monotonic
  event ID recovers cross-shard order in Subscribe backlogs), and
  each subscriber's delivery is fanned in through per-shard feed
  channels, so producers never contend with other shards' publishes
  racing toward the same consumer. All existing semantics hold and
  are race-tested: exactly-once reconnect via sinceID,
  drop-don't-block for slow consumers, safe cancellation. 10-way
  parallel publish: 222ns → 115ns with the dashboard enabled but no
  client connected, 361ns → 268ns with a live stream attached. Small
  buses stay single-shard (exact global ring order).

## [1.48.0] - 2026-09-15

### Performance

- **`nexus dev` builds strip the symbol table too (`-ldflags="-w -s"`).**
  Measured on a ~95MB app: steady rebuild 2.2s → 1.9s, and the smaller
  binary also cuts the macOS code-signing cost the dev loop pre-pays
  before each swap. Safe for panic tracebacks — the Go runtime
  symbolizes from pclntab, not the symtab — and `--debug` restores
  both symtab and DWARF for delve, as before. The change lives in the
  CLI: reinstall `cmd/nexus` to pick it up.

## [1.47.0] - 2026-09-15

### Added

- **`extension/session` — Django-style server-side sessions.** A
  cookie carries an opaque 256-bit ID, the data lives in a pluggable
  `Store`, and handlers use a lazy per-request handle — for anonymous
  visitors and logged-in users alike, on REST, Inertia, and GraphQL
  (`p.Context`):

      nexus.Boot(session.Module(session.Config{}))

      s := session.Get(p.Context)
      s.Set("cart", skus)   // first write mints the ID + sets the cookie
      s.Cycle()             // rotate the ID on login (fixation defense)
      s.Destroy()           // logout: delete + expire the cookie

  Semantics mirror Django's: lazy (no store hit until the handler
  touches it), save-only-if-modified (`Touch()` forces a TTL
  refresh), and the cookie is set on the first WRITE — anonymous
  requests that never touch the session get no Set-Cookie. Stores:
  the default `NewMemoryStore()` is bounded, swept, and survives
  `nexus dev` rebuilds via the dev-state machinery; production wants
  `session.CacheStore(nexus.Cache)` (Redis via extension/cache =
  restart-safe and multi-replica) or your own DB-backed `Store`.
  Cookies are always HttpOnly, SameSite defaults to Lax; set
  `Secure: true` behind TLS. `nexus docs session`.

## [1.46.0] - 2026-09-15

### Performance

- **maskid: single-pass byte masking for REST and WebSocket
  responses.** The tree pipeline cost every masked response two full
  JSON encodes plus a decode into a `map[string]any` tree; masking now
  marshals once and rewrites the bytes in one linear pass, copying
  everything verbatim except the integer spans the policy claims. On a
  50-row list response: 90.7µs → 23.8µs, 1681 → 410 allocations,
  76.5KB → 22.9KB. Walk semantics are replicated exactly (Exclude
  subtree pruning, arrays inheriting the introducing key, only integer
  literals convert) and pinned by a differential test against the old
  pipeline. Responses also keep their struct field order instead of
  the tree's alphabetical re-sort. Inertia keeps the in-place prop
  masking it had.
- **The per-request `httpx.Ctx` is pooled** (its writer wrapper
  inlined), the routers' global middleware chain is built once instead
  of per request, `Query`/`ClientIP` memoize per request, and the
  binder stopped re-splitting struct tags per field per request.
  Six-run medians: REST −11% latency with 32 → 24 allocations, CRUD
  read −13% with 31 → 23. One contract note, documented on the type:
  a handler must not retain the Ctx past its return — gin's
  long-standing rule, now nexus's too.
- **auth: concurrent resolves of the same token single-flight** into
  one backend call instead of a stampede; the identity cache's hit
  path takes a read lock instead of serializing every authenticated
  request; a `CacheOption` that left MaxEntries zero is no longer an
  unbounded map keyed by client-supplied tokens (defaults to 4096).
- **GraphQL: one body read per locked-down request** — the production
  gate binds the full request once (maskid unmask included) and hands
  it to the cached handler, instead of gate and handler each reading
  and parsing the same bytes. The document cache — previously a
  single mutex every GraphQL request took — shards 16 ways at
  full size; small caches keep exact global LRU order.

### Changed

- **`nexus dev` request-log fields are legible and semantic.**
  `dur=610µs status=200` rendered in near-invisible dim gray on light
  and dark terminals alike. Keys use a brighter muted tone, values the
  terminal's default foreground, and the request vocabulary colors by
  meaning: status 2xx green / 3xx cyan / 4xx amber / 5xx red, `dur`
  amber past 500ms, `error=` echoing the level color.

## [1.45.0] - 2026-09-15

### Added

- **Structured, colored boot diagnostics for config errors.** A missing
  env var in nexus.toml used to surface as a Go panic — the one line
  the operator needed buried under a goroutine dump. Config mistakes
  are operator errors, not bugs: `MustLoadConfig`, `MustLoadExtensions`
  and `Boot` now render a short aligned block and exit with status 2
  instead of panicking:

      ✗ nexus: cannot load config (expand env vars)

        file   nexus.toml:139
        error  env var OATS_DB_PASSWORD is not set
        fix    export OATS_DB_PASSWORD=…  — or write
               ${OATS_DB_PASSWORD:default} in nexus.toml for a fallback

  TOML syntax errors additionally show go-toml's annotated snippet with
  a caret under the offending token. Output is colored when stderr is a
  terminal and under `nexus dev` (whose log view passes ANSI through);
  `NO_COLOR` disables it. Under the hood the error is the new typed
  `nexus.ConfigError` (source, stage, line, cause, hint), built on
  `manifest.ExpandError` / `manifest.MissingEnvError` — `LoadConfig`
  still returns an error with equivalent flat text, so callers that
  handle it themselves are unaffected.

## [1.44.0] - 2026-09-15

A hardening and housekeeping release: a full audit of the codebase
(security, hot paths, dead code, duplication) applied as ~40 commits.
Three items are security-relevant; two are breaking for how the repo is
consumed, though not for code that imports the library.

### ⚠ Release / consumption changes

- **`cmd/nexus` is now its own Go module.** `go get
  github.com/paulmanoni/nexus` previously downloaded ~175MB of module
  cache the library never links — wazero (via viteless), `x/tools`,
  esbuild, bubbletea, cobra — all of it CLI-only. The CLI now versions
  independently, like `httpx/ginrouter` and `di/fxcontainer` before it.
  `go install github.com/paulmanoni/nexus/cmd/nexus@latest` keeps
  working throughout: until the first `cmd/nexus/vX.Y.Z` tag exists it
  resolves to the old in-parent CLI. Release order matters — tag the
  parent first, then bump the submodule requires, then push the nested
  tags; the checklist lives in `cmd/nexus/go.mod`. In-repo development
  builds every module against the checked-out tree via the new root
  `go.work`.
- **`extension/cache/redis` is now its own Go module**, taking go-redis
  (~19MB) out of the main dependency graph — the "default cache pulls
  no heavy deps" promise is now true of the download, not just the
  link. The blank-import opt-in is unchanged; run
  `go get github.com/paulmanoni/nexus/extension/cache/redis` once.

### Security

- **The config server's HMAC auth is now real.** `AuthHMAC` mode
  shipped as a phase-1 stub that accepted *any* non-empty
  `Authorization` header while the operator-facing validation insisted
  a secret be configured — so it looked enforced and wasn't. Server and
  clients now implement the documented scheme (HMAC-SHA256 over
  `app:timestamp:path`, 30s skew window, constant-time compare); the
  signed path means a token minted for one app/profile cannot fetch
  another's snapshot. The version-poll endpoint, which sent no
  credentials at all, signs too.
- **`Ctx.ClientIP` no longer believes client-supplied forwarded
  headers.** `X-Forwarded-For` / `X-Real-IP` were trusted
  unconditionally, letting any direct client pick its own IP — which
  bypassed per-IP rate limits and could mint unbounded limiter state.
  Forwarded headers are now honored only when the socket peer is a
  trusted proxy (default: loopback + private ranges, the LB-in-VPC
  case), and `X-Forwarded-For` resolves right-to-left across trusted
  hops so client-prepended entries can't spoof even behind a real
  proxy. Tune with `[runtime.server] trusted_proxies` (or
  `httpx.SetTrustedProxies`); an explicit empty list trusts no proxy.
  **Behavior change:** apps fronted by a proxy on a *public* address
  must list it in `trusted_proxies` to keep seeing real client IPs.
- **The rate limiter evicts.** Its bucket map was keyed by client
  input (an IP under `PerIP`) with no eviction — one entry per distinct
  client for the process lifetime, i.e. an unbounded-memory primitive
  once combined with the header spoofing above. Buckets idle past ten
  minutes are now swept, and the hit path takes a read lock instead of
  serializing every request behind the store-wide write lock.
- **GraphQL-subscription WebSockets are bounded.** The connection had
  no read limit, no deadlines, no pong requirement, and no cap on
  subscriptions per socket — unbounded frames, leaked goroutines on
  half-open connections, unbounded contexts per client. Now: 512KiB
  read limit, ping/pong liveness with read and write deadlines, and at
  most 100 live subscriptions per connection. Cleanup also no longer
  closes a channel that subscription goroutines were still sending on
  — a send-on-closed-channel panic in an unrecovered goroutine, i.e. a
  remote process crash under ordinary disconnect timing.

### Added

- **`[runtime.websocket]` hub tuning.** `max_connections`,
  `max_message_bytes` and `workers` now flow into every `AsWS` hub.
  The options existed for years but nothing passed them, so the
  5000-connection cap, 512KiB frame limit and 32-worker pool were
  unreachable from configuration.
- **`/__nexus/ready` finally gates on peers.** The readiness endpoint
  documented peer gating but nothing ever wrote the peer table — a k8s
  probe wired to it would route traffic to a pod whose upstreams were
  all down. `extension/peer`'s prober now reports peer-level
  reachability through the new `App.ReportPeerHealth` after every
  probe round.
- **`Module()` is the canonical entry point on every extension.**
  cors, errors, frontend, openapi, security and tls previously exposed
  only `Plugin(cfg)`; they now match auth/maskid/oauth2/peer/proxy/
  config/inertia. `Plugin` remains as an alias.
- **`nexus.DeclareVolume`** (method and Option) — `UseVolume` is
  deprecated but still works; its data-driven counterpart was already
  named `DeclareVolumeProvider`.

### Fixed

- **dataloader: nested queries fetch instead of returning zero
  values.** Dispatch was gated by a loader-lifetime `sync.Once`, so
  keys loaded after the first batch fired — level-2 resolvers in a
  nested query, exactly the case dataloaders exist for — silently
  resolved to the zero value and were deduped away on retry. Dispatch
  is now per batch; results merge into a shared per-request cache, and
  a fetch error fails only its own batch's thunks.
- **Dashboard traces show real durations.** `AsRest` invoked the trace
  middleware inline at the end of the chain, where its `c.Next()`
  returned immediately — every REST request reported ~0ms and a
  pre-handler status. The handler now brackets its own body via the
  new `trace.StartRequest`/finish pair.
- **`nexustest`/`InProcess` sees decorator endpoints.** The test
  harness skipped the deferred-options drain, so `//@`-annotated
  handlers existed in production but were invisible to tests.
- **Manifest auto-load honors `NEXUS_CONFIG`.** It stat'ed cwd-relative
  `nexus.toml`, so a `NEXUS_CONFIG` deployment loaded runtime config
  from one file and its manifest blocks from another (or none).
- **`nexus routes` takes `--toml`, matching `lint` and `doctor`.** The
  old `--yaml` flag set a format nothing parsed and fell through to the
  JSON parser — a leftover from the YAML-to-TOML manifest migration.
- **`nexus dev --tui` runs the child in dev mode.** It spawned `go run`
  with no `NEXUS_*` environment at all: stale embedded frontend instead
  of `web/dist` from disk, no PreserveDev state, peer/config dev gates
  locked.
- **WS hub shutdown no longer strands reader goroutines.** The blocking
  sends on the unregister channel had no drain after `Stop`, parking up
  to thousands of `readPump` goroutines forever.
- **`nexus.AuthRoute(...)` works on `AsWS`.** It was the one
  cross-transport option missing its WebSocket half, and now sits in
  the `EndpointOption` compile-time assertion so it can't regress.

### Performance

- **The GraphQL production gate stops re-parsing every request.** With
  introspection locked down, every request paid an uncached
  `parser.Parse` plus four AST walks *in front of* the document cache
  built to avoid exactly that. Verdicts are now memoized per query
  string (bounded; unique-query floods just degrade to the old cost).
- **maskid sheds its per-key allocations.** `isID` ran up to three
  `ToLower`s per JSON key of every masked response; verdicts now cache
  per key (bounded — inbound unmask keys are client-supplied) and
  `typeNames` caches per reflect.Type.
- **REST arg binding caches its tag survey per args type**, as the
  GraphQL side already did.
- **Metrics error ring writes are O(1).** The newest-first prepend
  copied the whole 1000-entry ring on every error while holding the
  entry lock; it is now a circular buffer, and the success path's
  timestamp is an atomic instead of a mutex acquisition per request.
- **The dashboard's initial JS chunk drops from 2.3MB to 913KB.**
  elkjs (~1.4MB minified) loads lazily on the Architecture tab's first
  layout, and the fonts ship latin + latin-ext only — the
  cyrillic/greek/vietnamese subsets browsers never requested are gone
  (17 woff2 files → 6, ~150KB off every dashboard-enabled binary).

### Removed

- **Dead-on-arrival API with zero callers anywhere:** the
  `graph.CacheMiddleware` / `CachedFieldResolver` pair (bare maps
  written from concurrent resolvers — data races waiting to happen),
  `AsyncFieldResolver`, `LazyFieldResolver` and the `With*Field`
  decorators, the `WithTypedResolver` reflection engine,
  `MustGetRoot`/`GetRootOr`, `LoggingMiddleware`,
  `DataTransformResolver`, `ConditionalResolver`,
  `GetMiddlewareCount`, `App.RateLimiter()`, `client.GenerateDTS`,
  and `middleware`'s never-wired `Phase` constants, `WithRejectHook`,
  and `Builtin()`/`Custom()` constructors.
- **Deprecated aliases whose only callers were the framework's own
  tests:** `App.Engine()` (use `Router()`), `nexus.Description()` (use
  `Describe()`), and the GraphQL `Desc()`/`Middleware()` options (use
  `Describe`/`GraphMiddleware`).
- **The orphaned `DeployAs` option plumbing.** `nexus.DeployAs` never
  existed; the option-side seam was inert end to end (its own godoc
  example did not compile). The registry's deployment column and
  setter remain — they are real API.
- **`extension/visitors`** — 947 lines with no reference anywhere in
  the repo, docs, or changelog.

### Changed

- **`github.com/go-viper/mapstructure/v2`** replaces the archived
  `mitchellh/mapstructure` (same API, maintained upstream).
- **Internal consolidation with one behavioral fix:** the four typed
  resource binders (db/cache/mail/storage) now share their mechanics
  in `internal/bindutil`, which also makes options lazily evaluated in
  all four — previously only `db.BindFromConfig` worked under
  `nexus.Boot`; the cache/mail/storage variants applied options before
  nexus.toml was parsed. The two TypeScript SDK emitters share one
  rendering core (`internal/tsgen`), CRUD's REST and GraphQL handler
  factories are one set, and schema generation and arg mapping share a
  single field-name resolver so the SDL can never disagree with
  binding.

## [1.43.0] - 2026-08-10

### Changed

- **`sdk = true` is now independent of `introspection`.** It previously took
  effect only under `nexus dev` or with introspection on, and even then mounted
  behind the introspection gate — so a production binary that (correctly) locked
  `/__nexus` down also stopped serving the client its own frontend imports,
  which is the one thing the switch exists to do. The two flags now govern
  separate surfaces: `introspection` the dashboard, `sdk` the client, neither
  implying the other. The SDK routes are public, as a browser fetching
  `client.js` requires. What that publishes is a map of the API surface — paths,
  methods, argument and response shapes; no data, and no route that was not
  already listening. To ship a frontend without publishing the map, vendor the
  files at build time with `nexus client --out` and leave the flag off. An
  explicit `Config.Client` mount is unchanged and still gated.

## [1.42.0] - 2026-08-04

### Changed

- **maskid: excluding a key now prunes its whole subtree**, not just that one
  scalar. This is what makes reference data expressible. A lookup row's primary
  key is spelled `id` like every other, so excluding `id` is not an option — but
  excluding the field that *holds* the lookups (`countries`, `categories`)
  spares every ID inside it, in both directions. Without it a masked country id
  broke `country === 1` comparisons and made a masked `id` unusable as a
  `?category=` query value, since the inbound key was not ID-shaped and so was
  never converted back. Mask the records; leave the code tables numeric.

## [1.41.2] - 2026-08-04

### Fixed

- **maskid: an Inertia page's scope is now decided once, from its props struct,
  rather than per prop.** A page carries heterogeneous props — an entity list
  whose type is in scope sitting next to a bare `[]uint` of the same entity's
  IDs (`appliedAdvertIds`) — and judging each prop by its own root type masked
  the first and not the second. The two then no longer compared equal, so a
  membership test against the sidecar list silently returned false for every
  row. An in-scope page now masks every prop it carries, leaving the field
  policy to decide which keys are IDs. Pages out of scope keep the per-prop
  behaviour.

## [1.41.1] - 2026-08-04

### Fixed

- **maskid: the type scope now resolves through generic response envelopes.**
  A handler returning `Response[T]` or `Page[T]` has the reflect name
  `Response[github.com/you/app/svc.Invoice]`, which no scope would name, so
  every such response fell out of scope and went unmasked on the JSON-walking
  transports (REST, Inertia, WebSocket). Type arguments are now unwrapped too.
  GraphQL was unaffected, since it scopes on the object type itself.

## [1.41.0] - 2026-08-04

### Added

- **`maskid.Config.Types` / `MatchType` — scope masking to named response types.**
  Masking was previously all-or-nothing, which rules it out for an app where only
  part of the surface can safely hand out opaque IDs (the rest feed a system
  outside the app that expects integers). Naming the types that stay inside masks
  those and leaves the others alone. The name is the Go type of the response,
  which is also its GraphQL object name; pointers, slices and the generic
  envelopes handlers return (`Response[T]`, `Page[T]`) all resolve to the
  underlying name. Only masking is scoped — unmasking always runs and needs no scope, since
  a value converts only if it decrypts.

### Changed

- **GraphQL arguments keep their `Int` declaration under maskid.** v1.40.0
  declared masked ID *arguments* as `MaskedID` too, which churned the SDL and the
  generated client for every caller. It turns out to be unnecessary: a masked
  value in `variables` is already converted back to an integer when the request
  body is bound, before graphql-go coerces it. Output fields still become
  `MaskedID` — there a rewrite genuinely can't work. An inline literal in a
  hand-written query now needs a raw integer.
- The GraphQL `GET` handler unmasks its `variables` query parameter, which
  bypassed request binding and so missed the conversion the `POST` path got.

## [1.40.0] - 2026-08-04

### Added

- **`extension/maskid` — opaque IDs on the wire, with no handler changes.**
  `maskid.Module(maskid.Config{Key: …})` replaces integer IDs with 22-character
  opaque strings on the way out and converts them back on the way in, so handlers,
  models and SQL keep using `int64` keys. Covers all four transports: REST out and
  in (path, query, header, form, JSON body — hooked once in `httpx` binding),
  GraphQL (ID fields declared as the new `MaskedID` scalar in both outputs and
  arguments, since graphql-go coerces through the declared type and a rewrite
  can't work there), Inertia (masked after prop resolution, so `Defer`/`Optional`
  props are included), and WebSocket (inbound envelope data, every outbound
  `Emit`). Requests carrying raw integers still work, so rollout is incremental.
  Default codec is a deterministic, authenticated AES permutation over a single
  block — real encryption rather than the reversible arithmetic of hashids/sqids
  — swappable via `Config.Codec`. Field policy is tunable with
  `Include`/`Exclude`/`Match`. Masking removes enumeration and inference; it is
  not access control. `nexus docs maskid`.

- **`nexus.DevStateDir()`** — the `nexus dev` session directory, for state that
  already has its own on-disk format and so doesn't fit `PreserveDev`'s
  snapshot-as-bytes model. Returns `""` outside `nexus dev`.

### Changed

- **An OAuth2 login now survives a `nexus dev` rebuild.** `extension/oauth2`'s
  default token store is go-oauth2's in-memory store, which is literally
  `NewFileTokenStore(":memory:")`; under `nexus dev` it is now pointed at a file
  in the session state directory instead. Tokens are opaque values held in the
  store rather than self-contained JWTs, so this alone keeps sessions alive
  across a rebuild — no more re-authenticating on every save. Same lifetime as
  `PreserveDev`: survives rebuilds, not a Ctrl-C. Production binaries are
  unaffected (they never see `NEXUS_DEV_STATE`), and setting `Config.TokenStore`
  opts out either way.


## [1.39.0] - 2026-08-04

### Security

- **WebSocket upgrades now default to same-origin.** Every upgrader in the
  framework hardcoded `CheckOrigin: func(*http.Request) bool { return true }` —
  the dashboard's trace stream (`/__nexus/events`) and live snapshot
  (`/__nexus/live`), every `nexus.AsWS` endpoint, and GraphQL subscriptions.

  Browsers apply neither CORS nor the same-origin policy to WebSocket
  handshakes, and the handshake carries cookies. So any page a victim visited
  could open a socket to a nexus app and read or send whatever it carries, as
  the victim — cross-site WebSocket hijacking. Confirmed against a running app:
  an upgrade with `Origin: https://evil.example` returned `101 Switching
  Protocols`; it now returns `403 Forbidden`.

  Worst case was the dashboard, whose streams carry every request trace and the
  cached auth identities. That surface needs introspection open — always true
  under `nexus dev`, off by default in production — so the realistic exposure
  was developer machines and deployments that opened introspection. `AsWS`
  endpoints and GraphQL subscriptions had no such gate and were exposed
  wherever they were mounted.

  Requests with no `Origin` header are still accepted: non-browser clients
  don't send one, and they carry no ambient authority to abuse.

  **This can break a legitimately cross-origin frontend.** Allowlist it:

  ```toml
  [runtime.websocket]
  allowed_origins = ["https://app.example.com", "*.example.com"]
  ```

  `"*"` restores the old accept-everything behavior. Loopback origins are always
  allowed under `nexus dev`, where the SPA (:5173) and app (:8080) are
  cross-origin by design. A `*.example.com` entry deliberately does not match
  the parent `example.com`.

- **`extension/inertia`'s validation-error cookie no longer hardcodes
  `Secure: false`.** It carries the user's submitted field values and was sent
  in cleartext even on HTTPS sites. Now follows the request scheme, honoring
  `X-Forwarded-Proto` for the usual TLS-terminating-proxy deployment, and sets
  `SameSite=Lax`.

### Added

- **`[runtime.server] idle_timeout`** (default **120s**, new). Go falls back to
  `ReadTimeout` when `IdleTimeout` is unset, and `ReadTimeout` was unset too —
  so idle keep-alive connections were held indefinitely and a few thousand cheap
  connections could exhaust the process's file descriptors. `"-1s"` restores
  Go's behavior. (`extension/tls` already set this; the main listener didn't.)
- **`[runtime.server] read_timeout` / `write_timeout` / `max_header_bytes`** —
  all OFF by default and deliberately so: a `write_timeout` cuts server-sent-event
  streams and long downloads mid-flight, and a `read_timeout` cuts large uploads.
  The framework can't tell which an app serves, so these are yours to set.
- **`[runtime.server] max_body_bytes`** — caps request bodies, 413 over the
  limit. **OFF by default**, for the same reason: nexus can't know whether an app
  accepts large uploads, and rejecting them at a framework-chosen ceiling would
  be a worse failure than the risk. Worth setting — without it every
  JSON-binding handler is an unbounded memory sink for anonymous clients.
- `httpx.CheckWebSocketOrigin` / `httpx.SetAllowedWebSocketOrigins` — the shared
  origin policy, for adapters outside the framework.

## [1.38.0] - 2026-08-03

### Changed

- **`nexus dev` rebuilds are ~30% faster.** Measured on a ~114MB app: 4.27s →
  2.99s per rebuild. Profiling the build's action graph makes the reason plain —
  of ~2000 actions in a warm rebuild, every one but the link is a cache hit, so
  the only lever that moves is making the linker emit less. Two defaults follow,
  both dev-only:
  - **DWARF is stripped by default** (`-ldflags=-w` — what `--fast` used to opt
    into). Worth ~20% of every rebuild, and a dev binary is discarded on the next
    save. `--debug` keeps it for delve and complete panic traces; `--fast` still
    exists and is now the default.
  - **The frontend bundle is stubbed out of the dev binary.** Under `NEXUS_DEV`
    `ServeFrontend` already reads `web/dist` from disk, and the SPA is served by
    viteless on :5173 regardless — so the embedded copy is dead weight that gets
    relinked on every save (9.5MB / 198 files on the app measured). `nexus dev`
    now maps it to empty files through the same `go build -overlay` it already
    uses for handler codegen, which Go applies to `//go:embed` reads: no build
    tags, no scaffold change, existing apps included. `--no-embed-stub` opts out.

    Scoped strictly to the tree a `ServeFrontend` call names, so assets an app
    genuinely reads at runtime (fonts, templates, seed data) are never touched.
    The bundle's `index.html` stays real HTML (boot fails fast without it) and
    the Vite `manifest.json` stays real too, since `extension/inertia` resolves
    entry chunks through the embed when `NEXUS_VITE_DEV` isn't set.

  Worth keeping in perspective: with build-then-swap this is
  latency-until-your-change-is-live, not downtime — the app keeps serving
  through the whole compile, and the swap itself is ~22ms.

### Added

- `nexus dev --debug` — keep DWARF in the dev binary (the inverse of `--fast`).
- `nexus dev --no-embed-stub` — embed the real frontend bundle in the dev binary.

## [1.37.1] - 2026-08-03

### Fixed

- **Ctrl-C on `nexus dev` no longer takes 5 seconds.** Shutdown ran
  `http.Server.Shutdown` with an unbounded context, and nothing cancelled the
  contexts of the requests it was waiting on — so a single request still in
  flight (an SSE stream, a long poll, a slow query, a browser mid-request) held
  the app open until the dev loop gave up and sent SIGKILL. Measured on a
  scaffolded app: 5.05s with one in-flight request, now 0.02s. This also cost a
  full 5s on every build-then-swap **rebuild** that happened to catch a live
  request, not just on Ctrl-C. Hijacked WebSockets were never affected.

### Added

- **`[runtime.server] shutdown_timeout`** (and `Config.Server.ShutdownTimeout`)
  — the graceful-drain window on SIGINT/SIGTERM. Defaults to 10s in production
  and 250ms under `nexus dev`, where nothing in flight survives the rebuild
  anyway. A malformed duration falls through to the default rather than
  refusing to boot.
- **`di.WithStopTimeout`** bounds the whole lifecycle stop chain, so a resource
  whose `Close` blocks can't hold the process open either. Recorded on
  `di.Spec` and honored by both containers (the fx adapter maps it onto
  `fx.StopTimeout`), and enforced by running `Stop` on its own goroutine — a
  hook that ignores its context outright is abandoned rather than waited on.

### Changed

- In-flight request contexts are now derived from a cancellable root
  (`http.Server.BaseContext`) and cancelled when the drain window closes, so a
  handler that selects on its context returns immediately and shutdown
  finishes early instead of running out the clock. Under `nexus dev` the cancel
  is immediate. Connections that still won't budge are closed outright, which
  `Shutdown` alone never did.
- The dev loop's SIGTERM→SIGKILL grace period drops from 5s to 750ms, and
  escalation now prints `● app didn't exit within 750ms · SIGKILL` instead of
  pausing silently. A healthy app bounds its own shutdown well inside that.

## [1.37.0] - 2026-07-25

### Added

- **`nexus.PreserveDev` — in-memory state that survives a `nexus dev` rebuild.**
  A rebuild replaces the process, so anything living in a map used to die with
  the old binary: seeded users, rows you POSTed, fixtures set up by hand. A
  value that implements `nexus.DevState` (`SnapshotDev() ([]byte, error)` /
  `RestoreDev([]byte) error`) and registers itself with
  `nexus.PreserveDev(name, v)` now hands its state to the dev loop on the way
  out and takes it back on the way in. Restore happens inside `PreserveDev`, so
  lazily constructed DI values work without ordering rules; the snapshot is
  written on the graceful shutdown `nexus dev` already triggers before swapping
  in the new binary. `nexus.PreserveDevJSON(name, get, set)` covers state you
  can marshal directly, with no methods to write.

  Dev-only (gated on the state file the CLI passes, so a production binary
  carries a no-op), per-session (state survives rebuilds, not a Ctrl-C),
  graceful exits only, and best-effort — a failed snapshot or restore is
  reported on stderr and skipped, never fatal, so stale state from a struct you
  just reshaped can't stop the app from booting. Caches are deliberately not
  preserved: they're rebuildable by definition, and restoring typed values
  through an `any`-shaped store is unsound. `nexus docs devstate`.
- **`auth.MemoryUserStore` implements `DevState`**, so dev users survive a
  rebuild once registered (`nexus.PreserveDev("auth.users", store)`). Password
  hashes travel as-is, so restored users authenticate exactly as before; a user
  the new process seeds itself wins over the snapshot, so changing the seed in
  code does what you expect.

## [1.36.0] - 2026-07-25

### Changed

- **`nexus dev` rebuilds are build-then-swap.** The loop used to kill the app and
  then compile, so every save took the server down for the whole build. The next
  binary now compiles while the current one keeps serving, and the swap happens
  only once the build is green. Measured restart outage on a small app: **1413ms
  (`go run`) → 22ms**, and it no longer scales with build time. Three
  consequences: a **failed build leaves the running app up** (the compile error
  prints, the last good build keeps serving); a save that doesn't change the
  binary **skips the restart entirely** (Go's output is content-addressed, so
  comment-only edits and edits outside the build graph preserve app state); and
  the app is exec'd directly instead of under `go run`, dropping that
  supervisor's ~40MB RSS. The freshly built binary is pre-executed once —
  aborted inside the Go runtime, before any package init or `main` — so the OS
  pays its first-exec cost (~450ms of code-signature validation on macOS) while
  the old process is still answering. `--go-run` restores the legacy loop.
- **The dev watcher is scoped to real build inputs.** `_test.go` files (never
  compiled into the binary), `testdata/`, and nested modules with their own
  `go.mod` no longer trigger rebuilds — unless the root module `replace`s into
  such a module, which makes it a genuine build input.
- **`nexus dev` starts faster.** The handler codegen's `go list` calls (one for
  the main package plus one per annotated package, every restart) collapse into
  a single cached `go list -find ./...` per session, invalidated by a
  go.mod/go.sum change or a lookup miss: 203ms → ~1ms per restart on a small
  app, 317ms → 0 on a large tree. The Inertia auto-detection and the viteless
  dev server boot now run off the critical path instead of ahead of the first
  compile, and the ready line no longer burns its frontend grace window on apps
  with no dev server: first build 0.85s → 0.66s, ready 1.31s → 1.16s with a
  frontend, 2.82s → 1.32s without one.

### Added

- **`.nexusignore`** — a per-project ignore list next to `nexus.toml`, read at
  startup and honored by both the Go watcher and the `--dist` frontend build.
  Patterns are a documented subset of `.gitignore`: a trailing slash matches
  directories only, a pattern without a slash matches at any depth, a slash
  anchors it to the project root, `**` spans directories, and `!` re-includes
  (later rules win). An ignored directory is pruned, so nothing inside it is
  watched. For generated trees, fixtures, or a sibling service's source living
  in the same repo.

### Fixed

- **A package created mid-session never rebuilt.** A newly created directory was
  filtered out as irrelevant before it could join the watch set, so neither its
  creation nor any later edit inside it reached the rebuild signal. Directory
  creates now register the tree first, and count as a change when it already
  holds build inputs (`mkdir pkg && write pkg/x.go` is one editor action, so the
  file usually lands before the watch does).

## [1.35.0] - 2026-07-23

### Added

- **`extension/inertia/inertiatest` — an in-process test harness for Inertia
  pages.** The Inertia-aware layer over `nexustest`: it boots a listener-less
  `App` (real router, middleware, DI, and reflective dispatch — no socket),
  issues visits with the correct `X-Inertia` headers, decodes the page object
  (from an XHR's JSON body or an initial load's `data-page` attribute), and
  returns a `*Page` with prop / merge / defer / redirect / validation
  assertions. A cookie jar persists across visits, so flash-error and session
  flows (e.g. a failed submit that 303s back with its errors) work like a real
  browser via `Visit.Follow()`. `New(t, cfg, opts...)` boots and wraps in one
  call; `Wrap(t, app)` layers Inertia visits over a `nexustest.App` you already
  built, so one app serves both REST/GraphQL and Inertia assertions. Visit
  helpers (`Get`/`Post`/`Visit`/`Partial`/`Load`), request options
  (`Version`/`ErrorBag`/`Except`/`Reset`/`Header`), and fluent `Page`
  assertions (`AssertComponent`/`AssertProp`/`Bind`/`AssertMerge`/
  `AssertDeferred`/`AssertError`/…) collapse the old boot-a-real-port
  boilerplate. Documented under `nexus docs inertiatest`.

## [1.34.0] - 2026-07-21

### Added

- **`extension/proxy` — a strangler-fig bridge.** Reverse-proxies routes to a
  legacy upstream (e.g. a Django app being migrated) AND registers each proxied
  route on the dashboard, tagged as a proxy (new `registry.ProxyTag`), clustered
  in a dashboard module — so the architecture graph becomes a live migration
  board showing which routes are still forwarded vs. already served natively.
  The core move is **auto-yield**: at boot, any configured route that already
  has a native nexus handler at the same method+path is skipped, so migrating a
  route is purely additive (add the `AsRest`, rebuild, and it leaves the
  "Proxied" cluster automatically). Includes a live migration burndown via a
  `migration` dashboard snapshot-extra, an optional catch-all `Fallback` for the
  long tail, and header/path passthrough (stdlib `httputil.ReverseProxy`, no new
  deps). Optionally **launches and supervises the upstream process** via
  `Config.Command` — distinct Dev/Prod argv (dev picked under `nexus dev` /
  `NEXUS_DEV`), working dir, env, line-prefixed child logs, a readiness gate,
  and graceful interrupt-then-kill shutdown — so one `nexus dev` boots both
  nexus and the legacy app. The dashboard gains a **"Proxied"** header panel (a
  live burndown of the `proxied` snapshot-extra: upstream, migrated-of-total, and
  per-route proxied/migrated status), and proxied routes cluster in their own
  module on the architecture graph — so the dashboard doubles as a migration
  cockpit.

- **`BindFromConfig` parity for cache / storage / mail.** `db.BindFromConfig`
  read a `[databases.*]` block; the other resource binders had no equivalent, so
  wiring a cache/disk/mailer from config meant hand-writing a `build func()`
  closure full of `nexus.Get` calls. Now `cache.BindFromConfig[T]("name")`,
  `storage.BindFromConfig[T]("name")`, and `mail.BindFromConfig[T]("name")` read
  the `[cache.<name>]` / `[storage.<name>]` / `[mail.<name>]` blocks directly.
  Every key is optional (cache overlays `NewConfig()` defaults; storage/mail
  default fields to zero), the build runs at boot so it works under `nexus.Boot`,
  and the driver's required-field validation still fires at boot. Lifecycle
  options (`WithDefault`, `WithDescription`) stay explicit in code — the block
  describes the connection, code describes its role.
- **`nexus.EndpointOption` — a name for the cross-transport per-op option
  contract.** `AsRest` / `AsQuery` / `AsWS` each take a transport-specific option
  interface; an option that works on all three (Public, Describe, WithIcon,
  HideFromDashboard, Use → auth.Required/Requires) had to satisfy all three by
  convention with nothing to name it. `EndpointOption` is that intersection —
  return it from your own cross-transport option, and a compile-time assertion
  keeps every built-in one honest.
- **`App.Router() httpx.Router`** — the correctly-named accessor for the app's
  router seam.

### Changed

- **`ginAuthMiddleware` → `authMiddleware`, `cors.ginHandler` → `corsHandler`,
  `auth.Describe` → `auth.InspectExtractor`.** Post-router-seam cleanup: internal
  helpers and one exported function carried gin/`Describe` names that no longer
  matched what they do (the middleware is router-agnostic; `Describe` collided
  with the cross-transport `nexus.Describe` option). Internal renames are
  invisible; `auth.Describe` stays as a `// Deprecated:` alias of
  `InspectExtractor`.

### Deprecated

- **`App.Engine()`** — use `App.Router()`. Both return the same `httpx.Router`;
  "Engine" was a leftover from the gin-only era. Alias kept.
- **`auth.Describe(Extractor)`** — use `auth.InspectExtractor`. Alias kept.

### Removed

- **`extension/tour`** — the guided-product-tour plugin has been removed. It was
  self-contained (nothing else in the framework imported it), so removal is a
  clean drop; apps that used it should pin an earlier nexus version or vendor the
  package.

## [1.33.3] - 2026-07-17

### Fixed

- **`go build` on Windows works again.** The embedded viteless dep cross-process
  cache lock (`viteless/internal/store`) used `golang.org/x/sys/unix` (`Flock`,
  `LOCK_SH/EX/UN`) unconditionally, so any `go build` of a nexus app on Windows
  failed with `undefined: unix.LOCK_SH`. Bumped to **viteless v0.2.1**, which
  splits the lock syscall behind a `flockFile`/`funlockFile` seam — `flock(2)` on
  unix, `LockFileEx`/`UnlockFileEx` on Windows — preserving the same advisory
  whole-file locking semantics on both. Pure dependency bump; no nexus API change.

## [1.33.2] - 2026-07-09

### Fixed

- **WebSocket handler panics no longer crash the process.** A WS message handler
  runs in the connection's read-loop goroutine (`ws.Hub.readPump`), and an
  unrecovered panic in a goroutine takes down the whole Go process — so a handler
  doing `m[k]=v` on a nil map or an out-of-range index could kill the server,
  while the identical bug in a REST handler was caught. `callWSHandler` now
  recovers, mints a `*trace.StackError`, and routes it through the same path a
  returned error takes: `finish(500)`, a `request.op`/`request.end` event on the
  bus (dashboard "failed traces" + captured stack), an `error` envelope to the
  client, and a `[nexus] panic recovered in WS handler ...` line on stderr — the
  read loop survives. This closes the last execution context that lacked panic
  recovery (REST/GraphQL, workers, crons, and pubsub subscribers already had it).

### Changed

- **Config auto-load panics now carry `nexus:` context.** Bare `panic(err)` sites
  in `autoLoad` are wrapped (`nexus: failed to read config %q`, `nexus: malformed
  config (%s)`, `nexus: malformed [extensions.*] ...`) so a startup failure names
  the config source and cause instead of dropping a raw toml/IO stack.

### Added

- **Boot-time self-check in dev — foot-guns surface at startup, not at 2am.** A new
  `nexus.RegisterBootCheck(func() []manifest.Issue)` hook lets a package report
  live-topology problems at boot; nexus runs every registered check plus the
  existing `nexus.toml` config lint (addresses, CIDRs, CORS-credentials-wildcard,
  rate limits, unimported extensions) automatically under `nexus dev` / any
  `NEXUS_DEV` run, printing them to stderr. It's **advisory** (never aborts boot;
  genuine fatal misconfig still fails where it already did) and **dev-only** (zero
  cost in production). Covers both `nexus.Run` and `nexus.Boot`. The flagship
  check: **pubsub** now reports *"N topic(s) declared but no transport bound — add
  pubsub.UseInMemory()/UseRabbit(...)"* at boot instead of only at the first
  `Publish`. Runs last among boot invokes, after `BindTopics`, so a bound
  transport never false-positives.
- **`ERRORS.md`** documents nexus's three-layer error model (registration panics →
  `nexus:` prefix; handler-returned errors → transport; runtime panics → recover →
  `StackError` → dashboard + stderr) and the recover-invariant every execution
  context upholds — enforced by the new `TestUserHandlerPanicsAreRecovered`.

## [1.33.1] - 2026-07-08

### Added

- **Database SQL logging is quiet by default outside dev, with a config opt-out.**
  GORM's default logger prints slow-query / error lines to stdout in every
  environment. Now the `db` manager sets the logger explicitly: warn-level under
  `nexus dev` / a development environment (`runtime.environment = "development"`
  or `NEXUS_DEV`), and **silent otherwise**, so a production binary stays quiet.
  Override per connection with `[databases.<name>] log = "..."` (or
  `db.Config.LogLevel`): `"silent"`/`"false"`/`"off"`, `"error"`,
  `"warn"`/`"true"`/`"on"` (GORM's slow-query+error default), or `"info"`/`"all"`
  (every statement). Record-not-found is no longer logged as an error.

### Fixed

- **`nexus client` auto-dump: parse `tsconfig.json` / `jsconfig.json` as JSONC.**
  Merging the SDK path mappings failed with `invalid character '}' looking for
  beginning of object key string` when the config contained comments or trailing
  commas — both of which `tsc` accepts (the files are JSONC). The parser now
  strips `//` and `/* */` comments and trailing commas before the strict JSON
  decode (string literals preserved); the rewritten file is normalized to strict
  JSON as before.

## [1.33.0] - 2026-07-08

### Added

- **OAuth2 folds into a single `auth.Module` call — `auth.Config.Endpoints` +
  `oauth2.Backend`.** Previously a token server meant a standalone `NewServer`
  provide plus a separate `auth.Module` plus hand-wired `AsRest` lines for
  `/oauth/token`, login, and logout. Now:
  - `auth.Config.Endpoints{Login, Logout, Token, Revoke}` lets `auth.Module`
    mount its own HTTP front doors, each backed by a `Config.Backend`
    capability — so one `auth.Module(auth.Config{...})` owns the whole auth
    surface. Each path is off unless set; all are `Public`.
  - The cohesive backend gains three optional capabilities (discovered by type
    assertion, like `Resolve`/`Login`/`Authorize`): `Issue(ctx, *Identity)
    (any, error)` (login response / token pair), `RevokeToken(ctx, token)
    error` (logout), and `TokenHandler() httpx.HandlerFunc` (the raw grant
    endpoint).
  - `oauth2.Backend(oauth2.Config{...})` returns a ready `auth.BackendOption`
    implementing every capability, so an OAuth2 server drops straight into
    `auth.Config.Backend`. New `oauth2.Config` fields `LoginPath` / `LogoutPath`
    / `LoginClientID` / `LoginClientSecret` power the JSON login endpoint.

      auth.Module(auth.Config{
          Backend:   oauth2.Backend(oauth2.Config{Authenticator: authFn}),
          Endpoints: auth.Endpoints{Token: "/oauth/token", Login: "/api/auth/login"},
      })

  All additive: the `Config.Endpoints` zero value mounts nothing, so existing
  configs are unchanged. See `nexus docs auth`.

### Changed

- **`oauth2.Module` is now a thin wrapper over `auth.Module`** — it builds the
  server via `oauth2.Backend` and declares `auth.Endpoints` for the token/revoke
  paths, eliminating the internal `holder`/`atomic.Pointer` bridge that threaded
  the live `*Server` into the resolver closure during DI startup. Behavior is
  unchanged (the end-to-end password-grant test is untouched); `oauth2.Module`'s
  `RevokePath` now responds `200 {"ok":true}` instead of `204`.

### Deprecated

- **`auth.LoginEndpoint` / `auth.LogoutEndpoint`** — superseded by
  `auth.Config.Endpoints.Login` / `.Logout`, which mount the same handlers from
  inside `auth.Module` and source the issuer/revoker from the backend's `Issue`
  / `RevokeToken` capabilities. Both remain as thin wrappers and keep working.

## [1.32.3] - 2026-07-07

### Added

- **`auth.LoginHandler` / `auth.LogoutHandler` — exported handler builders.**
  `LoginEndpoint`/`LogoutEndpoint`'s `WithIssuer`/`WithRevoker` are static
  callbacks set at module-build time, so they can't reach DI-provided services
  (e.g. an OAuth2 token server). These builders return the same
  `httpx.HandlerFunc` the endpoints install, so an app can wire them inside its
  own `AsRestHandler` factory — where deps ARE injected — without a package
  global:

      nexus.AsRestHandler("POST", "/auth/login",
          func(m *auth.Manager, srv *TokenServer) httpx.HandlerFunc {
              return auth.LoginHandler(m, func(ctx, id *auth.Identity) (any, error) {
                  return srv.IssueToken(ctx, id.ID)
              })
          }, nexus.Public())

  `LoginEndpoint`/`LogoutEndpoint` now delegate to them, so behavior is
  unchanged; this only adds the DI-friendly wiring path. See `nexus docs auth`.

## [1.32.2] - 2026-07-07

### Added

- **`auth.LogoutEndpoint` — companion to `LoginEndpoint`.** A one-line helper
  that registers a `POST` logout endpoint (default `/auth/logout`): it extracts
  the presented token, drops it from the identity cache (`Manager.Invalidate`),
  and — with `auth.WithRevoker(func(ctx, token) error)` — invalidates it in the
  app's own store (an OAuth2 server, a DB session). Options: `auth.LogoutAt(path)`
  and `auth.LogoutExtractor(e)` (default `Bearer()`; use `auth.Cookie(...)` for
  cookie sessions). Public and idempotent — it authenticates by the very token
  it revokes, always returns `200 {"ok": true}`, and reveals nothing about
  whether a session existed. See `nexus docs auth`.

## [1.32.1] - 2026-07-07

### Added

- **`auth.LoginEndpoint` — HTTP front door for `Manager.Login`.** A one-line
  helper that registers a `POST` login endpoint (default `/auth/login`) which
  authenticates a `{username, password}` body through the login-capable
  `Config.Backend` and returns the result — so apps no longer hand-write a
  handler just to reach `Manager.Login`. Options: `auth.LoginAt(path)` and
  `auth.WithIssuer(func(ctx, *Identity) (any, error))` to shape the success body
  (e.g. mint a token); without an issuer it returns `{"identity": …}`. The
  endpoint is `Public` (you can't require a token to obtain one), returns 401 on
  invalid credentials with no user enumeration, and needs a `Config.Backend`
  that implements `Login`. See `nexus docs auth`.

## [1.32.0] - 2026-07-07

### Added

- **`auth.Config.Backend` — one cohesive, DI-constructed auth backend.**
  Previously the resolver (`Scheme.Resolve`) was a static func that couldn't see
  DI dependencies, so apps needing a resolver bound to app services (a DB, a
  token server) had to smuggle them in via package globals + a backfill
  `Invoke`, and authorization lived in a separate `Config.Authorization` block.
  `Config.Backend` collapses this: declare ONE backend, built from the container
  via `auth.UseBackend(func(deps...) *YourBackend { … })` (or `auth.StaticBackend(v)`
  for no deps). The framework discovers capabilities by type assertion — a
  backend implements any subset of `Resolve(ctx, token)` (fills any `Scheme`
  with a nil `Resolve`), `Login(ctx, Credentials)` (powers the new
  `Manager.Login`), and `Authorize(id, required) bool` (replaces the
  `Config.Authorization` permission check). A scheme-less `Config` with a backend
  gets a default bearer scheme. New `auth.Manager.Login`.

  Fully backward compatible: every new field/method is additive, the `Backend`
  zero value reproduces prior behavior exactly, and `Scheme.Resolve` /
  `Config.Authorization` / `auth.Authenticate` / `ModelBackend` are unchanged.
  Note `UseBackend` returns your concrete type, distinct from the existing
  `auth.Backend` login interface. See `nexus docs auth`.

## [1.31.0] - 2026-07-06

### Added

- **`extension/mail` — outbound email.** A Laravel-Mail / ActionMailer-style
  abstraction: app code composes a `mail.Message` and hands it to one `Mailer`
  interface; the transport is chosen by config, so log-in-dev / SMTP-in-prod is
  a `Config` change, not a code change. Wired like a cache or disk — a typed
  `mail.Bind[T]` whose `T` embeds `*mail.Manager`, injected into handlers and
  shown on the dashboard as a `resource.KindMail` resource. Two backends, both
  dependency-free (no third-party mail library):
  - `log` — the default (empty-driver) backend; prints each message and sends
    nothing, the safe default for dev/tests. Exposes `.Sent()` for assertions.
  - `smtp` — any SMTP server over stdlib `net/smtp`: STARTTLS (587), implicit
    TLS / SMTPS (465), and PLAIN auth. Builds a proper MIME message —
    `multipart/alternative` for text+HTML, `multipart/mixed` for attachments —
    with quoted-printable bodies and RFC 2047-encoded headers.

  `mail.Message` carries From (defaulting to `Config.FromAddress`), To/Cc/Bcc,
  ReplyTo, Subject, Text, HTML, Headers, and Attachments; recipients are
  validated before any transport round-trip. New `resource.KindMail` +
  `resource.NewMail`. See `nexus docs mail`.

## [1.30.0] - 2026-07-06

### Added

- **`nexus build` embeds `nexus.toml` into the binary.** The built artifact
  is now self-contained — no config file needs to ship alongside it. When a
  `nexus.toml` sits in the main package's directory, `nexus build` bakes it in
  via the linker (`-ldflags -X`, base64-encoded) and `Boot` uses it as a
  fallback when no config is found on disk. Resolution order is unchanged and
  disk still wins: `NEXUS_CONFIG` → `nexus.toml` in cwd → next to the
  executable → the embedded copy. So a deployed binary Just Works with no
  sidecar file, yet operators can still drop a `nexus.toml` next to it to
  override without a rebuild. The raw file is embedded with `${VAR}`
  placeholders intact, so secrets resolve from the runtime environment and are
  never baked into the binary. A pure-Go app with no `nexus.toml` embeds
  nothing.

## [1.29.2] - 2026-07-05

### Fixed

- **Deployed binaries now find `nexus.toml` beside the executable.** `Boot`
  previously looked for `nexus.toml` only in the current working directory,
  so a binary launched from a different directory (a common deploy layout —
  `./app` run from `/home/user` with the config in a project subdir) silently
  fell back to framework defaults, most visibly binding `:8080` instead of the
  configured `addr`. `resolveConfigPath` now resolves in priority order:
  `NEXUS_CONFIG` → `nexus.toml` in cwd → `nexus.toml` next to the executable.
  Ship the binary alongside its `nexus.toml` and the configured listen address
  is honored regardless of launch directory.
- **A missing `nexus.toml` warns instead of silently defaulting.** `Boot` still
  tolerates the file's absence (config-less apps boot), but now prints a clear
  stderr notice that framework defaults are in effect and the listen addr is
  falling back to `:8080`, rather than leaving the mystery port unexplained.

## [1.29.1] - 2026-07-04

### Fixed

- **CI lint job now runs.** `golangci-lint-action` downloaded the prebuilt
  golangci-lint binary (built with go1.24), which refuses to analyze the
  go1.26 modules ("the Go language version used to build golangci-lint is
  lower than the targeted Go version"). Switched to `install-mode:
  goinstall` so CI compiles it with the runner's Go 1.26 — matching how
  `make lint` runs locally.
- **`nexus version` honors release `-ldflags`.** `var Version =
  resolveVersion()` ran its initializer at startup and overwrote any
  linker-injected value, so a binary built with
  `-ldflags "-X main.Version=vX.Y.Z"` still printed `dev`. `Version` is now
  left uninitialized (so the `-X` value survives) with the BuildInfo/vcs/
  `dev` fallback filled in `init()`. Normal `go install …@vX.Y.Z` installs
  were unaffected (they resolve the tag via BuildInfo).
- **Scaffold Go directive.** `nexus new` generated a `go.mod` pinned to
  `go 1.25.1`; bumped to `go 1.26` to match the framework's requirement.
- Fixed stale internal `DatabaseFromConfig[T]` comments →
  `db.BindFromConfig[T]`.

## [1.29.0] - 2026-07-03

### Added — built-in web security: CSRF enforcement + security headers

- **Security response headers are now on by default** — the framework
  applies `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, and
  `Referrer-Policy: strict-origin-when-cross-origin` to every app with no
  code and no config, matching Django/Rails/Laravel/Phoenix. Opt-in HSTS,
  Content-Security-Policy, Permissions-Policy, and COOP. This is a
  behavior change (new response headers on existing apps) but the three
  defaults are safe; set `[runtime.middleware.security] headers = false`
  to turn them off, or `frame_options = "-"` to omit one.
- **CSRF enforcement** (double-submit cookie) available as an opt-in
  built-in — `[runtime.middleware.security] csrf = true` (or
  `Config.Middleware.Security.EnableCSRF`). Safe methods mint a random
  token in a non-HttpOnly `csrftoken` cookie; unsafe methods must echo it
  in the `X-CSRFToken` header (or a `csrf_token` form field). Those names
  match the generated client SDK, so an existing frontend needs no change.
  Bearer/token-auth requests (an `Authorization` header) are skipped —
  they aren't CSRF-vulnerable. The cookie's `Secure` flag auto-derives
  from the request scheme, so dev over http works without config. CSRF is
  **off by default** because a nexus app is usually a token-authenticated
  API where CSRF is moot; enable it when you serve cookie/session-
  authenticated, server-rendered HTML forms (a template engine, or
  Inertia backed by session cookies).
- **Config, secure-by-default and zero-code:** the new
  `Config.Middleware.Security` field, populated from
  `[runtime.middleware.security]` in nexus.toml. No Go, no import.
- **New `extension/security` package** for the pieces the core path can't
  offer: a dashboard "Security" tab (`security.Plugin()`) and per-route
  middleware bundles (`security.NewCSRFMiddleware` /
  `NewHeadersMiddleware`) for apps that mix cookie- and token-auth routes.
  Global enforcement stays in the core so the middleware is never applied
  twice.
- **New `middleware/secure` package** holds the transport-neutral header
  and CSRF implementations shared by the core and the extension (deps:
  `httpx` + stdlib only).
- `nexus docs security` documents it.

### Added — `extension/storage`: file/object storage (local + S3 disks)

- **A filesystem/object-storage abstraction, the Go equivalent of Laravel
  Storage / Rails ActiveStorage / Django file storages.** Application code
  talks to one `Disk` interface (`Put` / `Get` / `Exists` / `Delete` /
  `Stat` / `List` / `URL` / `SignedURL`); the backend is chosen by config,
  so local-in-dev and S3-in-prod differ only by a `Config`.
- **Two backends, both dependency-free:**
  - **Local** — the OS filesystem under a root dir. Atomic writes
    (temp-file + rename) and path-traversal rejection.
  - **S3** — any S3-compatible store (AWS S3, MinIO, Cloudflare R2,
    DigitalOcean Spaces) spoken directly over HTTPS with **hand-rolled
    SigV4 signing — no AWS SDK is linked** (go.mod is unchanged), keeping
    nexus's zero-heavy-dep ethos. `SignedURL` returns a presigned GET;
    virtual-hosted and path-style URLs both supported via `Endpoint`.
- **Wired like a cache or database** — `storage.Bind[T]("name", build,
  opts…)` where `T` embeds `*storage.Manager`; injected into handlers and
  registered as a dashboard resource (new `resource.KindStorage`).
- `PutOption`s: `WithContentType`, `WithSize` (stream without buffering),
  `Public`. `nexus docs storage` documents it. The SigV4 signer is
  verified against AWS's published example vector.

### Added — auth: password hashing + credential login backends (Django-style, phase 1)

- **Pluggable password hashing** (`auth.Hasher` / `auth.Hashers`), the
  Django `PASSWORD_HASHERS` analogue — encoded strings are self-describing
  (`<id>$<payload>`) so a set verifies any member algorithm and **rehashes
  on login** when the stored hash is stale. Three shippers, **zero new
  deps** (`golang.org/x/crypto` + stdlib `crypto/pbkdf2`):
  - `auth.BCrypt()` — the default (predictable memory, cost 12).
  - `auth.Argon2id()` — memory-hard alternative.
  - `auth.PBKDF2()` — PBKDF2-HMAC-SHA256 at 600k iterations (Django interop).
  - `auth.DefaultHashers()` = bcrypt default + argon2id/pbkdf2 for verify.
- **Pluggable password policy** (`auth.PasswordValidator`), the Django
  `AUTH_PASSWORD_VALIDATORS` analogue: `MinLength`, `NotNumericOnly`,
  `NotCommon`, `NotSimilarToUser`, run via `auth.ValidatePassword(...)`;
  `auth.DefaultValidators()` gives a sensible baseline.
- **Credential login backends** (`auth.Backend` + `auth.Authenticate`),
  the Django `AUTHENTICATION_BACKENDS` analogue — backends are tried in
  order; the built-in `auth.ModelBackend` authenticates a `Password`
  credential against a pluggable `auth.UserStore` with a `Hashers` set
  (constant-timing on the unknown-user path to avoid enumeration). Ships
  an in-memory `auth.MemoryUserStore` for dev/tests; swap in any store
  (GORM, external API) by implementing three methods.
- Non-breaking: this fills in the *login* half around the existing
  token-`Resolver`/`Scheme` surface, which is unchanged. (Phase 2:
  sessions + login/logout; phase 3: per-object policies.)

### Changed — CI now gates formatting and lint

- **`gofmt` is enforced in CI.** The whole tree was reformatted with the
  Go 1.26 toolchain (130 files — doc-comment reindentation + trailing
  newlines, no logic changes), and a new `gofmt` CI job + `make fmt-check`
  target fail the build if any file drifts. Run `make fmt` to fix.
- **`golangci-lint` gate added** (`.golangci.yml`, pinned `v1.64.8`, run
  per module). The enabled set — `gofmt`, `govet`, `ineffassign`,
  `durationcheck`, `makezero` — is a **ratchet** like the coverage floor:
  it passes clean today and only guards against regressions. Tighten
  `.golangci.yml` as the tree is cleaned up (errcheck / unused / staticcheck
  / bodyclose / errorlint are noted as next candidates); never loosen it to
  make a red build green. `make lint` runs the same locally.

## [1.20.4] - 2026-06-19

### Fixed — wildcard route params now match gin's convention on every backend

- **`c.Param("rest")` for a `*rest` route again returns a leading-slash suffix
  on the stdlib and chi backends.** gin exposes a `*filepath` capture as
  `/app.js` (leading slash); after the router-seam migration the stdlib backend
  returned `app.js` (ServeMux's `{rest...}` drops the slash) and the chi backend
  returned `""` (chi stores the capture under the key `*`, so the original name
  missed entirely). Handlers that build a path from the capture — notably the
  dashboard's `"assets" + c.Param("filepath")` — resolved to `assetsapp.js` /
  `assets`, 404'd, and served assets with an **empty MIME type**, so browsers
  blocked the dashboard's own JS module (`/__nexus/assets/index-*.js`). The
  seam now normalizes the wildcard capture to gin's leading-slash form via the
  new `httpx.WildcardName` helper, so `c.Param` behaves identically on gin,
  chi, and stdlib. Named (`:id`) params are unaffected.

## [1.20.3] - 2026-06-19

### Fixed — `stdrouter` treated `GET /` as a catch-all, swallowing assets

- **Trailing-slash routes are now exact matches.** gin treats a registered
  route as an exact path (its catch-all is the `*rest` wildcard), but
  `net/http.ServeMux` treats any pattern ending in `/` as a *subtree* match. So
  a home-page route like `GET /` (e.g. `inertia.Page("GET", "/", …)`) silently
  became a catch-all that shadowed every unmatched `GET` path — including
  `GET /assets/*` — so the SPA's JS/CSS never reached the `ServeFrontend`
  `NoRoute` fallback and the page loaded with **no assets**. `stdrouter` now
  appends ServeMux's `{$}` end-of-path marker to trailing-slash routes
  (`/` → `/{$}`, `/admin/` → `/admin/{$}`), restoring gin's exact-match
  semantics. Wildcard (`*rest`) routes keep their subtree behavior, and
  `NoRoute` / `Static` register their patterns directly, so the intended
  catch-alls are unaffected.

## [1.20.2] - 2026-06-19

### Fixed — `stdrouter.Static` no longer panics next to a catch-all route

- **`Static` is now scoped to `GET`.** It previously registered its prefix
  method-less (`/media/`), which Go 1.22's `ServeMux` treats as ambiguous
  against an app's catch-all `GET /` (the static pattern has a more specific
  path but matches *more* methods, so neither is a strict subset) and panics at
  boot — e.g. an SPA frontend plus a `Static("/media", …)` upload dir. A static
  file server only serves GET/HEAD, and `ServeMux` serves HEAD off a GET
  pattern, so registering `GET /media/` keeps full behavior while making the
  static route a strict path-refinement of `GET /` — no conflict. (gin's radix
  router tolerated the overlap; the stdlib default did not.)

## [1.20.1] - 2026-06-19

### Added — form accessors on `httpx.Ctx`

- **`httpx.Ctx` now carries gin-compatible form helpers**, closing a gap from the
  router-seam migration where low-level handlers that read POST bodies had no
  neutral equivalent for gin's form methods: `PostForm`, `DefaultPostForm`,
  `GetPostForm`, `PostFormArray`, `FormFile`, `MultipartForm`, and
  `SaveUploadedFile`. They read `*http.Request` directly, so they behave
  identically on the stdlib, chi, and gin backends — no adapter changes. Empty
  string for a missing key; `GetPostForm`/`DefaultPostForm` distinguish
  present-but-empty from absent (the latter falls back to the default).

## [1.20.0] - 2026-06-19

### Changed — `ginrouter` is now its own module (gin out of the main graph)

- **`github.com/paulmanoni/nexus/httpx/ginrouter` is a separate Go module.** gin
  (and its sonic / golang-asm / goccy / validator / json-iterator tree) is no
  longer a dependency of the main `github.com/paulmanoni/nexus` module at all —
  the module graph drops from 182 to 161 modules. The default build was already
  gin-free at link time (v1.19.0); now it's gin-free at the `go.mod`/`go.sum`
  level too, so `go get github.com/paulmanoni/nexus` pulls none of gin's tree.
- **The import path is unchanged** (`.../httpx/ginrouter`); it just versions
  independently. To use the Gin backend, add the module explicitly:

  ```bash
  go get github.com/paulmanoni/nexus/httpx/ginrouter
  ```
  ```go
  nexus.Boot(nexus.WithRouter(ginrouter.New()))
  ```
- `stdrouter` (default) and `chirouter` remain inside the main module — chi has
  no transitive dependencies, so it costs nothing to keep bundled.

## [1.19.0] - 2026-06-18

### Added — Pluggable HTTP router (`httpx` seam)

- **The HTTP router is now pluggable behind `github.com/paulmanoni/nexus/httpx`.**
  Handlers and middleware see a transport-neutral `*httpx.Ctx`; the concrete
  router is an adapter selected at boot via `nexus.WithRouter(...)` (or
  `Config.Router`). Three backends ship:
  - `httpx/stdrouter` — **the new default**, Go 1.22 `net/http.ServeMux`, with
    **zero third-party router dependencies**. The default binary no longer links
    gin (or its sonic/golang-asm/goccy/validator tree).
  - `httpx/chirouter` — opt-in (`go-chi`).
  - `httpx/ginrouter` — opt-in; the only package that imports gin now.
- Chain execution (`Next`/`Abort`, panic recovery, error accumulation) lives in
  `httpx.Ctx`, so every middleware runs identically on any backend; the router
  only matches paths and returns params. App-level middleware wraps the whole
  mux (runs even on 404/405, e.g. CORS preflight); per-op middleware runs inside
  the matched route. Route strings keep the canonical `:id` / `*rest` syntax on
  every backend.

### Changed (BREAKING) — gin no longer in the public surface

- `App.Engine() *gin.Engine` → **`App.Router() httpx.Router`**.
- Low-level handlers that took a `*gin.Context` parameter now take **`*httpx.Ctx`**.
- `gin.H` → **`httpx.H`**. `AsRestHandler` factories return `httpx.HandlerFunc`.
- To keep gin, add `nexus.WithRouter(ginrouter.New())` and blank-import the
  adapter — selecting gin/chi pulls their dependency trees back into the build;
  the stdlib default links none.

## [1.18.1] - 2026-06-18

### Security — Client SDK

- **SDK routes now sit behind the introspection gate.** An explicit
  `Config.Client{Enabled: true}` mount previously served `/__nexus/client/*`
  (the manifest — a full API map — and the `.d.ts` type surface) to anyone,
  with no `introspection_networks` enforcement. The mount now reuses the same
  gate as the dashboard: open under `nexus dev` / `Introspection`, 404 to
  non-allowed peers in a locked-down production binary. Opt back out with
  `Config.Client.Unguarded` when you deliberately serve the runtime SDK to the
  public (prefer vendoring `sdk/` at build time via `nexus client --out`).
- **Token store defaults to in-memory.** `NexusClient` previously defaulted to
  `localStorageTokenStore()`, leaving bearer tokens readable by any XSS and
  persistent across reloads. The default is now `memoryTokenStore()`;
  persistence is opt-in. The Vue/React `useNexus()` composables likewise default
  to in-memory and switch to `localStorage` only when `VITE_NEXUS_TOKEN` is
  explicitly set.
- **CSRF double-submit for cookie-based strategies.** Under `cookie` / `chain` /
  `custom` auth the SDK now, on state-changing requests, reads a non-HttpOnly
  CSRF cookie and echoes it in a header so a cross-site post is rejected. No
  cookie set → no header, so apps without CSRF cookies are unaffected.
- **Login token location is declarable, not just guessed.** The SDK reads the
  token from a configured dotted path before falling back to the heuristic walk,
  removing the risk of picking up an unrelated `token` field.

### Added

- `auth.Config` gains `LoginTokenField`, `CSRFCookie`, and `CSRFHeader`, bridged
  into the SDK manifest's auth section so the generated/runtime client reads the
  token from the declared location and uses the matching CSRF pair. Empty fields
  fall back to framework defaults: `data.token`, `csrftoken`, and `X-CSRFToken`
  (the Django/Laravel convention), exposed as `client.DefaultTokenField`,
  `client.DefaultCSRFCookie`, and `client.DefaultCSRFHeader`.
- `client.AuthMeta` (+ `WithDefaults`, `Empty`), `Handler.SetAuthMeta`, and
  `App.SetClientAuthMeta` — the additive bridge carrying the above without
  changing the `Mount` / `SetClientAuthInfo` signatures.
- `Config.Client.Unguarded` — escape hatch for serving the runtime SDK publicly
  from a locked-down binary.
- `Manifest.Projected` — marks the stripped (non-`Public`) manifest so the SDK
  surfaces a clear "the server is serving the stripped manifest" error on an op
  miss instead of a cryptic "no op named X".

### Changed

- **Breaking (runtime behavior):** apps relying on cross-reload token
  persistence must now pass `tokenStore: localStorageTokenStore()` explicitly
  (or set `VITE_NEXUS_TOKEN` for the composables).
- **Breaking (runtime behavior):** apps that intentionally serve the runtime SDK
  from a production binary with introspection off must set
  `Config.Client.Unguarded = true`.
- The SDK's default CSRF cookie/header changed from the Angular convention
  (`XSRF-TOKEN` / `X-XSRF-TOKEN`) to the Django/Laravel convention
  (`csrftoken` / `X-CSRFToken`). Override via `auth.Config` or the `NexusClient`
  constructor (`csrfCookie` / `csrfHeader`).
