# Reactive views (templ)

`github.com/paulmanoni/nexus/view` renders [templ](https://templ.guide) components
from nexus and keeps them reactive — with no JavaScript build and no Node. You
write plain templ; what is reactive follows from what the template reads.

```templ
//@page GET /
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
| `//@page GET /path` above a component | registers it as a page |
| `//@auth Required`, `//@auth Requires p…`, `//@auth Public`, `//@use expr` | its gates |

:::

- Signals have two methods, `Get` and `Set`. In markup, `Set` *describes* what an
  event does; it changes nothing on the server.
- **Shards are nexus ops.** A shard takes its own `//@auth`/`//@use`, or inherits
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
  them as the user types (debounced). An event that returns `nexus.Errors`
  re-renders with them — read them with `view.Errors(ctx).Field("name")` — and
  the form keeps what was typed; on success the form resets to its
  server-rendered values. A field's value is overwritten only when the
  server's `value` attribute changes, and never while it has focus.

```go
type PetInput struct {
	Name string `form:"name"`
	Kind string `form:"kind"`
}

func (b *Board) Add(ctx context.Context, store *Store, in PetInput) error {
	errs := nexus.NewErrors()
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
| `nexus dev` | generated on start and on every `.templ` save, written to disk (gopls reads them) | `tailwindcss --watch` |
| `nexus build` | compiled through the build overlay — nothing written | built once, minified |
| `nexus generate views [--check]` | written to disk / verified (CI) | — |

Gitignore the generated files: `*_templ.go`, `view_gen.go`, `view_imports_gen.go`.
Generation errors point at the `.templ` line and column; `nexus dev` keeps the
last good build serving until the template compiles again.

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

The frontend is the one `nexus.ServeFrontend` serves. A templ app whose
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

nexus.Boot(nexus.ServeFrontend(webFS, "web/dist"))
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

## Editor support

Every file is plain templ, so templ's tooling works unchanged: the templ language
server (gopls inside `{ … }` — completion, hover, type errors), `templ fmt`, and
the templ extensions for VS Code, GoLand, Zed, Neovim, Helix and Emacs.

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
search signal, and a meter on the live board.
