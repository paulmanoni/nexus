// Counter.island.tsx — generated bridge between nl-island
// lifecycle and React 18's createRoot.
//
// Same shape for every React island. Copy this file, change
// the import path on the line marked CHANGE, you're done.
// `nexus island --react <name>` automates the copy.

import { createElement, useSyncExternalStore } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import Counter from './Counter' // CHANGE: point at your component
import { ChannelContext, type Channel } from './_nl-react-runtime'

type Inst = {
  root: Root
  setProps: (p: any) => void
}

export function mount(el: Element, props: any, channel: Channel): Inst {
  // Tiny external store: holds current props, fires
  // listeners on change. useSyncExternalStore subscribes
  // React to it — concurrent-safe + correctly batched, the
  // official React 18 pattern for external state.
  let current = props ?? {}
  const listeners = new Set<() => void>()
  const subscribe = (fn: () => void) => {
    listeners.add(fn)
    return () => listeners.delete(fn)
  }
  const setProps = (next: any) => {
    current = next ?? {}
    listeners.forEach((fn) => fn())
  }

  // Wrapper component reads the store + injects the channel.
  // Defined inline so each island has its own closure over
  // current/listeners (no module-level shared state — two
  // <nl-island name="Counter"/> instances on one page Just
  // Work).
  const App = () => {
    const p = useSyncExternalStore(subscribe, () => current)
    return createElement(
      ChannelContext.Provider,
      { value: channel },
      createElement(Counter, p as any),
    )
  }

  const root = createRoot(el as HTMLElement)
  root.render(createElement(App))
  return { root, setProps }
}

export function updated(_el: Element, newProps: any, inst: Inst) {
  inst.setProps(newProps)
}

export function destroyed(_el: Element, inst: Inst) {
  inst.root.unmount()
}
