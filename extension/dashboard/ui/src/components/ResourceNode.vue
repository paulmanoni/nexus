<script setup>
import { computed, inject } from 'vue'
import { Handle, Position } from '@vue-flow/core'
import { Database, Zap, Layers, Share2 } from 'lucide-vue-next'

// ResourceNode — an external dependency (DB / cache / queue / other) as a
// calm atom card. Muted glyph by kind, name in mono, a single meta line
// (engine / backend / broker), and a health dot. Detail lives in the
// Inspector (click to select) and the Cmd-K resource drawer.
const props = defineProps(['data'])

const selectedId = inject('nexus.selectedNodeId', { value: null })
const focusSet = inject('nexus.focusSet', { value: null })
const density = inject('nexus.density', { value: 'regular' })

const id = computed(() => 'res:' + props.data.name)
const selected = computed(() => selectedId.value === id.value)
const dimmed = computed(() => focusSet.value ? !focusSet.value.has(id.value) : false)

const ICONS = { cache: Zap, queue: Layers, database: Database }
const Glyph = computed(() => ICONS[props.data.kind] || Share2)
const health = computed(() => (props.data.healthy === false ? 'err' : 'ok'))

const meta = computed(() => {
  const d = props.data.details || {}
  if (props.data.kind === 'database') return d.engine || 'database'
  if (props.data.kind === 'cache') return d.backend || 'cache'
  if (props.data.kind === 'queue') return d.broker || 'queue'
  return props.data.kind || 'resource'
})
</script>

<template>
  <div class="vf-node" :class="[{ selected, dim: dimmed }, 'density-' + density.value]">
    <Handle type="target" :position="Position.Left" />
    <div class="atom" :class="{ down: data.healthy === false }">
      <div class="nrow">
        <span class="nico"><component :is="Glyph" :size="15" :stroke-width="1.7" /></span>
        <span class="nname">{{ data.name }}</span>
        <span class="health" :class="health"></span>
      </div>
      <div class="nmeta">{{ meta }}</div>
    </div>
  </div>
</template>

<style scoped>
.vf-node { position: relative; cursor: pointer; }
.atom {
  width: 168px;
  background: linear-gradient(180deg, var(--surface-2), var(--surface));
  border: 1px solid var(--line);
  border-radius: 12px;
  box-shadow: var(--shadow-card), inset 0 1px 0 var(--hi);
  padding: 13px 15px;
  transition: box-shadow var(--speed) var(--ease), border-color var(--speed) var(--ease),
              transform var(--speed) var(--ease), opacity var(--speed) var(--ease);
}
.vf-node:hover .atom {
  transform: translateY(-1px); border-color: transparent;
  box-shadow: var(--shadow-card), inset 0 1px 0 var(--hi), 0 0 0 1px var(--accent-line);
}
.vf-node.selected .atom { box-shadow: var(--shadow-sel), inset 0 1px 0 var(--hi); border-color: transparent; }
.vf-node.dim .atom { opacity: .34; filter: saturate(.6); }
.atom.down { border-color: var(--err); }
.nrow { display: flex; align-items: center; gap: 8px; }
.nico { color: var(--ink-3); display: inline-flex; flex: none; }
.nname { flex: 1; min-width: 0; font-family: var(--font-mono); font-weight: 600; font-size: 13.5px; color: var(--ink); letter-spacing: -.01em; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.nmeta { font-size: 11px; color: var(--ink-3); margin-top: 6px; }
.health { width: 7px; height: 7px; border-radius: 50%; flex: none; }
.health.ok { background: var(--st-healthy); }
.health.err { background: var(--err); }
.density-compact .atom { padding: 10px 12px; }
.density-compact .nmeta { margin-top: 4px; font-size: 10.5px; }
.density-comfy .atom { padding: 16px 18px; }
.density-comfy .nmeta { margin-top: 9px; }
:deep(.vue-flow__handle) {
  width: 8px; height: 8px; background: var(--edge-strong); border: 2px solid var(--bg);
  opacity: 0; transition: opacity var(--speed) var(--ease);
}
.vf-node:hover :deep(.vue-flow__handle), .vf-node.selected :deep(.vue-flow__handle) { opacity: .8; }
</style>
