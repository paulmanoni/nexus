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

## Live views

Views come in three kinds, as in Phoenix:

| Kind | State | Use it for |
|---|---|---|
| **templ component** | none (arguments in, markup out) | almost everything: rows, cards, forms, layouts |
| **live view** | on the server, one copy per place it is shown; events, `Info` | a page, or a widget with state and events of its own |
| **signal** (`view.State`) | in the browser | browser-only state: a toggle, a note being typed |

A live view keeps its state **on the server** and changes through events sent
over a WebSocket — for pages whose state must not live in the browser or that
change while you look (admin boards, dashboards). It embeds `view.LiveView`,
and the same type is a **page** when it is routed and a **part of a page** when
another live view embeds it.

```go
type Board struct {
	view.LiveView
	Pets    view.Assign[[]Pet]
	Adopted view.Assign[map[string]bool]
}

// Mount fills this copy: dependencies (pointer or interface parameters,
// injected), then — optionally — its props.
func (b *Board) Mount(ctx context.Context, store *Store) error {
	b.Pets.Set(store.All())
	b.Adopted.Set(map[string]bool{})
	return nil
}

// An event is an exported method: dependencies first, then its arguments.
func (b *Board) Adopt(ctx context.Context, name string) error {
	b.Adopted.Update(func(a *map[string]bool) { (*a)[name] = true })
	return nil
}
```

```templ
templ (b *Board) Render() {
	for _, p := range b.Pets.Get() {
		if b.Adopted.Get()[p.Name] {
			<span>{ p.Name } adopted</span>
		} else {
			<button onclick={ view.Send(b.Adopt, p.Name) }>adopt { p.Name }</button>
		}
	}
}
```

Route it with `view.Live` — or put `//nexus:live <path>` on the type:

```go
var Module = nexus.Module("pets",
	nexus.Path("/admin"),
	view.Live[*Board]("/board", auth.Required()).Provide(NewBoard), // Provide is optional
)
```

```go
//nexus:live /board
//nexus:auth Required
type Board struct { … }
```

**Props.** A view's input is one struct, its props: routed, they are bound from
the path and the query (`path:` and `query:` tags; `validate:` rules apply, a
failure is a 422); embedded, the parent passes them. `Update` runs with new
props — a patch of the page's URL, or a parent passing different ones — and
without an `Update`, `Mount` runs again:

```go
type OrderProps struct {
	ID  int64  `path:"id"`
	Tab string `query:"tab"`
}

func (o *Order) Mount(ctx context.Context, svc *OrderService, p OrderProps) error { … }
func (o *Order) Update(ctx context.Context, svc *OrderService, p OrderProps) error { … }
```

**What `view.LiveView` gives a view:**

| | |
|---|---|
| `Connected()` | the live connection (true) or the first, HTTP render (false) |
| `Subscribe(topics…)` | its `Info` hears `view.Broadcast` to them |
| `Track(topic, key, meta)` / `Untrack` | [presence](#presence) |
| `PushPatch(href)` / `PushNavigate(href)` | move the browser after an event ([navigation](#navigation)) |
| `PutFlash(kind, msg)` / `Flash(kind)` | a message for the next render; the page's next event clears it |
| `ID()` | its id where a page embeds it, `""` for the page itself |

- **The DI instance is the template.** Each page render and each connection gets
  its own copy (embed `view.LiveView` by value, not as a pointer); `Mount`
  fills it. Without a provider for the type, it starts from its zero value.
  `Render()` is a templ method component.
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
- **Component libraries' random ids are made stable.** templUI and
  shadcn-templ give an element rendered without an `id` a random one
  (`id-` + `crypto/rand.Text()`). A live render renames each after its place on
  the page, so the same state renders the same markup: events don't churn
  those ids, tracked renders skip what didn't change, and the HTTP render
  joins the connection's. Pass an `ID` when script or CSS outside the
  component needs to find the element.
- The first request renders the page on the server; the page then connects
  (`<path>/_live`), mounts again with `Connected()` true, and reconnects if the
  socket drops, resuming its state (see [What travels](#what-travels)). Gates
  on `view.Live` apply to both.
- **Server push.** `Subscribe(topics…)` in `Mount` (it only takes effect on
  the live connection) makes the view hear `view.Broadcast(ctx, topic, data)`
  — sent from an event, a handler, a job, anywhere. Its optional
  `Info(ctx, deps…, msg view.Message) error` runs, then the page re-renders;
  with several messages waiting it handles them all and renders once. An
  embedded view subscribes on its own. `Info` is never callable from the
  browser.
- **Several replicas.** A broadcast reaches the pages on the replica that sent
  it; give the replicas a relay and it reaches all of them:

  ```go
  import "github.com/paulmanoni/nexus/extension/cache/redis/v2/viewrelay"

  nexus.Boot(view.UseRelay(viewrelay.New(viewrelay.Config{URL: os.Getenv("REDIS_URL")})), …)
  ```

  Data crosses replicas as JSON: read it in `Info` with `msg.Decode(&v)`, which
  gives the publisher's value on its own replica and the decoded JSON on the
  others. Use sticky sessions so a reconnect resumes on the replica that holds
  the page.

```go
func (b *Board) Mount(ctx context.Context, store *Store) error {
	b.Subscribe("adoptions")
	b.Adopted.Set(store.Adopted())
	return nil
}

func (b *Board) Adopt(ctx context.Context, store *Store, name string) error {
	store.SetAdopted(name, true)
	view.Broadcast(ctx, "adoptions", name) // every open board refreshes
	return nil
}

func (b *Board) Info(ctx context.Context, store *Store, msg view.Message) error {
	b.Adopted.Set(store.Adopted())
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

### Tracked state: `view.Assign`

Keep a live page's state in `view.Assign` fields and an event renders only the
parts of the page whose fields changed — LiveView's change tracking:

```go
type Orders struct {
	Status view.Assign[string]
	Rows   view.Assign[[]Order]
}

func (p *Orders) Filter(ctx context.Context, svc *OrderService, status string) error {
	p.Status.Set(status)
	p.Rows.Set(svc.List(ctx, status))
	return nil
}
```

```templ
templ (p *Orders) Render() {
	<h1>Orders · { p.Status.Get() }</h1>
	for _, o := range p.Rows.Get() {
		@OrderRow(o)
	}
}
```

`Get` reads (and, in `Render`, notes what depends on it), `Set` replaces —
setting an equal value of a comparable type is not a change — and
`Update(func(*T))` changes a slice or map in place. The compiler opens a
*spot* around each loop, branch and component call that uses none of the
template's own variables (parameters, loop variables, <code v-pre>{{ }}</code>
locals) — only the receiver, `ctx` and package-level names; on the next event
a spot whose `Assign`s didn't change is not run at all, and the browser keeps
what it showed. `view.Errors(ctx)` is tracked the same way.

- A page is tracked when its fields are all `Assign`s (signals may sit beside
  them). A page with any other field renders everything, as before — convert
  it a field at a time. Tag a field `view:"-"` when `Render` doesn't read it,
  or it doesn't change once the page is mounted (a configuration pointer,
  permissions worked out in `Mount`): it leaves the page tracked.
- Split `Render` along what changes together. A part is skipped as a whole,
  so a template that builds one view model for the whole page (`@page(p.view())`)
  re-renders everything on every event; one that renders
  `@table(p.tableView())`, `@form(p.formView())`, … — each built from the
  fields it shows — re-renders only the parts an event touched. On a
  1,000-row list, typing in its form went from ~10 ms of server work per
  keystroke to under 0.5 ms. For a page that uses `Assign`s, `nexus lsp` marks each
  other field, and each place its templates read one, as a warning;
  `nexus generate views`, `nexus dev` and `nexus doctor` print the same.
- On a tracked page, a service called from a template
  (`view.Use[*Store](ctx).Count()`) is flagged too: what it returns isn't an
  `Assign`, so the parts that show it wouldn't re-render when it changes. Load
  it into an `Assign` in `Mount`, `Info` or an event instead. (`view.Use` of a
  state struct is fine: the server never changes its signals.)
- `auth.Can` and `auth.Current` in a template need nothing: a page's identity
  is fixed for its connection, and ending a session closes the connection.
- `Render` must depend on `Assign`s alone — not on plain fields, services
  called from the template, or the clock. Under `nexus dev` and in tests every
  render that skips spots is checked against a full one: a spot that rendered
  differently is logged with its place in the template, and the full render is
  sent (`NEXUS_VIEW_VERIFY=1` turns the check on elsewhere, `0` off).
- `Set` and `Update` belong to the page's own goroutine: `Mount`, `Params`,
  `Info` and events.

### Embedding a live view

Any live view can be placed in another's template, by an id unique among its
type's there — Phoenix's LiveComponents and nested LiveViews in one:

```go
type Cart struct {
	view.LiveView
	Items view.Assign[[]Item]
	Open  view.Assign[bool]
}

type CartProps struct{ User string }

func (c *Cart) Mount(ctx context.Context, svc *CartService, p CartProps) error {
	c.Items.Set(svc.Items(ctx, p.User))
	c.Subscribe("cart:" + p.User) // an embedded view subscribes on its own
	return nil
}

func (c *Cart) Toggle(ctx context.Context) error {
	c.Open.Update(func(o *bool) { *o = !*o })
	return nil
}
```

```templ
@view.Component[*Cart]("cart", CartProps{User: p.User.Get()})
```

- **No registration** for a view that takes no dependencies. One that does
  needs its dependencies hooked up once: `view.Live[*Cart]("")` (no path:
  embed only) or `//nexus:live` with no path on the type.
- **State per placement.** The page keeps one instance per id while its
  template renders it and drops one it stops rendering — its subscriptions and
  presence with it; rendered again, it mounts afresh.
- **Props.** `Mount` runs on the first render; when the page renders it with
  different props, `Update` runs (or `Mount` again). A page event or `Info`
  can hand it new props with `view.UpdateComponent[*Cart](ctx, "cart", props)`.
- **Events reach their own instance.** `view.Send(c.Toggle)` (and `Submit`,
  `Change`) in its template goes to the instance it was clicked in — no target
  to name. It runs behind the page's gates.
- **Only what changed renders.** With `Assign` fields, its event runs its
  changed parts and the page spots it sits in; the rest of the page, and the
  other views, are skipped. One the page re-renders with the same props, and
  nothing changed, is skipped as a whole.
- It renders inside `<nx-c data-nx-c="id">` (laid out as if it weren't there).

**When to embed a live view, and when not.** A templ component is the default
— with `Assign` fields a page already re-renders only the parts that changed,
so embedding buys no speed. Embed a live view when a part needs **its own
state and events**, and appears more than once or on more than one page: a
cart in every page's header, an editable row repeated fifty times.

### Uploads

A `view.Upload` field is a file input of a live page (or live component),
LiveView's `allow_upload`: files go to the server as soon as they're chosen,
the page renders their progress, and an event takes them when the form is
submitted.

```go
type Profile struct {
	Name   view.Assign[string]
	Avatar view.Upload
}

func (p *Profile) Mount(ctx context.Context) error {
	p.Avatar.Allow(view.UploadConfig{Accept: []string{"image/*"}, MaxEntries: 1, MaxSize: 5 << 20})
	return nil
}

func (p *Profile) Save(ctx context.Context, disk *Uploads, in ProfileForm) error {
	return p.Avatar.Consume(func(e view.UploadEntry, f *os.File) error {
		return disk.Put(ctx, "avatars/"+e.Name, f)
	})
}
```

```templ
<form onsubmit={ view.Submit(p.Save) }>
	<input type="file" { p.Avatar.Input()... }/>
	for _, e := range p.Avatar.Entries() {
		<p>
			{ e.Name } { strconv.Itoa(e.Progress) }%
			if e.Err != "" {
				<span class="error">{ e.Err }</span>
			}
			<button type="button" onclick={ view.CancelUpload(&p.Avatar, e.Ref) }>×</button>
		</p>
	}
	<button disabled?={ p.Avatar.Busy() }>Save</button>
</form>
```

- **Checked on choice.** `Accept` (extensions or MIME types, `image/*`),
  `MaxEntries` (default 1; a single-file input replaces its file) and `MaxSize`
  (default 8 MiB). Each file becomes an entry; a refused one has `Err` set.
- **Sent at once, one HTTP request per file**, to the page's own route — its
  gates apply, the request must come from the user who opened the page, and
  its URL works once. The bytes stream to a temporary file; `Progress`,
  `Received` and `Done` follow and the page re-renders as they do. The app's
  body limit doesn't apply; `MaxSize` does.
- **`Consume(fn)`** hands each finished file to `fn`, open for reading, then
  deletes it and its entry. `view.CancelUpload(&p.Avatar, ref)` stops one and
  removes it. What's left when the page ends is deleted.
- An `Upload` is page state like an `Assign`: it doesn't keep a page from
  tracking its changes, and only the parts that show its entries re-render.

### Streams

A `view.Stream[T]` field shows a list the server doesn't keep — a chat, a
feed, a log of thousands of rows — LiveView's streams. The page sends what
changed; the browser keeps the rest.

```go
type Chat struct {
	view.LiveView
	Messages view.Stream[Message]
}

func (c *Chat) Mount(ctx context.Context, store *Store) error {
	c.Messages.Configure(func(m Message) string { return "msg-" + m.ID })
	c.Messages.Limit(-200) // the browser keeps the last 200
	c.Messages.Reset(store.Recent(50)...)
	c.Subscribe("chat")
	return nil
}

func (c *Chat) Info(ctx context.Context, msg view.Message) error {
	var m Message
	if err := msg.Decode(&m); err != nil {
		return err
	}
	c.Messages.Insert(m)
	return nil
}
```

```templ
<ul { c.Messages.Attrs()... }>
	for _, m := range c.Messages.Items() {
		<li id={ c.Messages.ID(m) }>{ m.Text }</li>
	}
</ul>
```

- `Insert` adds at the end, `Prepend` at the start, `InsertAt(i, item)` at an
  index; an item whose id is already shown is replaced in place. `Delete` /
  `DeleteID` remove one, `Reset` empties the list and shows new items, and
  `Limit(n)` bounds what the browser keeps (positive: the first n, negative:
  the last -n).
- `Items()` are the items of the latest change only — render each as a direct
  child of the element `Attrs()` is spread on, with its `ID`. The server holds
  that one change; the browser applies it once and keeps the rows it has.
- A `Stream` is page state like an `Assign`: a tracked page skips the list
  when it didn't change.

### Presence

Who is on a topic — the users on a page, the people in a room — as Phoenix
Presence tracks it:

```go
func (r *Room) Mount(ctx context.Context, p RoomProps) error {
	me := auth.Current(ctx)
	r.Subscribe("room:" + p.Name)
	r.Track("room:"+p.Name, me.ID, Seen{Name: me.User.(*User).Name})
	r.Online.Set(view.Presences("room:" + p.Name))
	return nil
}

func (r *Room) Info(ctx context.Context, msg view.Message) error {
	if _, ok := msg.Data.(view.PresenceDiff); ok {
		r.Online.Set(view.Presences(msg.Topic))
	}
	return nil
}
```

- `Track(topic, key, meta)` makes the page present as `key` while the view is
  shown (from the connected `Mount` on; the first, HTTP render tracks nothing).
  The same key can be present more than once — two tabs — each with its own
  meta; `Untrack(topic, key)` ends one early.
- `view.Presences(topic)` lists who is there: a `view.Presence{Key, Metas}`
  per key, sorted by key; `meta.Decode(&v)` reads a meta.
- Pages subscribed to the topic get a `view.PresenceDiff{Joins, Leaves}`
  through `Info` when keys join or leave.
- A page leaves when it ends: its connection closes and the reconnect grace
  (`view.ResumeGrace`) runs out, or it navigates away.
- **Across replicas** with a relay (`view.UseRelay`): joins and leaves travel
  to the other replicas, each restates its presences every 10 seconds (and to
  a replica that starts), and a replica not heard from for `view.PresenceTTL`
  (30s) is taken to have left with everyone it had.

### Coming from Phoenix LiveView

| LiveView | nexus |
|---|---|
| `defmodule MyAppWeb.OrdersLive` + `live "/orders", OrdersLive` | a struct + `view.Live[*Orders]("/orders", gates…)` |
| `use Phoenix.LiveView` | embed `view.LiveView` |
| `mount(params, session, socket)` | `Mount(ctx, deps…, props) error`; props bound from `path:`/`query:` tags |
| `handle_params(params, uri, socket)` | `Update(ctx, deps…, props) error` on a patch |
| `handle_event("cancel", params, socket)` + `phx-click="cancel"` | `Cancel(ctx, deps…, args…) error` + `onclick={ view.Send(p.Cancel, id) }` |
| `phx-submit` / `phx-change` | `view.Submit(p.Save)` / `view.Change(p.Validate)` with a `form:`-tagged struct |
| `handle_info(msg, socket)` + `Phoenix.PubSub.subscribe` | `Info(ctx, deps…, msg view.Message) error` + `v.Subscribe(topic)`, `view.Broadcast` |
| PubSub across nodes | `view.UseRelay(viewrelay.New(…))` (Redis) |
| `assign(socket, :rows, rows)` / `socket.assigns.rows` | `p.Rows.Set(rows)` / `p.Rows.Get()` on a `view.Assign` field |
| `update(socket, :count, &(&1 + 1))` | `p.Count.Update(func(n *int) { *n++ })` |
| change tracking (only changed assigns re-render) | the same, for pages whose fields are all `Assign`s |
| `render(assigns)` + HEEx | `templ (p *Orders) Render()` |
| function components | templ components |
| `live_component` + `phx-target={@myself}` | `@view.Component[*Cart](id, props)` — events reach their instance without a target |
| `update(assigns, socket)` / `send_update` | `Update(ctx, deps…, props) error` / `view.UpdateComponent[*Cart](ctx, id, props)` |
| `push_patch` / `push_navigate` / `<.link patch navigate>` | `v.PushPatch` / `v.PushNavigate` / `@view.Link(href)` |
| `put_flash` / `@flash` | `v.PutFlash(kind, msg)` / `v.Flash(kind)` |
| `phx-update="ignore"` | `data-nx-ignore` on an element with an `id` |
| `JS.show/hide/toggle/add_class/set_attr/focus/push` | `view.JS(view.Show(…).AddClass(…).Push(p.Save))` — see [JS commands](#js-commands) |
| `push_event(socket, "saved", payload)` / `JS.exec` from the server | `p.PushEvent("saved", payload)` / `p.PushJS(ops…)` |
| JS hooks | islands (Vue/React/TS components), `view.State` signals for browser-only state |
| form recovery on reconnect | the same (`view.Change` forms are re-sent) |
| — | **resume**: a reconnect within `view.ResumeGrace` keeps the page's state |
| `Phoenix.LiveViewTest` | `viewtest.Mount` (the real runtime in Go), `viewtest.Browser` (Chrome) |
| `allow_upload` / `live_file_input` / `consume_uploaded_entries` | a `view.Upload` field / `{ p.Avatar.Input()... }` / `p.Avatar.Consume(fn)` |
| `stream(socket, :messages, items)` / `phx-update="stream"` | a `view.Stream[T]` field: `Insert`/`Prepend`/`Delete`/`Reset`, `{ c.Messages.Attrs()... }` |
| `Phoenix.Presence.track` / `list` / `presence_diff` | `v.Track(topic, key, meta)` / `view.Presences(topic)` / `view.PresenceDiff` in `Info` |

### Context processors

Some values every page shows — the signed-in user's name, unread counts, the
navigation — come from the server on each request. Rather than each page
loading them and passing them down through every component, register a
context processor once, and read its value from any template:

```go
type Layout struct {
	User   string
	Unread int
}

view.ContextProcessor(func(ctx context.Context, inbox *Inbox) (Layout, error) {
	u := auth.Current(ctx)
	return Layout{User: u.ID, Unread: inbox.Unread(ctx, u.ID)}, nil
})
```

```templ
templ topbar() {
	{{ l := view.FromContext[Layout](ctx) }}
	<span>{ l.User }</span> <span class="badge">{ strconv.Itoa(l.Unread) }</span>
}
```

A processor that depends on the page reads `view.CurrentURL(ctx)` — the
URL of the page being rendered (a live page's as the browser shows it, kept
current as `view.Link` or `PushPatch` patch it) — so breadcrumbs or the active
menu entry can come from one place:

```go
view.ContextProcessor(func(ctx context.Context) Breadcrumbs {
	return breadcrumbsFor(view.CurrentURL(ctx).Path)
})
```

The processor takes `ctx` — the request's, or a live page's connection's,
with the visitor's identity — then any dependencies from the app's DI
container, and returns the value, with or without an error. Its result type
names it: one processor per type. It runs only when a template reads it, and
at most once per render however many components read it; on a live page,
each event's render runs it again, and a part of the page that reads it is
never skipped by change tracking. An error fails the render, as does reading
a type no processor returns.

### JS commands

Some of what a click does needs no server: opening a menu, showing a step,
marking a row. `view.JS` runs commands in the browser, in order — Phoenix
LiveView's `JS` — and is a script like `view.Send`, so it goes in any `on*`
attribute (and in a component library's `templ.Attributes`). Commands chain,
each step a method of the one before:

```templ
<button onclick={ view.JS(view.Show("#confirm").FocusFirst("#confirm")) }>Delete</button>
<div id="confirm" hidden>
	Delete { p.Name }?
	<button onclick={ view.JS(view.Hide("#confirm").Push(p.Delete, p.ID)) }>Yes</button>
</div>
```

A chain is a value: extending it never changes it, so steps shared by several
buttons are built once —

```go
closing := view.Hide("#confirm").PopFocus()
yes := closing.Push(p.Delete, p.ID)   // closing itself is unchanged
no := closing
```

— and `a.Then(b, c)` joins chains. `view.JS(a, b)` with separate commands
still works and runs the same as `a.Then(b)`; a `[]view.JSOp` built in code
spreads into `view.JS(ops...)`.

| Command | |
|---|---|
| `Show(sel, opts…)` / `Hide(sel, …)` / `Toggle(sel, …)` | drops `hidden` and `display: none` / sets `display: none`; `view.Display("flex")` for the display Show gives |
| `AddClass(classes, sel, …)` / `RemoveClass` / `ToggleClass` | space-separated classes |
| `SetAttr(name, value, sel, …)` / `RemoveAttr(name, sel, …)` / `ToggleAttr(name, value, sel, …)` | attributes — `aria-expanded`, `open`, `disabled` |
| `Focus(sel, …)` / `FocusFirst(sel, …)` | the element / its first focusable descendant |
| `Push(p.Method, args…)` / `PushTo(p, "Method", args…)` | `view.Send` / `view.SendTo` as a step |
| `Transition(classes, sel, …)` | adds classes for a while (`view.Time`, 200ms by default) — a shake, a flash |
| `Confirm(message)` | asks with the browser's confirm dialog; the commands after it run only on yes — `view.Confirm("Delete it?").Push(p.Delete, id)` |
| `SetValue(value, sel, …)` | sets a field's value as if typed: its `input` and `change` fire, so a `view.Change` or live form hears it — clearing a search box |
| `Copy(sel, …)` / `CopyText(text)` | puts a field's value (else the element's text) or `text` on the clipboard; the clicked element carries `data-copied` for a moment |
| `ScrollTo(sel, …)` | scrolls the element into view |
| `SetCookie(name, value, view.MaxAge(d))` / `Reload()` | sets a cookie the server reads on the next request (`Path=/`, `SameSite=Lax`, `Secure` on https; a session cookie without `MaxAge`, deleted with `MaxAge(0)`), and loads the page again — a language or theme switch: `view.SetCookie("lang", "sw", view.MaxAge(365*24*time.Hour)).Reload()`. Page scripts can read such a cookie: never a secret |
| `PushFocus(sel, …)` / `PopFocus()` | remembers an element (`view.This()`: the one the event is on) / focuses the last remembered |
| `If(cond)` / `ElseIf(cond)` / `Else()` | run the steps after them only when the condition holds — see [Conditions](#conditions) |
| `Debounce(d)` / `Throttle(d)` | time the steps after them — see [Debounce and throttle](#busy-buttons-behaviors-and-when-to-write-javascript) |
| `Exec(attr, sel, …)` | runs the commands held in an attribute — `view.Commands(…)`, or any live script such as `view.Send` |
| `Dispatch(event, sel, …)` | fires a `CustomEvent` (`view.Detail(v)`, `view.NoBubble()`) — for islands and scripts |

`view.Animate(during, from, to)` makes `Show`, `Hide` and `Toggle` a
transition: the element carries `during` throughout, starts at `from` and ends
at `to`, for `view.Time(d)` (200ms) — Tailwind classes work as they are:

```go
fade := view.Animate("transition-opacity duration-200", "opacity-0", "opacity-100")
view.JS(view.Show("#toast", fade))
```

A transition's classes are the browser's own — they leave with it and are not
kept across re-renders — and a new one on an element ends the one running.

A dialog keeps its closing steps once, so its close button, a Cancel and
anything else that closes it share them:

```templ
<button onclick={ view.JS(view.PushFocus(view.This()).Show("#confirm").FocusFirst("#confirm")) }>Delete</button>
<div id="confirm" hidden data-cancel={ view.Commands(view.Hide("#confirm"), view.PopFocus()) }>
	<button onclick={ view.JS(view.Exec("data-cancel", "#confirm")) }>Cancel</button>
</div>
```

From the server, an event (or `Info`, or `Mount` once connected) pushes
commands and events that run once its reply is applied:

```go
func (p *Orders) Save(ctx context.Context, f OrderForm) error {
	// … save …
	p.PushJS(view.Hide("#order-dialog"), view.Transition("flash", "#row-"+f.ID))
	p.PushEvent("orders:saved", map[string]any{"id": f.ID}) // window.addEventListener("orders:saved", …)
	return nil
}
```

In a pushed command `view.This()` is the page's live root. An event that fails
sends none of what it pushed, and the first, server-rendered load drops them —
there is no browser to run them in yet. `viewtest`'s `p.Eval(js)` listens for a
pushed event.

A command's target is a CSS selector, matching anywhere on the page, or
`view.This()` — the element the event is on (JavaScript's
`this`): `view.AddClass("is-on", view.This())`. `view.Closest()` makes a selector the nearest matching ancestor (a row's own row:
`view.ToggleClass("picked", "tr", view.Closest())`), `view.Inner()` matches
inside the element only, and `view.Within(container)` matches inside the
element's nearest `container` — an accordion section's own panel:
`view.ToggleClass("show", ".panel", view.Within(".section"))`. `FocusFirst`
prefers an `[autofocus]` field and skips anything marked `data-nx-nofocus`
(the kit marks dialogs' close buttons).

**What a command changes survives re-renders.** The page remembers, per
element, each attribute a command changed and what the server had rendered
there. A re-render that renders that attribute as before gets the command's
value back, so an event that updates a list doesn't close the menu above it;
one that renders it differently wins, and the attribute is the server's again
(as a form field takes the server's value when it changes). Show and Hide write
the `style` attribute's `display`, and Show removes `hidden`; both are kept the
same way. In tests, `viewtest` runs the commands: `Expect(loc).Visible()`,
`Hidden()`, `HasClass(c)`, `NoClass(c)`, `Attr`, `Focused()`.

### Conditions

`If`, `ElseIf` and `Else` are chain steps too: what follows an `If` runs only
when its condition holds, up to the next `ElseIf` or `Else`. A condition is
about an [element](#elements-view-el-and-view-this) — `view.El(sel)` or
`view.This()`, found as a target is (scoped with `view.Closest()`,
`view.Inner()` or `view.Within()`):

| Condition | Holds when |
|---|---|
| `view.El("#agree").Checked()` | it is checked |
| `view.El("#q").Value()` | its value isn't empty |
| `view.El("#m").Attr("open")` | it has the attribute |
| `view.This().Attr("aria-pressed").Eq("true")` | the read equals the text — an attribute's value (one it lacks never equals), a field's value, `Checked()` as `"true"`/`"false"` |
| `view.This().Is(":invalid")` | it matches the CSS selector |
| `view.El(".row.selected")` | the selector matches anything |

```templ
<button onclick={ view.JS(view.If(view.El("#agree").Checked()).Push(p.Continue).Else().Transition("shake", view.This())) }>Continue</button>
<input type="checkbox" onchange={ view.JS(view.If(view.This().Checked()).Show("#extra").Else().Hide("#extra")) }/>
<button onclick={ view.JS(view.If(view.El("#email").Is(":placeholder-shown")).Focus("#email").
	ElseIf(view.El("#email").Is(":invalid")).AddClass("error", "#email").
	Else().Push(p.Invite)) }>Invite</button>
```

A toggle button flips its own state:

```templ
<button aria-pressed="false" onclick={ view.JS(view.If(view.This().Attr("aria-pressed").Eq("true")).
	SetAttr("aria-pressed", "false", view.This()).
	Else().SetAttr("aria-pressed", "true", view.This())) }>Bold</button>
```

`Is` takes any CSS selector, so the rest needs nothing new: `:checked`,
`:placeholder-shown` (empty), `:valid`/`:invalid`, `:focus-within`, `:empty`,
`[open]`, `[hidden]`, `.is-open`, `:not(…)`. A few rules:

- A condition reads the page when the chain reaches it — after a `Debounce`,
  once the pause is over. It never sees a reply: `Push` sends and moves on, so
  what depends on the server's answer is pushed back by the event (`PushJS`).
- An `If` reaches to the end of the chain, or its `ElseIf`/`Else`. Steps that
  always run go before it. An `If` inside a branch takes the `ElseIf` and
  `Else` after it.
- What the server knows (permissions, a dirty record) is a Go `if` choosing
  which chain to render; what only the browser knows (ticked, typed, open,
  focused) is a condition.

### Configuration in the browser

A page's own script sometimes needs a setting from nexus.toml — a currency, a
support address, the environment. List the keys the browser may see; nothing
else leaves the server:

```toml
[runtime.browser]
config = ["shop.currency", "support.email", "runtime.environment"]
```

```js
__nx.config("shop.currency")   // "TZS"
__nx.config()                  // every listed key → its value
```

`view.Script()` puts the values in the page (as JSON in a script element of
its own, escaped so a value can't end it), with environment overrides applied
as `config.Get` applies them. A key is checked when the app boots, and boot
fails on one under `[databases]`, `[secrets]` or `[extensions]`, one named
like a password, secret, token, key or credential (`api_key`, `password_min`,
`smtp.token`), one that names nothing, or a whole table — list its values one
by one. Templates read configuration with `config.Get` as any Go code does;
`__nx.config` is for script.

### Busy buttons, behaviors, and when to write JavaScript

**A button that waits for its reply.** Mark any element whose event goes to
the server with `data-nx-busy`; from the click until that event's reply it
carries `aria-busy="true"`, ignores further clicks, and a re-render meanwhile
leaves the marker alone:

```templ
<button class="group" data-nx-busy onclick={ view.Send(p.FetchOrders) }>
	<span class="group-aria-busy:hidden">Fetch orders</span>
	<span class="hidden animate-spin group-aria-busy:inline-block">…</span>
</button>
```

Style it with CSS keyed to the element itself (`group-aria-busy:`, or
`[aria-busy=true]`) — not to an ancestor: the live page's root is busy
during every event. There is no script to write. A `view.JS(… .Push(…))`
chain on such an element counts too; a form being submitted is busy the
same way, and so is its `data-nx-busy` submit button; a dropped connection
releases every element whose reply can no longer come.

**Debounce and throttle.** `Debounce(d)` and `Throttle(d)` are chain steps:
they time the steps after them, as `Confirm` gates them, and steps before run
at once:

```templ
<input oninput={ view.JS(view.Debounce(300*time.Millisecond).Push(p.Search, view.This().Value())) }/>
<div onscroll={ view.JS(view.Throttle(200*time.Millisecond).Push(p.Seen)) }>…</div>
<button onclick={ view.JS(view.AddClass("is-saving", view.This()).Debounce(time.Second).Push(p.Save)) }>Save</button>
```

`Debounce` runs the rest once the event has stopped firing for `d` (each new
event restarts the wait). `Throttle` runs it for the first event at once and
drops the rest within `d` — except a field's last `input`/`change`, which
still runs when `d` is up, so the final value is never lost. A debounced chain
still waiting inside a form runs before that form's submit. `view.Send`,
`view.Change` and `view.Submit` are the one-event shortcuts — `view.Change`
already waits 150ms for typing to pause, and a form being submitted ignores a
second submit; anything more (timing, a confirm, several steps) is a
`view.JS` chain.

### Elements: `view.El` and `view.This`

A `view.Element` is an element of the page named by a CSS selector. A
selector written in place is one (`view.Show("#menu")`); `view.El(sel)` makes
one from a string built at render time (`view.Show(view.El("#row-" + id))`),
and `view.This()` is the element the event is on — JavaScript's `this` (in a
command pushed from the server, the live root). The same element is a
command's target, a [condition](#conditions), and a value read as the event is
sent — event arguments are otherwise fixed when the page renders:

```templ
<input oninput={ view.JS(view.Debounce(300*time.Millisecond).Push(p.Search, view.This().Value())) }/>
<input type="checkbox" onchange={ view.Send(p.Toggle, row.ID, view.This().Checked()) }/>
<button data-id={ row.ID } onclick={ view.Send(p.Open, view.This().Attr("data-id")) }>Open</button>
<button onclick={ view.Send(p.Search, view.El("#q").Value()) }>Search</button>
```

`Value()` is a string, `Checked()` a bool, `Attr(name)` a string (null when
missing, so a pointer parameter is nil); a number parameter takes a string
that holds one. A read of `view.El(sel)` reads the first element `sel`
matches. Reads work wherever arguments go — `view.Send`, `view.SendTo`,
`view.Push`. On a form's `oninput`, `this` is the form, not the field typed
into: send a form's fields with `view.Change`.

**Behaviors.** `@view.Behaviors()` (after `@view.Script()`; the kit's
`ui.Script()` includes it) adds what markup asks for with `data-nx`
attributes — no styling of its own, so any design system uses it:

| Attribute | |
|---|---|
| `data-nx-filter="<selector>"` on a text box | hides, as it is typed in, the `[data-nx-filter-item]` elements inside the selector's match whose text doesn't contain it (`data-nx-filter-item="<text>"` matches that text instead); a `[data-nx-filter-empty]` element shows when none is left. Hidden checkboxes stay checked and are still sent |
| `data-nx-check-all` on a checkbox | checks or clears the other boxes of its group (nearest `[data-nx-checks]`, else its form) before the form's own `view.Change` reads them; shows partly checked; `[data-nx-checked-count]` shows how many |
| `data-nx-valid` on a form | its submit buttons are enabled only while the fields pass the browser's own checks (`required`, `minlength`, `pattern`, `type=email`) |

Each is applied again after a live re-render, so a filtered list stays
filtered when the server renders it anew.

**Which tool, in order.** Reach for the first that does the job:

1. **A server event** (`view.Send`, `view.Submit`, a live form): state the
   server owns.
2. **A JS command** (`view.JS`): what a click does to the page — show, hide,
   classes, attributes, focus, a confirm, a copy — with at most one push.
3. **A behavior** (`data-nx-*`): a small, stateless reaction to typing or
   checking that no event should round-trip for.
4. **An island or your own script**: a widget with state of its own — a
   chart, an editor, a clock. Keep it thin: let it handle the DOM events it
   must and hand back to commands with
   `__nx.js(el, event, [["push", {event: "Drop", args: [id]}]])` — or, simpler,
   `el.click()` on an element whose `onclick` is the `view.JS` chain — rather
   than growing state of its own.

### Forms: `view.Form`

A form is one declaration — a struct embedding `view.Form`, as a Django form
class is the whole form. Tags say what the fields are and how they show;
methods on the struct complete it:

```go
type UserForm struct {
	view.Form
	Name   string `form:"name" label:"Full name" validate:"required" span:"6"`
	Email  string `form:"email" validate:"required,email" span:"6"`
	Bio    string `form:"bio" input:"textarea" help:"Shown on the profile."`
	RoleID uint   `form:"roleId" label:"Role"`
	Active bool   `form:"active" input:"switch"`
}

// Choices — Django's ModelChoiceField: the field renders as a select. The
// receiver is the form's current values, so choices may depend on other
// fields; services come via view.Use.
func (f UserForm) RoleIDChoices(ctx context.Context) []view.Choice {
	return view.Use[*RoleService](ctx).Choices(ctx)
}

// Validate — Django's clean(): runs after the tag rules, as the user types
// on a live form, and always before the submit.
func (f UserForm) Validate(ctx context.Context) error {
	if f.Name == f.Email {
		return nexus.Invalid().Field("email", "an e-mail is not a name")
	}
	return nil
}
```

| Tag | |
|---|---|
| `form:"name"` | the field's name; fields without one are skipped |
| `validate:"…"` | the rules; `required` also marks the label |
| `label:` / `help:` / `placeholder:` | how it shows; the label defaults to the Go name, spaced (`FirstName` → First Name) |
| `span:"6"` | width in a 12-column row (`AllFields`) |
| `input:"password"` | the control, where the Go type isn't enough: `password`, `textarea`, `switch`, `hidden`, any input type |

Without an `input:` tag the Go type decides: `bool` a checkbox, `time.Time` a
date, numbers a number input, `validate:"email"` an email one, a slice a
multi-select, and a field with choices a select.

The page holds the form — a field, or embedded when it has one — and the
struct's fields are the values, read and written directly. The submit method
**only runs when every rule passes**, the form's `Validate` included:

```go
type Users struct {
	view.LiveView
	Edit UserForm // or embedded: UserForm
}

func (p *Users) Open(ctx context.Context, id uint) error {
	p.Edit.Load(userRecord(id)) // copies the matching fields; or assign them: p.Edit.Name = …
	return nil
}
func (p *Users) Save(ctx context.Context, f UserForm) error { … } // f passed every rule
```

```templ
@ui.Form(p.Edit, p.Save)
```

Two more optional methods move the page's work onto the form itself:
`Init` — Django's `initial` — runs once, before the page's `Mount`, and
`Save` takes the submit when `ui.Form` is given no method:

```go
func (f *UserForm) Init(ctx context.Context) { f.Active = true }
func (f *UserForm) Save(ctx context.Context) error {
	return view.Use[*UserService](ctx).Save(ctx, f)
}
```
```templ
@ui.Form(p.Edit)
```

A childless `ui.Form` renders every field from the struct — spans honored —
and a Save button, like `{{ form }}`. Children take over the layout, and
`ui.Field` renders one field by name, with overrides where the struct's
defaults aren't enough:

```templ
@ui.Form(p.Edit, p.Save, ui.Live) {
	@ui.Field("name")
	@ui.Field("email", ui.Label("Work e-mail"))
	@ui.Field("roleId", ui.Options(extraRoles))   // instead of the form's choices
	@ui.Submit("Create user")
}
```

A misspelled `p.Edit.F("emial")` fails `nexus generate views` with a
did-you-mean when the form's struct is in the page's package; `ui.Field` and
`F` name an unknown field in their error either way.

**Liveness is graded.** With nothing, the form posts on submit and errors
show then — no traffic while typing. `ui.Live` checks the values as the user
types: each change is laid over the form's values and re-checked, and a
field's error appears once the user leaves it (every field's after a
submit). A change **method** does the same and then runs — dependent fields
in plain code:

```templ
@ui.Form(p.Edit, p.Save, p.Recalc)
```
```go
func (p *Users) Recalc(ctx context.Context, f UserForm) error {
	if p.Edit.Changed("employerId") {
		p.Edit.Update(func() { p.Edit.DesignationID = 0 })
	}
	return nil
}
```

The form's fields hold the loaded values under what the user typed, so a
field the page doesn't render is never zeroed, an unticked checkbox arrives
as false, and choices methods see the current values (a `BreedChoices`
reading `f.Species` is a dependent select with no code).

The rest of the behaviour: a successful submit resets the form to the values
it was loaded with; `Load(v)` and `Reset()` replace what every field shows,
even what the user typed; `Update(func() { … })` is a change of the page's
own (validated, shown to the user); a busy form drops a second submit and
`ui.Submit`'s button spins; `Dirty()` reports unsaved changes (the element
carries `data-nx-dirty`) and `view.ConfirmLeave(msg)` asks before leaving
with them; `Valid()`, `Submitted()`, `Error()` (the form-wide message) and
`F(name)` read the rest. Forms share field blocks by embedding structs, and
`view.RenderForm(f, extras…)` is the kit-free form element for markup of
your own. A plain-HTTP form renders its CSRF token with `@view.CSRF()`.

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
A form that must work without JavaScript renders the field itself with
`@view.CSRF()`. See [Web security](./security#csrf).

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

From a live page, a link moves over the page's socket, as Phoenix LiveView's
`patch` and `navigate` do — no HTTP request:

- **The same page with another query** (`/orders?page=2`) is a *patch*: the
  page's props are bound from the new URL, its `Update` runs with them (or
  `Mount` again, without one), and the page sends the change to its tree:

  ```go
  type OrdersProps struct {
      Page int `query:"page"`
  }

  func (o *Orders) Update(ctx context.Context, svc *OrderService, p OrdersProps) error {
      return o.load(ctx, svc, p.Page)
  }
  ```

- **Another live page** is opened on the same connection: the old page ends,
  the new one is mounted through its own route — its gates, DI and path
  parameters as for a page load — and sends the change from the page the
  browser holds. Two pages of one layout share its frames, so what travels is
  what differs: the title, the content, a menu's current item. The title and
  new stylesheets/scripts are merged in. (A page that came over HTTP fetches
  its tree when its socket is first quiet, so its first navigation is a
  change too.)
- **Anything else** — a page that isn't live, or one whose gates refuse — the
  browser loads as above.

An event moves the browser the same way with the view's `PushPatch(href)` or
`PushNavigate(href)`. The back and forward buttons go over the
socket too. After every navigation the runtime fires `nx:navigate` on
`window`, for a page's own scripts (a menu marking the current page).

### What travels

- **The first render comes over HTTP, once.** The socket that connects next
  mounts, renders and compares with what the page already has: identical, and
  nothing is sent; different (a `Mount` that does more once `Connected()`),
  and the page is corrected. The first change carries the render the browser
  patches against from then on.
- **Updates send what changed, as a render tree** — Phoenix LiveView's model.
  The view compiler has the code templ generates record each render as a tree:
  the markup a template always writes (its *statics*) apart from what it fills
  in (its *dynamics*), each `for` loop as a list of item frames, each `if` /
  `switch` branch and each component a template renders as a frame of its own.
  A connection is sent a frame's statics once, under an id, and from then on
  only the dynamics that changed: `{"u": {"3": "4"}}` is "the fourth dynamic is
  now 4". A loop's change keeps the items that stayed, inserts the new ones and
  changes the rest in place; a long piece of markup the page shows again (a
  button whose classes a component computed) travels once and is then named
  by an id. On the example board, adopting a pet the second time sends about
  170 B. Markup written by code the compiler didn't see — a hand-written
  `templ.ComponentFunc`, a library compiled by plain `templ generate` — is one
  dynamic, sent as a token patch when it is long and changed a little; run
  `go run github.com/paulmanoni/nexus/v2/view/viewgen/cmd/instrument <dir>` on
  such `_templ.go` files to give them statics too (the `view/ui` kit is).

  Messages are compressed (`permessage-deflate`). The browser keeps the tree,
  applies each change, renders it back to markup and morphs the page; one that
  loses track asks for the whole tree.
- **What a page costs.** Each connected page runs on one goroutine (plus its
  socket's reader) and keeps its state, a compact shadow of its last tree to
  diff against, and its socket; a page idle for `view.LiveIdleTrim` (2
  minutes) lets the shadow go and its next reply is the whole tree. Set
  `GOMEMLIMIT` in production to keep the heap's headroom bounded. Measured
  numbers are under [Performance](#performance).
- **Reconnects resume.** A dropped socket retries with jittered backoff (at
  once when the network returns or the tab is looked at again); the page shows
  `data-nx-live-state="disconnected"` meanwhile — style it. The server keeps
  the page — its state and its subscriptions — for `view.ResumeGrace` (30s)
  under a token the browser holds; a reconnect for the same page and the same
  signed-in user carries on with it, and events sent while disconnected are
  queued and delivered then. When the state is gone (the grace ran out, the
  server restarted), the page mounts afresh — and the browser first sends each
  form that validates as it is typed into (`view.Change`) back to its event,
  before anything queued, holding the page as it is until those replies
  arrive: an event that reopens what the form belongs to from its fields (a
  hidden id) gets back what the user typed, as LiveView's form recovery does.
  Browser-side signals keep their values throughout.

## Performance

Measured on one 10-core machine (nexus v2.18.0), each page a live view over
its own WebSocket, the load generator on the same machine. A page of a
200-row table and a form:

| | Plain fields | `view.Assign` | Table as an embedded view |
|---|---|---|---|
| Memory per open page | 232 KB | 229 KB | 249 KB |
| Typing in the form, 200 pages at full speed | 6,100 events/s · p50 6 ms | **102,600 events/s · p50 1 ms** | **97,600 events/s · p50 1 ms** |
| Changing the table, 200 pages at full speed | 6,060 events/s | 6,230 events/s | 5,370 events/s |
| 10,000 users, one event every ~5 s | p99 13 ms · CPU ~300% | **p99 11 ms · CPU ~30%** | **p99 11 ms · CPU ~30%** |

`view.Assign` doesn't make the page cheaper to hold; it makes an event that
changes a small part cheap, because the rest isn't rendered. When the table
itself changes, all three do the same work.

A chat of 1,000 messages, as a plain list in an `Assign` and as a
[stream](#streams):

| | Plain list | `view.Stream` |
|---|---|---|
| Memory per open page | 911 KB | **143 KB** |
| Posting, 200 pages at full speed | 2,160 events/s | **98,700 events/s** |
| 5,000 users posting every ~5 s | saturated: p50 12 s | **p99 4.7 ms · CPU ~20%** |
| One broadcast reaching 2,000 pages | 0.9 s | **23–34 ms** |

[Presence](#presence): a join reaching every page in its room — 50 pages
4 ms, 500 pages 65 ms, 2,000 pages 0.8 s. A room where every page lists every
other is quadratic in its size (2,000 pages listing 2,000 names took 4 GB);
show a count, or a page of the list, in a large one.

v2.18.0 (one kind of live view) measures level with v2.17.0 on every test
above, within run-to-run noise.

## The toolchain

| Command | Views | Tailwind |
| --- | --- | --- |
| `nexus dev` | compiled on start and on every `.templ` save, written beside the `.templ` files; `--no-view-files` keeps them in memory, for the build overlay | `tailwindcss --watch` |
| `nexus build` | compiled through the build overlay — nothing written | built once, minified |
| `nexus test` / `nexus vet` | `go test` / `go vet` through the same overlay | — |
| `nexus lsp` | handed to gopls as editor buffers (see [Editor support](#editor-support)) | — |
| `nexus generate views [--check]` | written to disk / verified (CI) | — |

The generated Go (`*_templ.go`, `view_gen.go`, `view_imports_gen.go`) never
needs to be committed: every nexus command compiles it through an overlay.
`nexus dev` writes it beside the `.templ` files by default, so any editor and a
plain `go build`/`go test` see it — gitignore it (`nexus new` does, and `nexus
dev` says when a project doesn't). `nexus dev --no-view-files` keeps it in
memory; the editor then gets it from `nexus lsp`. Generation errors point at the `.templ` line and column;
`nexus dev` keeps the last good build serving until the template compiles again.

**Tailwind:** a stylesheet that does `@import "tailwindcss"` (outside a Vite
frontend) is compiled with the Tailwind standalone CLI: `input.css` → `output.css`.
When it imports `./sources.generated.css`, nexus writes that file — an `@source`
for your templates and for every Go dependency that ships `.templ` files — so a
component library imported as a Go module contributes its classes. A library's
own stylesheets are imported from its module too, wherever Go keeps it on this
machine — list them, with optional conditions, in `nexus.toml`:

```toml
[runtime.tailwind]
imports = [
  "github.com/axadrn/shadcn-templ/v2/assets/css/shadcn-tailwind.css",
  "github.com/axadrn/shadcn-templ/v2/assets/css/styles/style-nova.css layer(base)",
]
```

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
| `Tabs(TabsProps)`, `TabPanel(id, active)` | a tab is a live event (`OnSelect`), a link (`Href`) or a browser-switched panel (`Panel`); the clicked tab shows selected at once, and keeps its choice across re-renders (JS commands) |
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
| `data-ui-open="id"`, `data-ui-close` | open and close a browser-side dialog — JS commands: it opens on its first field and closes back to its opener; its closing steps are its `data-cancel` |
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
the kit's shared pieces (`base.go`, `commands.go`, `ui.go`, `icon.templ`, `ui.js`, `ui.css`)
into your project as your own package, rewriting the package clause. The
copy serves its assets under `/_ui/<package>/`; load them with its
`Script()` instead of the kit's. Existing files are kept unless `--force`.

Dialogs and tabs run as the view runtime's [JS commands](#js-commands), so
`ui.js` needs `view.Script()` loaded first (as `@ui.Script()` after
`@view.Script()` already does), and a browser-switched tab keeps its choice
when the page re-renders. Limits: open menus follow the server again when a
live page re-renders; a browser-side dialog keeps its content
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

An editor still on plain gopls reads the files `nexus dev` writes (or run
`nexus generate views`). `templ fmt` works unchanged.

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
`diff` or `full`, `live.bytes`).

A page's gates feed `auth.OpGates` under its route (`GET /board`) and under its
component (`pets.Board`), so navigation can ask whether the user may open a page
from the same declaration that guards it.
