// arch-canvas.island.ts — generated bridge per the cookbook
// (cookbook/islands/vue/Counter.island.ts). Identical shape;
// only the imported component swaps.
//
// Don't hand-edit. `nexus island --vue arch-canvas` would
// regenerate this file.

import { createApp, reactive, type App } from 'vue'
import Component from './arch-canvas.vue'

// VueFlow's stylesheet is injected via a CDN <link> tag at
// first mount. Idempotent so two ArchCanvas islands on one
// page don't duplicate the tag, and so hot-reload doesn't
// pile on duplicates.
const VUE_FLOW_CSS_URL = 'https://esm.sh/@vue-flow/core@1.41.0/dist/style.css'
function ensureVueFlowStyles() {
  const marker = 'data-nl-vueflow-css'
  if (document.querySelector(`link[${marker}]`)) return
  const link = document.createElement('link')
  link.rel = 'stylesheet'
  link.href = VUE_FLOW_CSS_URL
  link.setAttribute(marker, '')
  document.head.appendChild(link)
}

type Channel = {
  on(event: string, fn: (payload: any) => void): () => void
}

type Inst = {
  app: App
  props: Record<string, unknown>
}

export function mount(el: Element, props: any, channel: Channel): Inst {
  ensureVueFlowStyles()
  const reactiveProps = reactive((props ?? {}) as Record<string, unknown>)
  const app = createApp(Component, reactiveProps as any)
  app.provide('nlChannel', channel)
  app.mount(el)
  return { app, props: reactiveProps }
}

export function updated(_el: Element, newProps: any, inst: Inst) {
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
