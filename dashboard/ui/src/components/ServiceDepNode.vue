<script setup>
import { computed, inject } from 'vue'
import { Handle, Position } from '@vue-flow/core'
import { Box } from 'lucide-vue-next'

// ServiceDepNode represents a nexus.Service as a DEPENDENCY that one or
// more endpoints consume. Visual parity with ResourceNode: a small pill
// on the right of the canvas. The conceptual shift away from services-
// as-containers lives here — modules now group endpoints; services are
// just typed deps endpoints ask for, on equal footing with DBs/caches.
const props = defineProps(['data'])

// When an op is selected, non-matching service deps dim. Match rule:
// the service is the op's declared owning service OR appears in the
// op's ServiceDeps list.
const selection = inject('nexus.opSelection', { value: null })
const inSelection = computed(() => {
  const sel = selection.value
  if (!sel) return true
  if (sel.owningService === props.data.name) return true
  return Array.isArray(sel.serviceDeps) && sel.serviceDeps.includes(props.data.name)
})
</script>

<template>
  <div class="svc-dep-node" :class="{ dim: !inSelection }">
    <Handle type="target" :position="Position.Left" />
    <div class="head">
      <Box :size="13" :stroke-width="2" class="icon" />
      <span class="name">{{ data.name }}</span>
      <span class="tag">service</span>
    </div>
    <div v-if="data.description" class="desc">{{ data.description }}</div>
  </div>
</template>

<style scoped>
.svc-dep-node {
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
.svc-dep-node.dim { opacity: 0.3; transition: opacity 120ms; }
.head {
  display: flex;
  align-items: center;
  gap: 7px;
  font-family: var(--font-mono);
  font-size: 12px;
  font-weight: 600;
}
.icon { color: #7c3aed; flex-shrink: 0; }
.name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.tag {
  font-family: var(--font-mono);
  font-size: 10px;
  font-weight: 700;
  padding: 1px 7px;
  border-radius: 10px;
  background: #ede9fe;
  color: #6b21a8;
  text-transform: lowercase;
  letter-spacing: 0.02em;
  flex-shrink: 0;
}
.desc {
  color: var(--text-dim);
  font-size: 11px;
  margin-top: 4px;
  line-height: 1.4;
}
</style>