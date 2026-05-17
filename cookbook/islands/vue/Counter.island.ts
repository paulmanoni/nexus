// Counter.island.ts — generated bridge between nl-island
// lifecycle and Vue 3's createApp/mount/unmount.
//
// Same shape for every Vue island. Copy this file, change
// the import path on the line marked CHANGE, you're done.
// `nexus island --vue <name>` automates the copy.

import { createApp, reactive, type App } from 'vue'
import Component from './Counter.vue' // CHANGE: point at your SFC

type Channel = {
  on(event: string, fn: (payload: any) => void): () => void
}

type Inst = {
  app: App
  // Reactive object holding the current props. updated()
  // mutates this in place; Vue's reactivity picks up the
  // diff and re-renders without remounting.
  props: Record<string, unknown>
}

export function mount(el: Element, props: any, channel: Channel): Inst {
  const reactiveProps = reactive((props ?? {}) as Record<string, unknown>)
  const app = createApp(Component, reactiveProps as any)
  // provide() makes the channel available via inject('nlChannel')
  // anywhere in the component tree.
  app.provide('nlChannel', channel)
  app.mount(el)
  return { app, props: reactiveProps }
}

export function updated(_el: Element, newProps: any, inst: Inst) {
  // Two-step in-place mutation so Vue's reactivity sees the
  // diff: overlay new keys, then drop keys that disappeared.
  // Replacing inst.props's reference wouldn't propagate —
  // Vue's reactivity is bound to the original object.
  Object.assign(inst.props, newProps ?? {})
  for (const k of Object.keys(inst.props)) {
    if (newProps == null || !(k in newProps)) {
      delete (inst.props as any)[k]
    }
  }
}

export function destroyed(_el: Element, inst: Inst) {
  inst.app.unmount()
}
