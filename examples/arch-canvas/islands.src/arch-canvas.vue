<!--
  arch-canvas.vue — VueFlow inside an nl-island, authored as a
  Vue 3 SFC (replaces the hand-rolled h() call from the prior
  inline version). Same wire shape, much better authoring
  ergonomics: <VueFlow :nodes :edges/> instead of nested
  createElement calls.

  Props are reactive — the bridge mutates them in place on
  every server-driven re-render. Vue's diff picks up the
  changes and VueFlow does an incremental update (animated
  position changes, no remount).
-->
<script setup lang="ts">
import { computed, inject, onBeforeUnmount } from 'vue'
import { VueFlow, Position } from '@vue-flow/core'
// VueFlow's stylesheet ships separately; the bridge injects a
// <link> tag once at mount time so we avoid Vite's hashed CSS
// asset path (which makes the islands/ output untidy and
// requires the browser to fetch a second file even for a
// minimal island).

type ArchNode = { id: string; label: string; x: number; y: number }
type ArchEdge = { id: string; source: string; target: string; animated?: boolean }

const props = defineProps<{
  nodes?: ArchNode[]
  edges?: ArchEdge[]
}>()

// Translate server-side shape → VueFlow's expected node /
// edge shape. Computed so it re-derives on every prop change.
const flowNodes = computed(() =>
  (props.nodes ?? []).map((n) => ({
    id: String(n.id),
    position: { x: Number(n.x) || 0, y: Number(n.y) || 0 },
    data: { label: n.label ?? n.id },
    label: n.label ?? n.id,
    type: 'default',
    sourcePosition: Position.Right,
    targetPosition: Position.Left,
  })),
)

const flowEdges = computed(() =>
  (props.edges ?? []).map((e) => ({
    id: String(e.id),
    source: String(e.source),
    target: String(e.target),
    animated: !!e.animated,
  })),
)

// Channel comes from the bridge via app.provide('nlChannel').
// Subscribe to "focus-node" so ctx.PushIsland("ArchCanvas",
// "focus-node", { id }) on the server lands here.
type Channel = {
  on(event: string, fn: (payload: any) => void): () => void
}
const channel = inject<Channel>('nlChannel')

const offFocus = channel?.on('focus-node', ({ id }: { id: string }) => {
  // Real impl would call useVueFlow().setCenter(node.position.x,
  // node.position.y, { zoom: 1.5 }). Logging keeps the demo
  // self-contained.
  // eslint-disable-next-line no-console
  console.info('[arch-canvas] focus-node', id)
})

onBeforeUnmount(() => {
  offFocus?.()
})
</script>

<template>
  <!-- Sized + styled by the host page via the
       :data-arch-canvas-host wrapper attribute the parent
       template sets on the <nl-island/> element. Keeps the
       built bundle to a single .js file with no companion
       CSS asset. -->
  <VueFlow
    :nodes="flowNodes"
    :edges="flowEdges"
    :fit-view-on-init="true"
    :default-viewport="{ x: 0, y: 0, zoom: 0.9 }"
  />
</template>
