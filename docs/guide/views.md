# Reactive views (templ)

`github.com/paulmanoni/nexus/v2/view` renders [templ](https://templ.guide) components
from nexus and keeps them reactive — with no JavaScript build and no Node. You
write plain templ; what is reactive follows from what the template reads.

```templ
//nexus:page GET /
templ Home() {
	@Layout("Pets") {
		@Counter()
		@SearchBox()
		@PetResults()
	}
}

templ Counter() {
	{{ count := view.State(ctx, 0) }}
	<button onclick={ count.Set(count.Get() + 1) }>+1</button>
	<button onclick={ count.Set(0) } disabled?={ count.Get() == 0 }>reset</button>
	<p>Clicked { count.Get() } times</p>
	if count.Get() >= 5 {
		<p>High five!</p>
	}
}

templ SearchBox() {
	{{ q := view.Use[*Search](ctx).Query }}
	<input value={ q.Get() } oninput={ view.Do(func(e view.Event) { q.Set(e.Target.Value) }) }/>
}

templ PetResults() {
	{{ q := view.Use[*Search](ctx).Query }}
	{{ pets := view.Use[*PetStore](ctx).Search(q.Get()) }}
	<ul>
		for _, p := range pets {
			<li>{ p.Name }</li>
		}
	</ul>
}
```

```go
type Search struct{ Query *view.Signal[string] }   // page state, shared through DI

func NewSearch() *Search { return &Search{Query: view.Initial("")} }

func main() { nexus.Boot(nexus.Provide(NewPetStore, NewSearch)) }
```

That is the whole app: `nexus dev` compiles the views, registers the page and
the shard, and keeps them current as you save.

## The rules

::: v-pre

| You write | What happens |
| --- | --- |
| `{{ x := view.State(ctx, v) }}` | state owned by one component instance (call it unconditionally, at the top — like a React hook) |
| a struct of `*view.Signal` fields, provided with DI; `view.Use[*T](ctx).Field` | state shared by every component on the page; each page gets its own copy |
| `{ x.Get() }` | text kept current in the browser |
| `attr={ … x.Get() … }`, `attr?={ … }` | attribute kept current in the browser |
| `onclick={ x.Set(…) }` | the one action: runs in the browser |
| `onclick={ view.Do(func(e view.Event) { … }) }` | an action that reads the event or takes several steps |
| `if x.Get() … { } else { }` | every branch rendered; the browser shows the one that holds |
| `{{ … x.Get() … }}`, `for … x.Get()`, `@C(x.Get())` | server code reads the signal: the component is a **shard**, re-rendered on the server when `x` changes |
| `//nexus:page GET /path` above a component | registers it as a page |
| `//nexus:auth Required`, `//nexus:auth Requires p…`, `//nexus:auth Public`, `//nexus:use expr` | its gates |

:::

- Signals have two methods, `Get` and `Set`. In markup, `Set` *describes* what an
  event does; it changes nothing on the server.
- **Shards are nexus ops.** A shard takes its own `//nexus:auth`/`//nexus:use`, or inherits
  the gates of the pages that render it — across packages — which must agree. A
  shard no page reaches, or one reached by pages with different gates, must name
  its own, so a shard endpoint is never less guarded than its page. Its
  arguments and restored signals are user input: validate them.
- A shard's own signals survive its re-renders: the browser sends their current
  values back and `view.State` restores them under the same ids.
- Everything the directives do has a Go form — `view.Page(m, p, C, gates…)`,
  `view.Shard(C, gates…)`, `view.Expose[T]()` — and a Go registration wins.

## Live pages

A live page keeps its state **on the server**, one copy per connected page, and
changes through events sent over a WebSocket — for pages whose state must not
live in the browser or that change while you look (admin boards, dashboards).
It is declared like a `nexus.Resource`:

```go
var Module = nexus.Module("pets",
	nexus.Path("/admin"),
	view.Live[*Board]("/board", auth.Required()).
		Provide(NewBoard),
)

type Board struct {
	Pets    []Pet
	Adopted map[string]bool
}

func NewBoard() *Board { return &Board{} }

// Mount fills this page's copy: dependencies first, path parameters last.
func (b *Board) Mount(ctx context.Context, store *Store) error {
	b.Pets, b.Adopted = store.All(), map[string]bool{}
	return nil
}

// An event is an exported method: dependencies first, then its arguments.
func (b *Board) Adopt(ctx context.Context, name string) error {
	b.Adopted[name] = true
	return nil
}
```

```templ
templ (b *Board) Render() {
	for _, p := range b.Pets {
		if b.Adopted[p.Name] {
			<span>{ p.Name } adopted</span>
		} else {
			<button onclick={ view.Send(b.Adopt, p.Name) }>adopt { p.Name }</button>
		}
	}
}
```

- **The DI instance is the template.** Each page render and each connection gets
  its own copy; `Mount` fills it (`sock *view.Socket` tells it whether the page
  is connected). `Render()` is a templ method component.
- **Events are methods** of the shape `func(ctx, deps…, args…) error`:
  dependencies are pointer or interface parameters, injected from DI; the rest
  are the arguments `view.Send(b.Adopt, p.Name)` passes. Any such exported method
  is callable by a client that can open the page — authorize and validate
  inside it; an error is reported to the page, which keeps its state.
- After each event the server renders again and the browser **patches the page
  in place**: focus, caret and what you are typing survive. Signals still work
  inside `Render` for browser-only state (a note field, a toggle) — the signal
  wins over the server's copy of its value.
- **`data-nx-ignore`** leaves an element (with an `id`) as the browser has it:
  something a script drew into, like a chart canvas, or a shell whose menus
  and collapsed state the user set. A render that gives it a new `id` (a chart
  whose data changed) replaces it.
- The first request renders the page on the server; the page then connects
  (`<path>/_live`), mounts again with `Connected()` true, and reconnects if the
  socket drops. Gates on `view.Live` apply to both.
- **Server push.** `sock.Subscribe(topics…)` in `Mount` (it only takes effect on
  the live connection) makes the page hear `view.Broadcast(ctx, topic, data)`
  — sent from an event, a handler, a job, anywhere. The page's optional
  `Info(ctx, deps…, msg view.Message) error` runs, then it re-renders; with
  several messages waiting it handles them all and renders once. `Info` is
  never callable from the browser.

```go
func (b *Board) Mount(ctx context.Context, sock *view.Socket, store *Store) error {
	sock.Subscribe("adoptions")
	b.Adopted = store.Adopted()
	return nil
}

func (b *Board) Adopt(ctx context.Context, store *Store, name string) error {
	store.SetAdopted(name, true)
	view.Broadcast(ctx, "adoptions", name) // every open board refreshes
	return nil
}

func (b *Board) Info(ctx context.Context, store *Store, msg view.Message) error {
	b.Adopted = store.Adopted()
	return nil
}
```

- **Forms.** `view.Submit(b.Add)` on a form's `onsubmit` sends its fields to an
  event whose last parameter is a struct, bound by `form:"name"` tags exactly
  as a REST form binds; `view.Change(b.Validate)` on `oninput`/`onchange` sends
  them as the user types (debounced). The struct's `validate:` tags are checked
  first; an event that fails them, or returns `nexus.Invalid()`, re-renders with
  the errors — read them with `view.Errors(ctx).Field("name")` — and
  the form keeps what was typed; on success the form resets to its
  server-rendered values. How fields and re-renders meet is set out under
  [Form fields](#form-fields).

```go
type PetInput struct {
	Name string `form:"name"`
	Kind string `form:"kind"`
}

func (b *Board) Add(ctx context.Context, store *Store, in PetInput) error {
	errs := nexus.Invalid()
	if in.Name == "" {
		errs.Field("name", "a name is required")
	}
	if errs.Any() {
		return errs
	}
	store.Add(Pet{Name: in.Name, Kind: in.Kind})
	return nil
}
```

```templ
<form onsubmit={ view.Submit(b.Add) } oninput={ view.Change(b.Validate) }>
	<input name="name" value={ b.Draft.Name }/>
	<p>{ view.Errors(ctx).Field("name") }</p>
	<button type="submit">Add</button>
</form>
```

- A panic in `Mount`, `Render`, an event or `Info` is reported to the page as
  an error; the connection stays up.
- `view.Send`, `view.Submit` and `view.Change` also work in a component
  library's `Props.Attributes`
  (`templ.Attributes{"onclick": view.Send(b.Adopt, p.Name)}`).

### Form fields

A live page re-renders after every event, and the browser patches the page
while the user may be typing into it. Fields follow these rules — guarantees,
tested against the runtime on a DOM that keeps field state as a browser does
(not yet in real browsers):

1. **A field's value belongs to the user until the server's value changes.**
   After the user types, a re-render that renders the same `value` leaves what
   they typed; one that renders a different `value` replaces it.
2. **A focused field is never overwritten** — not its value, its checked state
   or its selection, whatever the server renders. (A browser on its own lets a
   changed `value` or `selected` attribute through to a field the user hasn't
   edited yet; the runtime puts back what was shown.)
3. **A `<select>` follows the server's chosen option** the same way: when the
   options the server marks `selected` change, the select shows them; when
   they don't, the user's choice stands. No option marked means the first.
4. **A `<textarea>` follows the server's value** — its text — like an input's
   `value` attribute.
5. **A checkbox or radio** follows its `checked` attribute by the same rules.
6. **A form resets after a successful submit** (`view.Submit`) to the values
   the new render gives it; after an invalid one (validation errors) it keeps
   what was typed.

`view.Value(v)` marks a field **server-owned**: unless it has focus, it shows
the server's value after every render, even one that left the value alone —
for a field the server corrects or clears (a normalised amount, a search box
an event empties). Spread it into the field; on a select it chooses the
option with that value, on a textarea it is the text:

```templ
<input name="amount" { view.Value(b.Amount)... }/>
<select name="sort" { view.Value(b.Sort)... }>
	<option value="name">Name</option>
	<option value="date">Date</option>
</select>
<textarea name="notes" { view.Value(b.Notes)... }></textarea>
```

A field bound to a signal (`value={ q.Get() }`) follows the signal: browser
state is newer than the server's copy.

Forms that post over plain HTTP need nothing for CSRF: when the app's CSRF
middleware is on, the runtime adds the token as a `csrf_token` field to a
same-origin POST form as it submits, and sends it with every shard re-render.
See [Web security](./security#csrf).

## Navigation

`view.Link` is an in-app link: following it fetches the page and patches it
into the current one — no document reload — with live pages connecting and
disconnecting as needed, the title and needed stylesheets/scripts updated,
and the back and forward buttons working. Shared DI state keeps its value
across pages. A modified click (new tab) or another site's link behaves as a
plain link.

```templ
@view.Link("/board") {
	Adoption board
}
@view.Link("/", templ.Attributes{"class": "underline"}) {
	Home
}
```

### What travels

- **The first render comes over HTTP, once.** The socket that connects next
  mounts, renders and compares with what the page already has: identical, and
  nothing is sent; different (a `Mount` that does more once `Connected()`),
  and the page is corrected. The first change carries the render the browser
  patches against from then on.
- **Updates send what changed.** Renders are cut into tokens — each text node,
  attribute value and `view.Send` argument its own token — and diffed. A patch
  copies unchanged runs, sends changed values, reuses markup already on the
  page (a new list item sends only its values), and names markup the
  connection has seen before by a dictionary index, so it never travels
  twice. On a 100-row board (37 KB rendered):

  | Change | Sent |
  | --- | --- |
  | a count | 15 B |
  | a new row | 92 B |
  | adopting a row (first time / after) | 283 B / 112 B |

  Messages are compressed (`permessage-deflate`); a full render, when one is
  needed, is about 1.5 KB. A patch that would not be clearly smaller than the
  render is sent as the render; a browser that loses track asks for one.
- **Reconnects.** A dropped socket retries with jittered backoff (at once when
  the network returns or the tab is looked at again); the page shows
  `data-nx-live-state="disconnected"` meanwhile — style it. Events sent while
  disconnected are queued and delivered once connected. The server mounts
  afresh and sends the full render; browser-side signals keep their values.

## The toolchain

| Command | Views | Tailwind |
| --- | --- | --- |
| `nexus dev` | compiled on start and on every `.templ` save, in memory — the build overlays them; `--view-files` writes them to disk instead | `tailwindcss --watch` |
| `nexus build` | compiled through the build overlay — nothing written | built once, minified |
| `nexus test` / `nexus vet` | `go test` / `go vet` through the same overlay | — |
| `nexus lsp` | handed to gopls as editor buffers (see [Editor support](#editor-support)) | — |
| `nexus generate views [--check]` | written to disk / verified (CI) | — |

The generated Go (`*_templ.go`, `view_gen.go`, `view_imports_gen.go`) never
needs to be on disk: every nexus command compiles it through an overlay, and
the editor gets it from `nexus lsp`. If you write it (`nexus generate views`,
`nexus dev --view-files`) for a plain `go build`/`go test` or an editor on plain
gopls, gitignore it. Generation errors point at the `.templ` line and column;
`nexus dev` keeps the last good build serving until the template compiles again.

**Tailwind:** a stylesheet that does `@import "tailwindcss"` (outside a Vite
frontend) is compiled with the Tailwind standalone CLI: `input.css` → `output.css`.
When it imports `./sources.generated.css`, nexus writes that file — an `@source`
for your templates and for every Go dependency that ships `.templ` files — so a
component library imported as a Go module contributes its classes.

## Component libraries

A library like [templUI](https://templui.io) works as is. Its components take
extra attributes in `Props.Attributes`; a `templ.Attributes{…}` literal compiles
like attributes on an element, actions and bindings included:

```templ
@button.Button(button.Props{Attributes: templ.Attributes{
	"onclick":  count.Set(count.Get() + 1),
	"disabled": count.Get() >= 10,
}}) {
	+1
}
```

Serve the library's files with `view.Assets`:

```go
mux := http.NewServeMux()
utils.SetupScriptRoutes(mux, dev)          // templUI's /templui/js/*
nexus.Boot(
	nexus.Provide(NewPetStore, NewSearch),
	view.Assets("/templui/js/", mux),
	view.Assets("/assets/", http.StripPrefix("/assets/", http.FileServer(http.Dir("assets")))),
)
```

templUI's scripts initialize content that appears later, so its widgets keep
working inside re-rendered shards.

## Component kit

`github.com/paulmanoni/nexus/v2/view/ui` is a small kit of components shaped
for live pages: the pieces admin screens keep rebuilding — buttons, fields,
tabs, dialogs, menus, data tables, toasts, loaders, page headers and badges.
They are plain templ, styled with Tailwind utilities over CSS variables, and
their events are ordinary `view.Send` / `view.Change` / `view.Submit` scripts.

Load the kit's stylesheet and behaviour in the document head. Importing the
package serves them at `/_view/ui/ui.css` and `/_view/ui/ui.js`:

```templ
import (
	"github.com/paulmanoni/nexus/v2/view"
	"github.com/paulmanoni/nexus/v2/view/ui"
)

templ Layout(title string) {
	<head>
		<link rel="stylesheet" href="/assets/css/output.css"/>
		@view.Script()
		@ui.Script()
	</head>
	…
}
```

The components' classes are Tailwind utilities: `nexus dev` and `nexus build`
add the kit's templates to your Tailwind sources (`sources.generated.css`),
so your stylesheet carries them. Colors come from `--ui-*` tokens defined in
`ui.css`, light and dark (`prefers-color-scheme` or a `.dark` class). Each
token falls back to the shadcn/templUI token of the same role (`--primary`,
`--border`, `--muted`, …), so an app themed for templUI is matched as is;
set `--ui-primary` and friends to theme the kit alone.

### A table on a live page

```go
type Orders struct {
	Rows                []Order
	Total, Page, Size   int
	Query, Sort         string
	Desc                bool
}

type OrderQuery struct {
	Q    string `form:"q"`
	Size int    `form:"size"`
}

func (o *Orders) Search(ctx context.Context, db *DB, in OrderQuery) error { … }
func (o *Orders) SetPage(ctx context.Context, db *DB, page int) error   { … }
func (o *Orders) SortBy(ctx context.Context, db *DB, key string) error  { … }

func (o *Orders) table() ui.TableProps {
	return ui.TableProps{
		ID:      "orders",
		Columns: []ui.Column{{Key: "number", Label: "Number", Sortable: true}, {Key: "total", Label: "Total", Align: "right", Sortable: true}},
		Rows:    len(o.Rows), Total: o.Total, Page: o.Page, PageSize: o.Size, PageSizes: []int{10, 25, 50},
		Sort:    o.Sort, Desc: o.Desc, Query: o.Query,
		OnSearch: view.Change(o.Search),
		OnSort:   func(k string) templ.ComponentScript { return ui.Loading(view.Send(o.SortBy, k), "orders") },
		OnPage:   func(n int) templ.ComponentScript { return ui.Loading(view.Send(o.SetPage, n), "orders") },
		Actions:  true,
	}
}
```

```templ
templ (o *Orders) Render() {
	@ui.PageHeader(ui.PageHeaderProps{Title: "Orders", Crumbs: []ui.Crumb{{Label: "Home", Href: "/"}, {Label: "Orders"}}}) {
		@ui.Button(ui.ButtonProps{Icon: ui.Icon("plus"), Href: "/orders/new", Nav: true, Hotkey: "mod+n"}) {
			New order
		}
	}
	@ui.DataTable(o.table()) {
		for _, r := range o.Rows {
			@ui.TableRow(ui.RowProps{ID: "order-" + r.ID, Href: "/orders/" + r.ID, Actions: []ui.MenuItem{
				{Label: "Copy link", Icon: ui.Icon("link"), Copy: "/orders/" + r.ID},
				{Label: "Cancel", Danger: true, OnClick: view.Send(o.AskCancel, r.ID)},
			}}) {
				<td>{ r.Number }</td>
				<td class="text-right">{ r.Total }</td>
			}
		}
	}
}
```

The table shows the state it is given and sends events; the live page
re-renders it. The search form sends the fields `q` and `size` to
`OnSearch`; a row with `Href` opens as an in-app link when clicked anywhere
but on its own links, buttons and fields (Ctrl/Cmd-click opens a new tab).

### Components

| Component | What it is |
| --- | --- |
| `Button(ButtonProps)` | variants (`Primary`, `Secondary`, `Outline`, `Ghost`, `Danger`, `Success`, `Warning`, `Info`, `LinkStyle`), sizes (`Sm`, `Md`, `Lg`), `Icon`, `IconOnly`, `Loading`, `Disabled`, `Href` (+`Nav`), `Hotkey`, `OnClick` |
| `Field(FieldProps)` + `Input`, `Select`, `Textarea`, `Checkbox` | a label, the control, help text and the error — taken from `view.Errors(ctx)` for the field's `Name`, which also marks the control `aria-invalid` |
| `Tabs(TabsProps)`, `TabPanel(id, active)` | a tab is a live event (`OnSelect`), a link (`Href`) or a browser-switched panel (`Panel`); the clicked tab shows selected at once |
| `Dialog(DialogProps)` | server-owned (`OnClose`: render it while open, close it in the event) or browser-side (`ID`, opened with `ui.OpenDialog(id)`); Esc and the backdrop close it unless `Persistent` |
| `Dropdown(ButtonProps, items…)`, `MenuButton`, `RowActions(items…)` | menus of `MenuItem`s: links, `Copy` entries, `OnClick` events, separators; placed at the trigger, closed by an outside click, Esc or a choice |
| `DataTable(TableProps)`, `TableRow(RowProps)`, `EmptyState`, `SearchInput` | toolbar, sortable headers, rows, empty state, counts, page size and pager |
| `Toast(ToastProps)` | rendered after an event; the browser shows each `ID` once |
| `Loader(LoaderProps)`, `Skeleton(class)` | an inline or overlay spinner; a pulsing placeholder |
| `PageHeader(PageHeaderProps)` | breadcrumbs, title, description, actions (its children) |
| `Badge(variant)`, `Icon(name)` | a status label; the kit's icons |

### Loading

`ui.Loading` wraps an event so a loader covers part of the page until the
live page answers:

```templ
<button onclick={ ui.Loading(view.Send(x.Run), "preview") }>Run</button>
```

It is the kit's spelling of `view.Send(x.Run).Loading("preview")`: it
prefixes the script, so it wraps `view.Send`, `view.Submit` and `view.Change`
alike, and needs nothing from the view runtime — the reply is seen as the
live root dropping its `aria-busy`. With no ids it covers the live region
that sent the event; a kit `Button` that triggers it also shows its spinner.
Where the script can't be wrapped (a library's `templ.Attributes`), the
attribute does the same on click or submit:

```templ
<button onclick={ view.Send(x.Run) } { ui.LoadingAttr("preview")... }>Run</button>
```

### In the browser

`ui.js` works by attributes and event delegation, so markup that live
updates, shards and navigation bring in needs no setup:

| Attribute / call | Behaviour |
| --- | --- |
| `data-ui-hotkey="mod+s"` (`ui.Hotkey`, `ButtonProps.Hotkey`) | the combination clicks the element (`mod` is Cmd on a Mac, Ctrl elsewhere); combinations without a modifier don't fire while typing; an open dialog owns the keyboard |
| `data-ui-copy="/orders/7"` (`ui.CopyLink`, `MenuItem.Copy`) | copies the link (a path becomes an absolute URL) and confirms with a toast |
| `data-ui-open="id"`, `data-ui-close` | open and close a browser-side dialog |
| `data-ui-href` on a row | in-app navigation on click |
| `nxui.toast(text, {variant, title, duration})` | a toast from your own scripts |
| `nxui.loading(el, ids)`, `nxui.copy(text)`, `nxui.dialog.open(id)` | the same behaviour from code |

### Owning a component

```sh
nexus add ui table            # DataTable and what it renders, into ./ui
nexus add ui button dialog --dir internal/kit
nexus add ui all --package kit
```

`nexus add ui` copies a component's template, the components it renders and
the kit's shared pieces (`base.go`, `ui.go`, `icon.templ`, `ui.js`, `ui.css`)
into your project as your own package, rewriting the package clause. The
copy serves its assets under `/_ui/<package>/`; load them with its
`Script()` instead of the kit's. Existing files are kept unless `--force`.

Limits: browser-switched tabs (`Panel`) and open menus follow the server
again when a live page re-renders; a browser-side dialog keeps its content
as first rendered (`data-nx-ignore`) — use a server-owned dialog for
content that changes.

## Islands

Some widgets are better written with a JavaScript framework: a rich text
editor, a chart library, a drag-and-drop board. An island puts a component of
the project's Vite frontend into a templ page, and only that part of the page
loads JavaScript for it.

Each file under `web/src/islands` is an island, named by its path there
without the extension (`Chart`, `admin/Editor`):

| File | Mounted with |
| --- | --- |
| `.vue` | Vue (`createApp`, or `createSSRApp` to hydrate) |
| `.tsx`, `.jsx` | React (`createRoot`, or `hydrateRoot`) |
| `.ts`, `.js` | its own `export function mount(el, props, ctx)`, returning `{ update(props), unmount() }` |

### Declaring and placing an island

Declare each island once in Go, with the type of its props:

```go
var Chart = view.NewIsland[ChartProps]("Chart")

type ChartProps struct {
	Points []int `json:"points"`
}
```

Then place it like a component. Its children are rendered on the server and
stay until the island mounts:

```templ
templ Stats(points []int) {
	@Chart(ChartProps{Points: points}, view.Visible()) {
		<div class="h-64 animate-pulse rounded bg-muted"></div>
	}
}
```

The Go compiler checks the props where the island is placed. The client SDK
types them for the component as `NexusIslandProps`:

```vue
<script setup lang="ts">
import type { NexusIslandProps } from 'nexus-client'
const props = defineProps<NexusIslandProps['Chart']>()
</script>
```

Props travel as JSON (public, like any page data) and must encode to an
object. By default an island mounts as soon as the page loads.
`view.Idle()` waits for the browser to be idle, `view.Visible()` until it is
about to scroll into view, and `view.Media("(min-width: 768px)")` until the
query matches. Each island is its own lazily loaded chunk, so a page
downloads only the islands it shows.

The Vite plugin checks names against files both ways. An island declared in
Go with no file is a warning under `nexus dev` and fails `vite build`. A file
nothing declares is reported.

### Page signals in props

A props field may be a page signal. The island receives its value, and is
updated when the signal changes anywhere on the page:

```go
type ChartProps struct {
	Kinds []KindCount           `json:"kinds"`
	Query *view.Signal[string] `json:"query"` // typed as string for the island
}
```

```templ
{{ q := view.Use[*Search](ctx).Query }}
@Chart(ChartProps{Kinds: kinds, Query: q})
```

The island can also set the signal, and everything that reads it follows:
bindings, branches, and shards re-rendered on the server.

- A Vue island emits `update:query`. That means `defineModel('query')` works.
- A React island calls `props.setQuery(v)`.
- A `mount` module calls `ctx.set('query', v)`. `ctx.signals` lists the
  signal props.

### Rendering on the server

`view.SSR()` renders an island on the server too, so its HTML is in the page
before any JavaScript runs (search engines, slow phones). The browser
hydrates it when it mounts:

```templ
@Chart(props, view.Visible(), view.SSR()) { … }
```

Turn on the server build in `vite.config`:

```js
nexus({ islands: { ssr: true } })
```

`nexus build` then also writes `web/dist/ssr/islands.js`. That file is a
small Node server with every dependency inside, so it runs without
`node_modules`. Run it beside the binary:

```sh
node web/dist/ssr/islands.js     # listens on 127.0.0.1:13715
```

- **Another address:** set `NEXUS_ISLANDS_PORT` / `NEXUS_ISLANDS_HOST`,
  and point the app at it with `view.IslandServer(url)`.
- **Under `nexus dev`:** islands render in the browser only.
- **When the server is down:** the island renders in the browser only and
  the app logs why. The page still renders either way.
- **`.ts` / `.js` islands:** export `render(props)` for SSR.
- **Live pages:** a socket re-render never calls the islands server, because
  the mounted island keeps its DOM.

### The frontend

The frontend is the one `nexus.Frontend` serves. A templ app whose
frontend is only islands needs no `index.html` and no entry:

```js
// web/vite.config.js
import vue from '@vitejs/plugin-vue'
import nexus from './sdk/nexus-vite-plugin.js'

export default { plugins: [vue(), nexus()] }
```

```go
//go:embed all:web/dist
var webFS embed.FS

nexus.Boot(nexus.Frontend(webFS, "web/dist"))
```

Under `nexus dev` islands load from Vite with hot reload. `nexus build`
adds the islands loader to the Vite build, and the binary loads it from the
manifest. Vue plugins (a router, i18n, a component library) or React
providers go in `web/src/islands/_setup.ts`: its default export takes the Vue
app, or wraps the React element.

Islands work with the rest of the package:

- **Live pages.** A re-render with new props updates the mounted island
  instead of remounting it, so its own state survives.
- **Navigation.** `view.Link` unmounts the islands that leave the page and
  mounts the new ones.
- **Shards.** An island inside a shard is mounted again when the shard
  re-renders.

Without a build to load from (a Go test, or a build that lacks the island)
the page still renders. The island keeps its children, its element's
`data-error` says why, and the server logs it once.

The app's own static files must not share Vite's output directory. If the app
serves `/assets/`, set `build.assetsDir` in `vite.config` to something else.

## Testing views

`github.com/paulmanoni/nexus/v2/view/viewtest` drives pages end to end in a Go
test, without a browser. The page is fetched from the app over a real HTTP
server; the view runtime and the page's compiled expressions run in goja
against a small DOM; a live page connects its WebSocket to the app; events go
out as the runtime sends them and patches are applied by its own morph — so
signals, shards, live events, navigation and the [form rules](#form-fields)
are the ones the browser runs.

```go
func TestBuilder(t *testing.T) {
	app := nexustest.New(t, config.Runtime{}, appOptions()...)
	p := viewtest.Mount[*reports.Builder](t, app, viewtest.As(&auth.Identity{ID: "7", Kind: "staff"}))

	p.Fill("title", "Sales").Select("ds_keys", "main.id").Click("#run")
	p.Expect("#save").Enabled()
	p.Click("#save")
	p.Expect("#saved li").Text("Sales")
	p.Expect("title").Value("") // the form reset
}

func TestHome(t *testing.T) {
	p := viewtest.Get(t, app, "/")
	p.Click("#inc").Expect("#count").Text("1")
	p.Fill("#q", "cat").Expect("#pets").ContainsText("Mochi") // a shard re-render
}
```

- **Opening.** `viewtest.Mount[*T](t, app, opts…)` opens the live page
  `view.Live[*T]` serves (found in the app's registry; `viewtest.At("/orders/42")`
  for a prefix with parameters) and waits for its socket to join.
  `viewtest.Get(t, app, path, opts…)` opens any page; `p.Status()` is its HTTP
  status (a gate's 401, say). `app` is the `*nexus.App` or `nexustest.App`.
- **Who.** `viewtest.As(&auth.Identity{…})` makes every request and the socket act
  as that identity (extension/auth's test credential, honoured only in test
  binaries), so gates, areas and policies run as for a real sign-in.
  `viewtest.Header(k, v)` and `viewtest.Cookie(c)` add anything else; cookies the
  app sets are kept.
- **Locators** are a form field's `name`, else a CSS selector (`#id`, `.class`,
  `tag`, `[attr=v]`, `:checked`, `:not(…)`, descendant and `>` combinators).
- **Actions** — `Fill`, `Select` (one value, or several on a multiple select),
  `Check`/`Uncheck`, `Click`, `Submit`, `Press`, `Focus`/`Blur`, `Visit`, `Back`
  — act as a person does: the field takes focus, input and change fire, a
  submit button submits its form, a link navigates. Each first lets the page
  settle (replies to earlier events arrive, `view.Change`'s debounce runs), so
  it acts on a quiet page. `Wait()` settles explicitly.
- **Reading** — `Text`, `Attr`, `Value` (what the field shows, not its `value`
  attribute), `Checked`, `Exists`, `Count`, `HTML`, `URL`, `Console`.
- **`Expect(loc)`** assertions — `Exists`, `Absent`, `Count`, `Text`,
  `ContainsText`, `Attr`, `NoAttr`, `Value`, `Enabled`/`Disabled`,
  `Checked`/`Unchecked`, `Visible`/`Hidden`, `Focused` — retry until they hold
  or the timeout (`viewtest.Timeout`, 5s) passes, so they also see what another
  page's `view.Broadcast` pushes.
- **Not a browser.** There is no layout or CSS: `Visible` means no `hidden`
  ancestor. Only the view runtime's scripts run (not a component library's or
  inline ones), islands don't mount (there is no Vite build to load them
  from), and file inputs are not supported. Page time is virtual: debounces and
  reconnect backoffs run without waiting. Checking layout, or behaviour that
  depends on a real browser's quirks, needs a real browser:

### In a real browser

```go
p := viewtest.Browser(t, app, "/", viewtest.As(staff))   // headless Chrome
p.Click("#to-report").ExpectURL("/report")
p.Click("#run").Expect("#ran").Text("1")
if !p.Visible("#save") { t.Fatal("the save button is off screen") }
p.Viewport(390, 700).Screenshot("report.png")
```

`viewtest.Browser` opens the page in a headless Chrome over the DevTools protocol (no
third-party driver): layout and CSS apply, every script runs, islands mount from the
built bundle, and `Click` presses the mouse where the element really is — a covered
button isn't clicked. `Box(loc)` is an element's rect, `Visible` checks size, CSS and
the viewport, `Eval(js)` runs script, `Viewport(w, h)` resizes, `Screenshot(path)`
writes a PNG; `Expect(loc)` retries `Text`, `ContainsText`, `Visible`, `Hidden`. It
takes the same options as `Get`/`Mount`. The test is skipped when no Chrome is
installed — set `NEXUS_CHROME` to its binary when it lives elsewhere.

## Editor support

Run **`nexus lsp`** as the language server for both `.go` and `.templ` files. It
starts gopls and sits in front of it:

- **Go files** see the generated code. nexus lsp compiles the views (and the
  `//nexus:` handler registrations) in memory and opens the result in gopls as
  unsaved editor buffers, so `pages.Home()` or `pets.Module` type-checks, completes
  and jumps to definition — into the `.templ`, not the generated Go — with nothing
  generated on disk. It recompiles as you type in a `.templ` and when a `.go` file
  is saved.
- **`.templ` files** get the views compiler's errors (syntax, `//nexus:page`,
  shard gates, browser-side expressions) and the Go type errors of the expressions
  inside them, both at the `.templ` line and column. Definition, hover, completion
  and references on a Go expression are answered by gopls at the matching position
  of the generated code (templ's source map), and the same code nexus builds —
  plain templ's language server generates different Go.
- **`//nexus:` directives** that don't parse (an unknown keyword, the v1 `//@`
  spelling) are diagnostics on the `.go` file.

Setup — point the editor's Go and templ language servers at `nexus lsp`
(`--gopls <path>` picks gopls, `--log <file>` writes a debug log):

- **Zed / IntelliJ (GoLand):** set the nexus plugin's language server command to
  `nexus lsp`, for Go and templ files.
- **VS Code:** for `.go`, set `"go.alternateTools": {"gopls": "<script>"}` where
  the script runs `exec nexus lsp "$@"` — nexus lsp stands in for gopls (it accepts
  `serve` and gopls's flags, and runs any other gopls subcommand, like `version`,
  with gopls). For `.templ`, use a generic LSP client extension that runs
  `nexus lsp` for the `templ` language instead of templ's own server.
- **Neovim / Helix / Emacs:** configure `nexus lsp` as the server for the `go`
  and `templ` filetypes, in place of gopls and `templ lsp`.

An editor still on plain gopls needs the generated files on disk: run
`nexus dev --view-files` (or `nexus generate views`). `templ fmt` works unchanged.

## Limits

- An `if` on a signal renders every branch on the server, so a branch must not
  assume its condition.
- A shard re-render replaces its HTML: focus inside a shard is lost (live pages
  patch in place).
- Live pages: one goroutine per connected page; `view.Broadcast` reaches the
  pages connected to this process only (a replica's own pages); file inputs are
  not sent over the socket.
- `/`, `%`, indexing and field access (other than the event's) do not compile to
  the browser yet.
- `/_view/*` ignores nexus `route_prefix`.
- An island's signal props are its top-level fields; a signal nested deeper
  is passed as its value but cannot be set from the island. Svelte and other
  frameworks mount through a `.ts` module that exports `mount` (and `render`
  for SSR).

See `view/example` for a multi-package app built on templUI, with two Vue
islands in `view/example/web`: a server-rendered chart bound to the page's
search signal, and a meter on the live board. Its `/registry` page is built
from the component kit.

## On the dashboard

Pages, live pages and shards are endpoints like any other, and `/__nexus` shows
them as such: the endpoint list marks them PAGE, LIVE or SHARD (filter with
**Views**), and a live page's detail lists its component and its events with
their argument types — `Add(pets.PetInput)`, `Clear()`. Every live event is its
own trace, named `Type.Event` (`pets.Board.Adopt`), carrying how long the event
and its render took (`live.duration_ms`) and what travelled back (`live.render`:
`patch` or `full`, `live.bytes`).

A page's gates feed `auth.OpGates` under its route (`GET /board`) and under its
component (`pets.Board`), so navigation can ask whether the user may open a page
from the same declaration that guards it.
