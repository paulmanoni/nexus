<script setup>
import { computed } from 'vue'
import { Handle, Position } from '@vue-flow/core'
import { Globe, Zap, Radio } from 'lucide-vue-next'

const props = defineProps(['data'])
const MAX_VISIBLE = 6

const displayed = computed(() => (props.data.endpoints || []).slice(0, MAX_VISIBLE))
const hidden = computed(() => Math.max(0, (props.data.endpoints?.length || 0) - MAX_VISIBLE))
const total = computed(() => props.data.endpoints?.length || 0)

function iconFor(t) {
  if (t === 'websocket') return Radio
  if (t === 'graphql') return Zap
  return Globe
}

function label(e) {
  if (e.Transport === 'rest') return `${e.Method} ${e.Path}`
  if (e.Transport === 'graphql') return `${e.Method} ${e.Name}`
  return e.Path
}
</script>

<template>
  <div class="service-node">
    <Handle type="target" :position="Position.Left" />
    <div class="header">
      <span class="name">{{ data.name }}</span>
      <span class="total">{{ total }}</span>
    </div>
    <div v-if="data.description" class="desc">{{ data.description }}</div>
    <div class="endpoints">
      <div v-for="(e, i) in displayed" :key="i" class="item" :class="e.Transport">
        <component :is="iconFor(e.Transport)" :size="11" :stroke-width="2" class="icon" />
        <span class="label">{{ label(e) }}</span>
      </div>
      <div v-if="hidden > 0" class="more">+ {{ hidden }} more</div>
      <div v-if="!total" class="empty-item">No endpoints</div>
    </div>
    <Handle type="source" :position="Position.Right" />
  </div>
</template>

<style scoped>
.service-node {
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: 10px;
  min-width: 240px;
  max-width: 280px;
  color: var(--text);
  box-shadow: var(--shadow-md);
  overflow: hidden;
  font-family: var(--font-sans);
}
.header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding: 9px 14px;
  background: #111827;
  color: #f9fafb;
  font-family: var(--font-mono);
  font-size: 12.5px;
  font-weight: 600;
  letter-spacing: 0.01em;
}
.total {
  background: rgba(255, 255, 255, 0.14);
  padding: 1px 8px;
  border-radius: 10px;
  font-size: 10.5px;
  font-weight: 600;
  color: #f9fafb;
}
.desc {
  padding: 8px 14px;
  color: var(--text-muted);
  font-size: 11.5px;
  border-bottom: 1px solid var(--border);
  line-height: 1.45;
}
.endpoints { padding: 6px 6px 8px; }
.item {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 4px 10px;
  font-family: var(--font-mono);
  font-size: 11px;
  border-radius: 4px;
  color: var(--text);
}
.icon { flex-shrink: 0; }
.item.rest .icon { color: var(--rest); }
.item.graphql .icon { color: var(--graphql); }
.item.websocket .icon { color: var(--ws); }
.label {
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  flex: 1;
  min-width: 0;
}
.more, .empty-item {
  padding: 4px 10px;
  color: var(--text-dim);
  font-size: 11px;
  font-family: var(--font-sans);
}
</style>
