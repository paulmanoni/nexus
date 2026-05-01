<script setup>
import { Handle, Position } from '@vue-flow/core'
import CategoryIcon from './CategoryIcon.vue'

// InternetNode represents the outside world — the source of every
// request the dashboard visualises. Rendered once at the far left of
// the Architecture canvas; edges fan out to each service. Live traffic
// animation (in Architecture.vue) briefly highlights the edge from this
// node to whichever service just received a request.
defineProps(['data'])
</script>

<template>
  <div class="internet-node">
    <CategoryIcon type="internet" :size="40" />
    <div class="label">Clients</div>
    <div class="sub">external traffic</div>
    <Handle type="source" :position="Position.Right" />
  </div>
</template>

<style scoped>
.internet-node {
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  padding: var(--space-3) var(--space-4);
  min-width: 150px;
  color: var(--text);
  box-shadow: var(--shadow-md);
  font-family: var(--font-sans);
  position: relative;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: var(--space-1);
  transition: border-color 120ms, box-shadow 120ms;
}
.label {
  font-weight: 600;
  font-size: var(--fs-md);
  color: var(--text);
  margin-top: var(--space-1);
}
.sub {
  font-size: var(--fs-xs);
  color: var(--text-dim);
}

/* Pulsing glow when a request is flowing through this node. Architecture
   toggles the 'active' class on the VueFlow node data via the live-traffic
   handler; the ring animates out briefly then disappears. */
.internet-node.active {
  border-color: var(--accent);
  box-shadow: 0 0 0 4px var(--accent-soft);
}
</style>