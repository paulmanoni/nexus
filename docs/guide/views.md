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
- `view.Send` also works in a component library's `Props.Attributes`
  (`templ.Attributes{"onclick": view.Send(b.Adopt, p.Name)}`).

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

## Editor support

Every file is plain templ, so templ's tooling works unchanged: the templ language
server (gopls inside `{ … }` — completion, hover, type errors), `templ fmt`, and
the templ extensions for VS Code, GoLand, Zed, Neovim, Helix and Emacs.

## Limits

- An `if` on a signal renders every branch on the server, so a branch must not
  assume its condition.
- A shard re-render replaces its HTML: focus inside a shard is lost (live pages
  patch in place).
- Live pages: one goroutine per connected page; server push (pubsub), form
  events and diffs are not in yet.
- `/`, `%`, indexing and field access (other than the event's) do not compile to
  the browser yet.
- `/_view/*` ignores nexus `route_prefix`.

See `view/example` for a multi-package app built on templUI.
