<script setup>
import { computed } from 'vue'
import { Handle, Position } from '@vue-flow/core'
import { Database, HardDrive, Radio as RadioIcon } from 'lucide-vue-next'

const props = defineProps(['data'])

const icon = computed(() => {
  if (props.data.kind === 'cache') return HardDrive
  if (props.data.kind === 'queue') return RadioIcon
  return Database
})

// Pill surfaces the most-interesting single detail: backend for caches,
// engine for databases. Updates live because details are probed each poll.
const pillLabel = computed(() => {
  const d = props.data.details || {}
  if (props.data.kind === 'cache') return d.backend
  if (props.data.kind === 'database') return d.engine
  if (props.data.kind === 'queue') return d.broker
  return null
})

const pillClass = computed(() => {
  const l = (pillLabel.value || '').toLowerCase()
  if (l === 'redis') return 'redis'
  if (l === 'memory' || l === 'in-memory') return 'memory'
  if (l === 'postgres' || l === 'postgresql') return 'postgres'
  if (l === 'mysql' || l === 'mariadb') return 'mysql'
  if (l === 'rabbitmq') return 'rabbit'
  if (l === 'kafka') return 'kafka'
  return 'neutral'
})

// Secondary detail rows — skip whatever we already promoted into the pill.
const detailKeys = computed(() => {
  const d = props.data.details || {}
  const skip = new Set(['backend', 'engine', 'broker'])
  return Object.keys(d).filter(k => !skip.has(k)).slice(0, 3)
})
</script>

<template>
  <div class="resource-node" :class="{ unhealthy: !data.healthy }">
    <Handle type="target" :position="Position.Left" />
    <div class="head">
      <component :is="icon" :size="13" :stroke-width="2" class="icon" />
      <span class="name">{{ data.name }}</span>
      <span v-if="pillLabel" class="pill" :class="pillClass">{{ pillLabel }}</span>
      <span class="dot" :class="{ on: data.healthy }" :title="data.healthy ? 'Healthy' : 'Unhealthy'"></span>
    </div>
    <div v-if="data.description" class="desc">{{ data.description }}</div>
    <div v-if="detailKeys.length" class="details">
      <div v-for="k in detailKeys" :key="k" class="row">
        <span class="k">{{ k }}</span>
        <span class="v">{{ data.details[k] }}</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.resource-node {
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
.resource-node.unhealthy { border-color: var(--error); }
.head {
  display: flex;
  align-items: center;
  gap: 7px;
  font-family: var(--font-mono);
  font-size: 12px;
  font-weight: 600;
}
.icon { color: var(--accent); flex-shrink: 0; }
.name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--text-dim);
  flex-shrink: 0;
}
.dot.on { background: var(--success); box-shadow: 0 0 0 3px var(--success-soft); }

.pill {
  font-family: var(--font-mono);
  font-size: 10px;
  font-weight: 700;
  padding: 1px 7px;
  border-radius: 10px;
  text-transform: lowercase;
  letter-spacing: 0.02em;
  flex-shrink: 0;
}
.pill.redis     { background: #dbeafe; color: #1e40af; }
.pill.memory    { background: #fef3c7; color: #b45309; }
.pill.postgres  { background: #dbeafe; color: #1e40af; }
.pill.mysql     { background: #fed7aa; color: #9a3412; }
.pill.rabbit    { background: #fed7aa; color: #9a3412; }
.pill.kafka     { background: #e9d5ff; color: #6b21a8; }
.pill.neutral   { background: var(--bg-hover); color: var(--text-muted); }

.desc {
  color: var(--text-dim);
  font-size: 11px;
  margin-top: 4px;
  line-height: 1.4;
}
.details {
  margin-top: 8px;
  padding-top: 8px;
  border-top: 1px solid var(--border);
  font-family: var(--font-mono);
  font-size: 11px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.details .row { display: flex; gap: 8px; }
.details .k { color: var(--text-dim); min-width: 50px; }
.details .v { color: var(--text); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
