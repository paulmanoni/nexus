# React 18 nl-island bridge

Three files per project:

- **`_nl-react-runtime.ts`** — shared once across all React islands.
  Provides a tiny `ChannelContext` + `useChannel()` hook. Copy once into
  your project, all React islands import from it.
- **`Counter.tsx`** — your component. Uses `useChannel()` to subscribe to
  server pushes; otherwise standard React.
- **`Counter.island.tsx`** — the bridge. Same shape for every React island
  — copy and swap the `import Counter from './Counter'` line.

## How props flow

Bridge holds the current props in a tiny external store, exposes a
`subscribe` + `snapshot` pair, and feeds them to React via
`useSyncExternalStore`. When the server changes `:nl-island-props`,
`updated()` calls `setProps` → store fires → React re-renders with the new
value. No remount, internal state (`useState`, refs, etc.) survives.

This pattern is the official React 18 recipe for "external state that
React subscribes to" — concurrent-safe, batched correctly.

## How the channel reaches your component

The bridge wraps your component in `<ChannelContext.Provider value={channel}>`.
Anywhere inside, `useChannel()` returns it:

```tsx
import { useEffect } from 'react'
import { useChannel } from './_nl-react-runtime'

const channel = useChannel()
useEffect(() => {
  const off = channel.on('reset', () => { /* … */ })
  return off  // cleanup on unmount; off is the unsubscribe func
}, [channel])
```

## Embedding in your template

```html
<nl-island name="Counter"
           src="/islands/Counter.js"
           :props="CounterProps()"/>
```

Same Go-side as the Vue example — the bridge is the only difference.
