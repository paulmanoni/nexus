# Debug toolbar

Under `nexus dev`, every HTML page the app serves gets a debug toolbar, like Django's
debug toolbar. A handle on the right edge of the page shows the request's time and
query count. Clicking it opens a drawer with what that request did:

- **Request**: method, path, status, time, the handler that served it, the query
  parameters and the headers (cookie and authorization values hidden).
- **SQL**: every statement the request ran, in order, with its time drawn as a bar,
  how often the same statement ran, and the SQL highlighted. Repeated statements are
  amber; a failed one is red, with its error. When the nexus ORM sees the same
  statement run over and over in one request, the panel names the N+1 and how to fix it.
- **Timeline**: a bar for each span the request's trace recorded — the handler, each
  query, each `trace.StartSpan` of your own.
- **Logs**: what the app logged while handling the request.

The handle's edge turns amber when a panel warns (repeated queries, a warning logged)
and red on an error (a 5xx, a failed query, an N+1).

The dropdown at the top lists every request the page made: the page itself, then each
`fetch` and XHR call as it answers — an Inertia visit, a form post, a JSON call — and,
on a live view, the work its socket does: the connected Mount, each event, each URL
update and `Info`, as `LIVE Dashboard.Search` entries. Pick one to see its panels.

Moving to another page without a page load — a live page navigating over its socket,
an Inertia visit, an in-app link — pins the new page's entry (`LIVE /orders`, the
Inertia visit, the fetched page), so the handle and the drawer show the page on
screen. The list keeps every page visited since the last full page load.

## Rebuilds

The toolbar follows `nexus dev`'s rebuild, so you don't need the terminal to know whether
a save is live:

- **Building:** while the next binary compiles — the previous one keeps serving — the
  handle shows a spinner and `building… 12s`, and the open drawer a "Rebuilding" chip.
  The page reloads by itself once the new build takes over.
- **Failed:** a save that doesn't compile turns the handle red (`build failed`), and the
  drawer shows the compiler's output above the panels. The previous build keeps serving
  until a save compiles.
- **Unchanged:** a save that doesn't change the binary (a comment, a `_test.go` file)
  clears the state without a reload.

## What it shows

**Queries from both the nexus ORM and GORM.** Each statement is recorded on the
request's trace, so it also shows in the dashboard's trace waterfall. GORM's statements
are recorded when they run with the request's context:

```go
db.GetDB().WithContext(ctx).Where("owner_id = ?", id).Find(&pets) // recorded
db.GetDB().Where("owner_id = ?", id).Find(&pets)                  // not: no request
```

**Queries the ORM refused.** A query the nexus ORM rejects before sending it — a
value of the wrong kind for a field, a name it doesn't know, a field read into the
wrong type — shows as a red row with the ORM's error, counted under "Refused by the
ORM". Without it such a query would leave no trace when the caller drops the error.

**Logs written with a context.** Records written through the app's logger (the
`*slog.Logger` nexus provides, or `App.Logger()`) with the request's context are
collected: `l.InfoContext(ctx, …)`, `l.WarnContext(ctx, …)`. They still go to the
console as before.

**Which pages get it.** A page the browser loads (a navigation or a frame) whose
answer is a whole HTML document gets the toolbar before its `</body>`. Fetch and XHR
calls, Inertia JSON responses, fragments, `/__nexus` routes and WebSocket upgrades
never do. So that the app's own compression middleware leaves a page as text, a page
load reaches the app without its `Accept-Encoding` header under `nexus dev` (the
Request panel shows the one the browser sent); everything else stays compressed. Every response under `nexus dev` carries an
`X-Nexus-Toolbar` header naming its record, which is how the toolbar lists the page's
calls.

The toolbar lives in a shadow DOM, so the app's CSS can't restyle it and its CSS can't
leak into the app. Its colours are the `/__nexus` console's, light or dark: it follows
the console's theme choice, else the system's.

## Turning it off

It runs only under `nexus dev`: a production binary has no toolbar, no header and no
toolbar routes. To turn it off in development too:

```toml
[runtime.toolbar]
enabled = false
```

## Your own panels

A panel is a name and a function that renders a request. Register it once, from
`main` or an `init`:

```go
import "github.com/paulmanoni/nexus/v2/dev"

dev.AddPanel(dev.Panel{Name: "Cache", Render: func(r *dev.Request) dev.Section {
    t := &dev.Table{Columns: []dev.Column{{Title: "Key", Code: true}, {Title: "Hit"}}}
    for _, n := range r.Notes("Cache") {
        l := n.(Lookup)
        t.Rows = append(t.Rows, []any{l.Key, l.Hit})
    }
    return dev.Section{Summary: fmt.Sprintf("%d lookups", len(t.Rows)), Table: t}
}})
```

Code running inside the request records what the panel shows with `dev.Note`:

```go
func (c *Cache) Get(ctx context.Context, key string) ([]byte, bool) {
    v, ok := c.store.Get(key)
    dev.Note(ctx, "Cache", Lookup{Key: key, Hit: ok})
    return v, ok
}
```

`dev.Note` and `dev.AddPanel` do nothing outside `nexus dev`, so they can stay in
production code.

### Work a page starts outside a request

Live views list their socket work under their page by themselves. Other work a page
starts — over a socket of your own, say — joins the page's list with `dev.Track`, given
the page load's toolbar ID (the `X-Nexus-Toolbar` header of its response):

```go
ctx, finish := dev.Track(ctx, page, "WS", "chat.send")
err := handle(ctx, msg)
finish(200, err)
```

### What a panel can show

`dev.Section` holds the panel's parts; each is optional and they show in this order:

| Field | Shows |
| --- | --- |
| `Summary` | the text under the panel's name in the list, and beside its title |
| `Tone` | colours the summary: `"warn"` or `"error"` (the handle follows the worst) |
| `Stats` | figures at the top: `[]dev.Stat{{Label, Value, Tone}}` |
| `Text` | a preformatted note |
| `View` | a templ component of your own |
| `HTML` | your own markup, shown as is |
| `Table` | rows under `Columns`, with an optional `Tones` per row |

A `dev.Column` can show its cells as code (`Code: true`), as highlighted SQL
(`Lang: "sql"`), right-aligned numbers (`Num: true`), or numbers drawn as bars against
the column's largest (`Bar: true`).

A panel's `Render` gets the whole record: `r.Method`, `r.Path`, `r.Query`, `r.Header`,
`r.Status`, `r.Duration`, `r.Spans` (the trace's spans), `r.Logs()`, `r.Notes(panel)`
and `r.TraceIDs()`. A panel that panics shows its error in place of its content; the
other panels are unaffected. A panel registered under a built-in panel's name, or
under a name already used, replaces it.

## Limits

- The toolbar keeps the latest 200 requests; an older one says it is no longer kept.
- GORM statements repeated in a request are flagged as repeated, but only the nexus ORM
  names an N+1 with its fix.
- A request's spans come from the trace ring buffer: with a very small
  `[runtime] trace_capacity`, a busy request's earliest spans may be gone.
