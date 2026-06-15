<script setup>
import { computed, inject } from 'vue'
import { Handle, Position } from '@vue-flow/core'
import { Globe, User, Box, LayoutGrid } from 'lucide-vue-next'

// InternetNode — the single "Clients" node (external traffic source) at the
// far left of the canvas. Calm atom card matching the redesign; its glyph is
// configurable from the Tweaks panel (globe / user / box / layout).
defineProps(['data'])

const selectedId = inject('nexus.selectedNodeId', { value: null })
const focusSet = inject('nexus.focusSet', { value: null })
const clientIcon = inject('nexus.clientIcon', { value: 'globe' })
const density = inject('nexus.density', { value: 'regular' })

const ICONS = { globe: Globe, user: User, box: Box, layout: LayoutGrid }
const Glyph = computed(() => ICONS[clientIcon.value] || Globe)
const selected = computed(() => selectedId.value === 'internet')
const dimmed = computed(() => focusSet.value ? !focusSet.value.has('internet') : false)
</script>

<template>
  <div class="vf-node" :class="[{ selected, dim: dimmed }, 'density-' + density.value]">
    <div class="atom">
      <div class="nrow">
        <span class="nico"><component :is="Glyph" :size="15" :stroke-width="1.7" /></span>
        <span class="nname">Clients</span>
      </div>
      <div class="nmeta">external traffic</div>
    </div>
    <Handle type="source" :position="Position.Right" />
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
.nrow { display: flex; align-items: center; gap: 8px; }
.nico { color: var(--ink-3); display: inline-flex; flex: none; }
.nname { font-family: var(--font-mono); font-weight: 600; font-size: 13.5px; color: var(--ink); letter-spacing: -.01em; }
.nmeta { font-size: 11px; color: var(--ink-3); margin-top: 6px; }
.density-compact .atom { padding: 10px 12px; }
.density-comfy .atom { padding: 16px 18px; }
:deep(.vue-flow__handle) {
  width: 8px; height: 8px; background: var(--edge-strong); border: 2px solid var(--bg);
  opacity: 0; transition: opacity var(--speed) var(--ease);
}
.vf-node:hover :deep(.vue-flow__handle), .vf-node.selected :deep(.vue-flow__handle) { opacity: .8; }
</style>
