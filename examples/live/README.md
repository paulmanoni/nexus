# `examples/live` — multi-tab live posts demo

A 200-line end-to-end demo of [`live/template`](../../live/template/):
posts list with like / filter / add, where every connected browser
tab stays in sync as state mutates.

## Run

```bash
go run ./examples/live
# → listening on http://localhost:8080 — open in two tabs
```

Open the URL in **two** browser tabs and try:

- **Like** a post in tab A → the count updates in tab B in ~10 ms.
- **Filter** in tab A → tab B's filter is independent (per-session state).
- **Add** a post in tab A → it appears in tab B's list.
- Type in the **filter** input → only the rows matching update;
  unmatched rows aren't touched in the DOM.

## What it shows

| Engine feature | Where in this demo |
|---|---|
| `Parse` + `Lower` at registration | `engine.Register("Posts", postsTemplate, …)` in `main.go` |
| SSR first paint | `<h1>Posts (3)</h1>…` is in the HTML body before JS runs |
| WS upgrade + session goroutine | The `nexus-live.js` bootstrap takes over once loaded |
| Sparse diffs | Like button increments → one slot updates, not the whole list |
| `nl-for` with `:key` | Per-row diff, not per-list rerender |
| `nl-if` | The "No posts match" message appears/vanishes |
| `:bind` and `@on` | `@click="like"` on the like button |
| `nl-model` two-way binding | `nl-model="Filter"` and `nl-model="NewTitle"` on the inputs |
| Event modifiers | `@submit.prevent` on the add form |
| `data-*` payload pickup | `:data-id="p.ID"` → handler reads `payload.Int("id")` |
| `live.Notifier` fan-out | `PostsRepo.Like` calls `notifier.Notify()` → every session re-renders |
| Scoped `<style>` | `<style scoped>` block is inlined into the SSR `<head>` |
| Reflection-dispatched events | `@click="like"` → method `Like(ctx, payload)` |

## What's NOT in this demo

Things the engine doesn't yet ship — see [`live/template/`](../../live/template/)
for the full status:

- Component composition (`ComponentSlot` emits a placeholder).
- Auto-reconnect with state token resume.
- Auth integration.

## Files

```
examples/live/
  main.go      ← 130 lines: PostsRepo + PostsList component + http wiring
  posts.nlt    ← the SFC: <template> + <style scoped>
  README.md    ← this file
```

`go:embed` pulls `posts.nlt` into the binary at compile time, so the
example is a single self-contained executable. No file-watching, no
reload — restart the process to see template edits.
