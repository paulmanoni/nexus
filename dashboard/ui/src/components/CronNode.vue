<script setup>
import { computed, inject } from 'vue'
import { Calendar, AlertTriangle } from 'lucide-vue-next'
import BaseNodeCard from './BaseNodeCard.vue'
import CategoryIcon from './CategoryIcon.vue'
import DeploymentTag from './DeploymentTag.vue'

// CronNode renders a scheduled job (app.Cron(...)) on the architecture
// graph. Schedule shown inline, status pill mirrors the resource +
// worker conventions, last-error gets a red panel beneath the schedule
// row when present. Click opens the cron drawer.
const props = defineProps(['data'])

const openDrawer = inject('nexus.openDrawer', () => {})
function onClick() {
  openDrawer({ kind: 'cron', key: props.data.name })
}

const lastErr = computed(() => props.data.lastRun?.error || '')
const lastRunOK = computed(() => props.data.lastRun?.success === true)

// Map cron lifecycle to the canonical status set so the pill shares
// vocabulary with workers/resources.
const pillState = computed(() => {
  if (props.data.paused) return 'inactive'
  if (lastErr.value) return 'error'
  if (props.data.running) return 'throttled'
  if (props.data.lastRun) return lastRunOK.value ? 'healthy' : 'error'
  return 'healthy'
})
const pillLabel = computed(() => {
  if (props.data.paused) return 'paused'
  if (props.data.running) return 'running'
  if (lastErr.value) return 'failed'
  if (lastRunOK.value) return 'ok'
  return 'idle'
})
</script>

<template>
  <BaseNodeCard @click.stop="onClick">
    <template #head>
      <CategoryIcon type="cron" :size="32" />
      <div class="title">
        <div class="name-row">
          <span class="name">{{ data.name }}</span>
          <DeploymentTag v-if="data.deployment" :name="data.deployment" />
        </div>
        <div class="kind">cron</div>
      </div>
      <span class="status" :class="pillState">
        <span class="dot" />
        {{ pillLabel }}
      </span>
    </template>
    <div class="row">
      <Calendar :size="11" :stroke-width="2" class="ico" />
      <code class="schedule">{{ data.schedule || '—' }}</code>
    </div>
    <div v-if="lastErr" class="err" :title="lastErr">
      <AlertTriangle :size="11" :stroke-width="2" />
      <span class="err-msg">{{ lastErr }}</span>
    </div>
  </BaseNodeCard>
</template>

<style scoped>
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

.row {
  display: flex;
  align-items: center;
  gap: 6px;
}
.ico { color: var(--cat-cron); flex-shrink: 0; }
.schedule {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text);
  background: var(--bg-hover);
  padding: 1px 7px;
  border-radius: var(--radius-sm);
}

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
</style>