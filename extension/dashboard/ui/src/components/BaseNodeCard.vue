<script setup>
import { Handle, Position } from '@vue-flow/core'

// BaseNodeCard is the shared card frame used by ServiceDepNode,
// ResourceNode, and WorkerNode — every "rounded card with optional
// left/right handles, a header row, optional description / metric row /
// chip row, and a freeform body" node we draw in the architecture graph.
//
// ServiceNode (the big endpoint-rows node), BoundaryNode (the
// dashed system perimeter), and InternetNode (the clients puck)
// have shapes too different to fit through this slot — they keep
// their own bespoke layouts, but follow the same token system.
//
// Slots:
//   #head        — header row (CategoryIcon, name, status pill, …)
//   #description — small grey paragraph below the header
//   #metrics     — single-line metric row (req/s, p99, errors), mono font
//   default      — node body (deps list, details grid, …)
//   #chips       — summary chip row pinned at the bottom
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
    <div v-if="$slots.metrics" class="metrics"><slot name="metrics" /></div>
    <div v-if="$slots.default" class="body"><slot /></div>
    <div v-if="$slots.chips" class="chips"><slot name="chips" /></div>
    <Handle v-if="source" type="source" :position="Position.Right" />
  </div>
</template>

<style scoped>
.base-node {
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  padding: var(--space-3) var(--space-3) var(--space-3) var(--space-3);
  min-width: 240px;
  max-width: 260px;
  color: var(--text);
  box-shadow: var(--shadow-md);
  font-family: var(--font-sans);
  transition: opacity 120ms, border-color 120ms, box-shadow 120ms;
}
.base-node:hover { box-shadow: var(--shadow-lg); }
.base-node.dim       { opacity: 0.35; }
.base-node.unhealthy { border-color: var(--st-error); }
.base-node.failed    { border-color: var(--st-error); }
.base-node.stopped   { opacity: 0.65; }

.head {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: var(--fs-lg);
  font-weight: 600;
  letter-spacing: -0.005em;
}
.desc {
  color: var(--text-muted);
  font-size: var(--fs-sm);
  line-height: var(--lh-body);
  margin-top: var(--space-2);
}
.metrics {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-muted);
  font-variant-numeric: tabular-nums;
  margin-top: var(--space-2);
  padding-top: var(--space-2);
  border-top: 1px solid var(--border);
}
.body { margin-top: var(--space-2); }
.chips {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-1);
  margin-top: var(--space-2);
}
</style>