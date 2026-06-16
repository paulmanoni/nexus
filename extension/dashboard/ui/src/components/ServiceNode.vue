<script setup>
import { computed, inject } from 'vue'
import { Handle, Position } from '@vue-flow/core'
import { Boxes, Box, Lock, Cloud } from 'lucide-vue-next'

// ServiceNode renders one nexus.Module (or bare service group) as a CALM
// card in the redesigned topology: a muted glyph, the module name in mono,
// a health dot, and an optional lock — plus a single muted meta line
// ("module · N endpoints"). All per-endpoint detail (rows, auth, traces,
// limits) lives in the right-hand Inspector now, so the canvas stays
// scannable at scale. Selection + overlay highlight + focus dimming are
// driven from Architecture.vue via provide/inject.
const props = defineProps(['data'])

const selectedId = inject('nexus.selectedNodeId', { value: null })
const focusSet = inject('nexus.focusSet', { value: null })
const overlays = inject('nexus.overlays', { value: new Set() })
const density = inject('nexus.density', { value: 'regular' })

const displayName = computed(() => props.data.name || props.data.service || '')
const isModule = computed(() => !!props.data.isModule)
const headerKind = computed(() => (isModule.value ? 'module' : 'service'))
const total = computed(() => props.data.totalEndpoints ?? (props.data.endpoints || []).length)
const Glyph = computed(() => (isModule.value ? Boxes : Box))

const reqTotal = computed(() => (props.data.endpoints || []).reduce((s, e) => s + (e.Stats?.count || 0), 0))
const errTotal = computed(() => (props.data.endpoints || []).reduce((s, e) => s + (e.Stats?.errors || 0), 0))
const health = computed(() => {
  if (reqTotal.value === 0) return 'idle'
  if (errTotal.value === 0) return 'ok'
  return errTotal.value / reqTotal.value >= 0.02 ? 'err' : 'warn'
})

const hasAuth = computed(() => (props.data.endpoints || []).some(e => {
  const mw = Array.isArray(e.Middleware) ? e.Middleware : []
  return mw.some(m => m === 'auth' || m.startsWith('auth') || m.startsWith('permission'))
}))
const limited = computed(() => (props.data.endpoints || []).some(e => e.RateLimit && e.RateLimit.rpm))

const selected = computed(() => selectedId.value === props.data.groupKey)
const dimmed = computed(() => focusSet.value ? !focusSet.value.has(props.data.groupKey) : false)

// Overlay highlight: when a Highlight chip is active, matching cards get a
// coloured ring and non-matching cards fade. errors > limits > auth.
const overlayClass = computed(() => {
  if (!overlays.value.size) return ''
  const matched = []
  if (overlays.value.has('errors') && errTotal.value > 0) matched.push('errors')
  if (overlays.value.has('limits') && limited.value) matched.push('limits')
  if (overlays.value.has('auth') && hasAuth.value) matched.push('auth')
  if (!matched.length) return 'overlay-dim'
  return 'overlay-' + matched[0]
})
</script>

<template>
  <div class="vf-node" :class="[{ selected, dim: dimmed }, overlayClass, 'density-' + density.value]">
    <Handle type="target" :position="Position.Left" />
    <Handle type="source" :position="Position.Right" />
    <Handle type="source" :position="Position.Right" id="svc" />
    <div class="svc" :class="{ remote: data.remote }">
      <div class="nrow">
        <span class="nico"><component :is="Glyph" :size="15" :stroke-width="1.7" /></span>
        <span class="nname">{{ displayName }}</span>
        <span class="health" :class="health"></span>
        <span class="nspacer"></span>
        <span v-if="data.remote" class="nremote"><Cloud :size="12" :stroke-width="1.8" /></span>
        <span v-else-if="hasAuth" class="nlock"><Lock :size="12" :stroke-width="1.8" /></span>
      </div>
      <div class="nmeta">
        {{ headerKind }} · {{ total }} {{ total === 1 ? 'endpoint' : 'endpoints' }}
        <span v-if="data.deployment" class="ndep" :title="'Deployment: ' + data.deployment">{{ data.deployment }}</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.vf-node { position: relative; cursor: pointer; }
.svc {
  width: 214px;
  background: linear-gradient(180deg, var(--surface-2), var(--surface));
  border: 1px solid var(--line);
  border-radius: 12px;
  box-shadow: var(--shadow-card), inset 0 1px 0 var(--hi);
  padding: 13px 15px;
  transition: box-shadow var(--speed) var(--ease), border-color var(--speed) var(--ease),
              transform var(--speed) var(--ease), opacity var(--speed) var(--ease);
}
.vf-node:hover .svc {
  transform: translateY(-1px);
  border-color: transparent;
  box-shadow: var(--shadow-card), inset 0 1px 0 var(--hi), 0 0 0 1px var(--accent-line);
}
.vf-node.selected .svc { box-shadow: var(--shadow-sel), inset 0 1px 0 var(--hi); border-color: transparent; }
.vf-node.dim .svc { opacity: .34; filter: saturate(.6); }

.svc.remote { border-style: dashed; border-color: var(--cat-peer); }

.nrow { display: flex; align-items: center; gap: 8px; }
.nico { color: var(--ink-3); display: inline-flex; flex: none; }
.nname {
  font-family: var(--font-mono); font-weight: 600; font-size: 13.5px; color: var(--ink);
  letter-spacing: -.01em; white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
}
.nspacer { flex: 1; }
.nlock, .nremote { color: var(--ink-3); display: inline-flex; opacity: .55; flex: none; }
.nremote { color: var(--cat-peer); opacity: .9; }
.nmeta { font-size: 11px; color: var(--ink-3); margin-top: 6px; display: flex; align-items: center; gap: 6px; }
.ndep {
  font-family: var(--font-mono); font-size: 9px; font-weight: 600; letter-spacing: .02em;
  color: var(--cat-peer); background: color-mix(in srgb, var(--cat-peer) 14%, transparent);
  border-radius: 4px; padding: 1px 5px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; max-width: 90px;
}

.health { width: 7px; height: 7px; border-radius: 50%; flex: none; }
.health.ok { background: var(--st-healthy); }
.health.warn { background: var(--warn); }
.health.idle { background: var(--ink-3); opacity: .5; }
.health.err { background: var(--err); }

/* Overlay (Highlight chip) states */
.vf-node.overlay-dim .svc { opacity: .34; filter: saturate(.6); }
.vf-node.overlay-errors .svc { border-color: var(--err); box-shadow: 0 0 0 1.5px var(--err-soft), inset 0 1px 0 var(--hi); }
.vf-node.overlay-limits .svc { border-color: var(--warn); box-shadow: 0 0 0 1.5px var(--warn-soft), inset 0 1px 0 var(--hi); }
.vf-node.overlay-auth .svc { border-color: var(--authc); box-shadow: 0 0 0 1.5px var(--auth-soft), inset 0 1px 0 var(--hi); }

/* Density */
.density-compact .svc { padding: 10px 12px; }
.density-compact .nmeta { margin-top: 4px; font-size: 10.5px; }
.density-comfy .svc { padding: 16px 18px; }
.density-comfy .nmeta { margin-top: 9px; }

/* Handles — hidden dots, lifted on hover/selection */
:deep(.vue-flow__handle) {
  width: 8px; height: 8px; background: var(--edge-strong); border: 2px solid var(--bg);
  opacity: 0; transition: opacity var(--speed) var(--ease);
}
.vf-node:hover :deep(.vue-flow__handle),
.vf-node.selected :deep(.vue-flow__handle) { opacity: .8; }
</style>
