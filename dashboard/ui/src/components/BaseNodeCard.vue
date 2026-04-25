<script setup>
import { Handle, Position } from '@vue-flow/core'

// BaseNodeCard is the shared card frame used by ServiceDepNode,
// ResourceNode, and WorkerNode — every "rounded card with optional
// left/right handles, a header row, optional description, and a
// freeform body" node we draw in the architecture graph.
//
// ServiceNode (the big endpoint-rows node), BoundaryNode (the
// dashed system perimeter), and InternetNode (the clients puck)
// have shapes too different to fit through this slot — they keep
// their own bespoke layouts.
//
// Slots:
//   #head        — header row (icon, name, tag, status pill, etc.)
//   #description — small grey paragraph below the header
//   default      — node body (deps list, details grid, etc.)
//
// Props:
//   target       — render the left-side target Handle (default true)
//   source       — render the right-side source Handle (default false)
//   dim          — fade the card while op-selection has filtered it out
//   unhealthy    — red border (used by ResourceNode for failed health)
//   status       — extra modifier class ('failed' / 'stopped' / 'running')
defineProps({
  target: { type: Boolean, default: true },
  source: { type: Boolean, default: false },
  dim: { type: Boolean, default: false },
  unhealthy: { type: Boolean, default: false },
  status: { type: String, default: '' },
})
</script>

<template>
  <div class="base-node" :class="[status, { dim, unhealthy }]">
    <Handle v-if="target" type="target" :position="Position.Left" />
    <div v-if="$slots.head" class="head"><slot name="head" /></div>
    <div v-if="$slots.description" class="desc"><slot name="description" /></div>
    <slot />
    <Handle v-if="source" type="source" :position="Position.Right" />
  </div>
</template>

<style scoped>
.base-node {
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 10px 12px;
  min-width: 200px;
  max-width: 240px;
  color: var(--text);
  box-shadow: var(--shadow-sm);
  font-family: var(--font-sans);
}
.base-node.dim       { opacity: 0.3; transition: opacity 120ms; }
.base-node.unhealthy { border-color: var(--error); }
.base-node.failed    { border-color: var(--error); }
.base-node.stopped   { opacity: 0.65; }

.head {
  display: flex;
  align-items: center;
  gap: 7px;
  font-family: var(--font-mono);
  font-size: 12px;
  font-weight: 600;
}
.desc {
  color: var(--text-dim);
  font-size: 11px;
  margin-top: 4px;
  line-height: 1.4;
}
</style>