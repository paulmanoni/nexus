<script setup>
import { computed, inject } from 'vue'
import { Database, Link2, AlertTriangle } from 'lucide-vue-next'
import BaseNodeCard from './BaseNodeCard.vue'
import CategoryIcon from './CategoryIcon.vue'
import DeploymentTag from './DeploymentTag.vue'

// WorkerNode renders a nexus.AsWorker entry on the architecture graph.
// Workers are long-lived background tasks (DB listeners, queue
// consumers, sweepers) — structurally similar to a service in that
// they depend on resources / other services, but they don't handle
// HTTP traffic, so they don't need the row-by-row endpoint layout
// ServiceNode uses. A single card per worker with a status pill +
// dep list is enough to tell the "what does this worker depend on" story.
const props = defineProps(['data'])

// Drawer plumbing — clicking a worker node opens the per-worker
// detail drawer (status, last error, deps, actions).
const openDrawer = inject('nexus.openDrawer', () => {})

function onClick() {
  openDrawer({ kind: 'worker', key: props.data.name })
}

const statusClass = computed(() => props.data.status || 'unknown')
const hasDeps = computed(() => {
  const r = props.data.resourceDeps || []
  const s = props.data.serviceDeps || []
  return r.length + s.length > 0
})
// Status pill state — maps the worker lifecycle to the canonical status
// set so the pill shares a vocabulary with resources (healthy/error).
const pillState = computed(() => {
  switch (statusClass.value) {
    case 'running':  return 'healthy'
    case 'starting': return 'throttled'
    case 'failed':   return 'error'
    case 'stopped':  return 'inactive'
    default:         return 'inactive'
  }
})
</script>

<template>
  <BaseNodeCard :status="statusClass" source @click.stop="onClick">
    <template #head>
      <CategoryIcon type="worker" :size="32" />
      <div class="title">
        <div class="name-row">
          <span class="name">{{ data.name }}</span>
          <DeploymentTag v-if="data.deployment" :name="data.deployment" />
        </div>
        <div class="kind">worker</div>
      </div>
      <span class="status" :class="pillState" :title="data.status">
        <span class="dot" />
        {{ data.status || 'unknown' }}
      </span>
    </template>
    <div v-if="data.lastError" class="err" :title="data.lastError">
      <AlertTriangle :size="11" :stroke-width="2" />
      <span class="err-msg">{{ data.lastError }}</span>
    </div>
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
/* Whole card opens the worker drawer on click. */
:deep(.base-node) { cursor: pointer; }

.title {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.name-row {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
}
.name {
  flex: 1;
  min-width: 0;
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

.status {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: var(--fs-xs);
  font-weight: 500;
  padding: 2px 8px;
  border-radius: 999px;
  flex-shrink: 0;
  text-transform: capitalize;
}
.status .dot { width: 6px; height: 6px; border-radius: 50%; background: currentColor; }
.status.healthy   { background: var(--st-healthy-soft);   color: var(--st-healthy); }
.status.throttled { background: var(--st-throttled-soft); color: var(--st-throttled); }
.status.error     { background: var(--st-error-soft);     color: var(--st-error); }
.status.inactive  { background: var(--st-inactive-soft);  color: var(--st-inactive); }

.err {
  display: inline-flex;
  gap: 5px;
  align-items: center;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--st-error);
  overflow: hidden;
}
.err-msg { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

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