<script setup>
// LaneNode — a non-interactive tier label that floats above a column of
// the layered topology (Clients / Services / Data / Jobs). Rendered as a
// Vue Flow node so it pans + zooms with the canvas; Architecture.vue
// positions one per present tier from the laid-out node bounds.
defineProps(['data'])
</script>

<template>
  <div class="lane-label">{{ data.text }}</div>
</template>

<style scoped>
.lane-label {
  font-size: 10.5px;
  font-weight: 700;
  letter-spacing: 0.14em;
  text-transform: uppercase;
  color: var(--ink-3);
  pointer-events: none;
  white-space: nowrap;
  display: flex;
  align-items: center;
  gap: 8px;
  font-family: var(--font-sans);
}
.lane-label::before {
  content: '';
  width: 5px;
  height: 5px;
  border-radius: 50%;
  background: var(--accent);
}
</style>
