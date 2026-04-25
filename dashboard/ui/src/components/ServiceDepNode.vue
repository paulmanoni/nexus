<script setup>
import { computed, inject } from 'vue'
import { Box, Database, Link2 } from 'lucide-vue-next'
import BaseNodeCard from './BaseNodeCard.vue'

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

const hasDeps = computed(() => {
  const r = props.data.resourceDeps || []
  const s = props.data.serviceDeps || []
  return r.length + s.length > 0
})
</script>

<template>
  <BaseNodeCard :dim="!inSelection" source>
    <template #head>
      <Box :size="13" :stroke-width="2" class="icon" />
      <span class="name">{{ data.name }}</span>
      <span class="tag">service</span>
    </template>
    <template v-if="data.description" #description>{{ data.description }}</template>
    <!-- Inline dep list — surfaces ProvideService's constructor deps
         on the node itself in addition to the graph edges. Helps when
         the dagre layout routes an edge behind another node and makes
         the relationship hard to eyeball. -->
    <div v-if="hasDeps" class="deps">
      <div v-for="r in data.resourceDeps || []" :key="'r:' + r" class="dep">
        <Database :size="10" :stroke-width="2" class="dep-ico" />
        <span class="dep-name">{{ r }}</span>
      </div>
      <div v-for="s in data.serviceDeps || []" :key="'s:' + s" class="dep svc">
        <Link2 :size="10" :stroke-width="2" class="dep-ico" />
        <span class="dep-name">{{ s }}</span>
      </div>
    </div>
  </BaseNodeCard>
</template>

<style scoped>
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
.deps {
  margin-top: 8px;
  padding-top: 7px;
  border-top: 1px dashed var(--border);
  display: flex;
  flex-direction: column;
  gap: 3px;
}
.dep {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-family: var(--font-mono);
  font-size: 10.5px;
  color: var(--text-muted);
}
.dep-ico { color: var(--accent); flex-shrink: 0; }
.dep.svc .dep-ico { color: #7c3aed; }
.dep-name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>