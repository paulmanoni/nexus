<script setup>
import { Handle, Position } from '@vue-flow/core'
import { Globe } from 'lucide-vue-next'

// InternetNode represents the outside world — the source of every
// request the dashboard visualises. Rendered once at the far left of
// the Architecture canvas; edges fan out to each service. Live traffic
// animation (in Architecture.vue) briefly highlights the edge from this
// node to whichever service just received a request.
defineProps(['data'])
</script>

<template>
  <div class="internet-node">
    <div class="puck">
      <Globe :size="16" :stroke-width="2.2" />
    </div>
    <div class="label">Clients</div>
    <div class="sub">external traffic</div>
    <Handle type="source" :position="Position.Right" />
  </div>
</template>

<style scoped>
.internet-node {
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: 10px;
  padding: 12px 14px;
  min-width: 140px;
  color: var(--text);
  box-shadow: var(--shadow-sm);
  font-family: var(--font-sans);
  position: relative;
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 4px;
}
.puck {
  width: 34px;
  height: 34px;
  border-radius: 50%;
  display: grid;
  place-items: center;
  background: var(--bg-hover);
  color: var(--text-muted);
  margin-bottom: 4px;
}
.label { font-weight: 600; font-size: 12.5px; color: var(--text); }
.sub { font-size: 10.5px; color: var(--text-dim); }

/* Pulsing glow when a request is flowing through this node. Architecture
   toggles the 'active' class on the VueFlow node data via the live-traffic
   handler; the ring animates out briefly then disappears. */
.internet-node.active {
  border-color: var(--accent);
  box-shadow: 0 0 0 4px var(--accent-soft);
}
</style>
