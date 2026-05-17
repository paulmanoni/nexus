# Vue 3 nl-island bridge

Two files per island:

- **`Counter.vue`** — the component. Standard Vue 3 SFC; you'll edit this.
- **`Counter.island.ts`** — the bridge. Same shape for every Vue island —
  copy it verbatim and only swap the `import Component from './Counter.vue'`
  line.

## How props flow

The server sends a fresh JSON blob on every `:nl-island-props` change. The
bridge holds it in a `reactive()` object and **mutates in place** on
`updated()` — Vue's reactivity picks up the diff and re-renders. Replacing
the reference would break reactivity.

Inside `Counter.vue` you use `defineProps()` like usual; the bridge feeds
the reactive object as the component's props.

## How the channel reaches your component

The bridge calls `app.provide('nlChannel', channel)`. Inside the component:

```vue
<script setup lang="ts">
import { inject } from 'vue'

type Channel = { on(event: string, fn: (payload: any) => void): () => void }
const channel = inject<Channel>('nlChannel')

const off = channel?.on('reset', (payload) => { /* … */ })
// Don't forget onBeforeUnmount(() => off?.())
</script>
```

`channel.on()` returns an unsubscribe function; call it from
`onBeforeUnmount` so reload-style swaps don't leak listeners.

## Embedding in your template

```html
<nl-island name="Counter"
           src="/islands/Counter.js"
           :props="CounterProps()"/>
```

```go
func (p *Page) CounterProps() map[string]any {
    return map[string]any{"initial": p.count}
}

func (p *Page) Reset(ctx *template.Ctx) {
    ctx.PushIsland("Counter", "reset", nil)
}
```
