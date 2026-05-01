<script setup>
import { computed, inject } from 'vue'
import BaseNodeCard from './BaseNodeCard.vue'
import CategoryIcon from './CategoryIcon.vue'

// ResourceNode renders one external dep — DB, cache, queue, or generic
// "other" — on the architecture canvas. Visual grammar comes from
// BaseNodeCard + CategoryIcon so it stays in lockstep with services and
// workers; only the icon hue + the engine/backend pill tell you the kind.
const props = defineProps(['data'])

// Drawer plumbing — clicking a resource node opens the per-resource
// detail drawer (health, details map, attached services, actions).
const openDrawer = inject('nexus.openDrawer', () => {})

function onClick() {
  openDrawer({ kind: 'resource', key: props.data.name })
}

// When an op is selected, resources not in the op's resource list dim.
// Provided by Architecture via provide/inject.
const selection = inject('nexus.opSelection', { value: null })
const inSelection = computed(() => {
  const sel = selection.value
  if (!sel) return true
  return Array.isArray(sel.resources) && sel.resources.includes(props.data.name)
})

// Map registry kind → CategoryIcon type. The taxonomy lives in tokens.css;
// "other" falls back to database so the canvas never has an un-iconed
// resource (rare in practice; only Resource.Other types).
const iconType = computed(() => {
  if (props.data.kind === 'cache')    return 'cache'
  if (props.data.kind === 'queue')    return 'queue'
  if (props.data.kind === 'database') return 'database'
  return 'database'
})

// Pill surfaces the most-interesting single detail: backend for caches,
// engine for databases, broker for queues. Updates live because details
// are probed each snapshot.
const pillLabel = computed(() => {
  const d = props.data.details || {}
  if (props.data.kind === 'cache') return d.backend
  if (props.data.kind === 'database') return d.engine
  if (props.data.kind === 'queue') return d.broker
  return null
})

// Secondary detail rows — skip whatever we already promoted into the pill.
const detailKeys = computed(() => {
  const d = props.data.details || {}
  const skip = new Set(['backend', 'engine', 'broker'])
  return Object.keys(d).filter(k => !skip.has(k)).slice(0, 3)
})
</script>

<template>
  <BaseNodeCard :dim="!inSelection" :unhealthy="!data.healthy" @click.stop="onClick">
    <template #head>
      <CategoryIcon :type="iconType" :size="32" />
      <div class="title">
        <div class="name">{{ data.name }}</div>
        <div v-if="pillLabel" class="kind">{{ pillLabel }}</div>
      </div>
      <span
        class="status"
        :class="{ healthy: data.healthy, error: !data.healthy }"
        :title="data.healthy ? 'Healthy' : 'Unhealthy'"
      >
        <span class="dot" />
        {{ data.healthy ? 'healthy' : 'down' }}
      </span>
    </template>
    <template v-if="data.description" #description>{{ data.description }}</template>
    <div v-if="detailKeys.length" class="details">
      <div v-for="k in detailKeys" :key="k" class="row">
        <span class="k">{{ k }}</span>
        <span class="v">{{ data.details[k] }}</span>
      </div>
    </div>
  </BaseNodeCard>
</template>

<style scoped>
/* The whole card is clickable (opens the resource drawer); make that
   discoverable with a pointer cursor scoped to the underlying card. */
:deep(.base-node) { cursor: pointer; }

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

/* Status pill — two-axis design from tokens.css (category color = what,
   status color = how). Slim by default, never competes with the icon. */
.status {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: var(--fs-xs);
  font-weight: 500;
  padding: 2px 8px;
  border-radius: 999px;
  flex-shrink: 0;
  font-variant-numeric: tabular-nums;
}
.status .dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: currentColor;
}
.status.healthy { background: var(--st-healthy-soft); color: var(--st-healthy); }
.status.error   { background: var(--st-error-soft);   color: var(--st-error); }

.details {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  display: flex;
  flex-direction: column;
  gap: 3px;
}
.details .row { display: flex; gap: var(--space-2); }
.details .k { color: var(--text-dim); min-width: 50px; }
.details .v { color: var(--text); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>