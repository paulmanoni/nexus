<script setup>
import { computed, inject } from 'vue'
import { Database, Link2 } from 'lucide-vue-next'
import BaseNodeCard from './BaseNodeCard.vue'
import CategoryIcon from './CategoryIcon.vue'

// ServiceDepNode represents a nexus.Service as a DEPENDENCY that one or
// more endpoints consume. Visual parity with ResourceNode: the same card
// grammar (icon tile + name/kind + body), only the category color
// differs (--cat-service vs --cat-database). The conceptual shift away
// from services-as-containers lives here — modules now group endpoints;
// services are just typed deps endpoints ask for, on equal footing with
// DBs/caches.
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
      <CategoryIcon type="service" :size="32" />
      <div class="title">
        <div class="name">{{ data.name }}</div>
        <div class="kind">service</div>
      </div>
    </template>
    <template v-if="data.description" #description>{{ data.description }}</template>
    <!-- Inline dep list — surfaces ProvideService's constructor deps
         on the node itself in addition to the graph edges. Helps when
         the dagre layout routes an edge behind another node and makes
         the relationship hard to eyeball. -->
    <div v-if="hasDeps" class="deps">
      <div v-for="r in data.resourceDeps || []" :key="'r:' + r" class="dep">
        <Database :size="11" :stroke-width="2" class="dep-ico res" />
        <span class="dep-name">{{ r }}</span>
      </div>
      <div v-for="s in data.serviceDeps || []" :key="'s:' + s" class="dep">
        <Link2 :size="11" :stroke-width="2" class="dep-ico svc" />
        <span class="dep-name">{{ s }}</span>
      </div>
    </div>
  </BaseNodeCard>
</template>

<style scoped>
.title {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.name {
  font-size: var(--fs-md);
  font-weight: 600;
  color: var(--text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.kind {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-dim);
  text-transform: lowercase;
  letter-spacing: 0.02em;
}

.deps {
  display: flex;
  flex-direction: column;
  gap: 3px;
}
.dep {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-muted);
}
.dep-ico { flex-shrink: 0; }
.dep-ico.res { color: var(--cat-database); }
.dep-ico.svc { color: var(--cat-service); }
.dep-name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>