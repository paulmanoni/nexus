<script setup>
import { computed, inject } from 'vue'
import { Handle, Position } from '@vue-flow/core'
import { Globe, Zap, Radio, Database, Box, Activity, AlertTriangle, Shield } from 'lucide-vue-next'

// ServiceNode renders one service card on the Architecture canvas. Each
// op row carries its own VueFlow source handle (id "op:<opName>") so
// edges can fan out from the specific row that uses a resource — the
// viewer sees, row by row, where each op's lines go.
const props = defineProps(['data'])

// Selection store lives in Architecture; used to highlight the active
// row + emphasise its lines.
const selection = inject('nexus.opSelection', { value: null })
const setOp = inject('nexus.setOp', () => {})
const clearOp = inject('nexus.clearOp', () => {})

// Error-dialog opener. Provided by Architecture so clicking the red
// error badge on a row pops the recent-errors modal.
const openErrors = inject('nexus.openErrors', () => {})

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

function opKey(e) {
  return e.Name || `${e.Method} ${e.Path}`
}

function resources(e) {
  return Array.isArray(e.Resources) ? e.Resources : []
}

function serviceDeps(e) {
  return Array.isArray(e.ServiceDeps) ? e.ServiceDeps : []
}

// middlewareNames filters out the framework-owned "metrics" recorder —
// every op has it, so a chip per row would be pure noise. Others (auth,
// permission, rate-limit, custom) are meaningful per-op and shown as
// chips.
function middlewareNames(e) {
  const all = Array.isArray(e.Middleware) ? e.Middleware : []
  return all.filter(m => m !== 'metrics')
}

// Stats accessors. Architecture.vue attaches a Stats object on each
// endpoint (Count, Errors, LastError, LastAt, LastErrAt). Missing when
// the op hasn't been hit yet.
function stats(e) { return e.Stats || null }
function hasErrors(e) { const s = stats(e); return s && s.errors > 0 }
function tooltipForStats(e) {
  const s = stats(e)
  if (!s) return 'no requests yet'
  const parts = [`${s.count} requests`, `${s.errors} errors`]
  if (s.lastError) parts.push(`last error: ${s.lastError}`)
  return parts.join('\n')
}

function isActive(e) {
  const sel = selection.value
  return sel && sel.service === props.data.name && sel.op === opKey(e)
}

function onRowClick(e) {
  if (isActive(e)) {
    clearOp()
    return
  }
  // Selection payload carries both resource and service targets so the
  // canvas can dim non-matching nodes of either kind.
  setOp({
    service: props.data.name,
    op: opKey(e),
    resources: resources(e),
    serviceDeps: serviceDeps(e),
  })
}

const hasSelection = computed(() => !!selection.value)

// When the canvas has an op selected, non-owning service cards dim unless
// they're in the selected op's ServiceDeps.
const cardDimmed = computed(() => {
  const sel = selection.value
  if (!sel) return false
  if (sel.service === props.data.name) return false          // owning
  const deps = Array.isArray(sel.serviceDeps) ? sel.serviceDeps : []
  return !deps.includes(props.data.name)
})
</script>

<template>
  <div class="service-node" :class="{ 'has-selection': hasSelection, dim: cardDimmed }">
    <!-- Target handle at the card level for any inbound edge (rare today;
         reserved for future service→service topology). -->
    <Handle type="target" :position="Position.Left" />
    <div class="header" @click.stop="clearOp">
      <span class="name">{{ data.name }}</span>
      <span class="total">{{ total }}</span>
    </div>
    <div v-if="data.description" class="desc">{{ data.description }}</div>
    <div class="endpoints">
      <div
        v-for="(e, i) in displayed"
        :key="i"
        class="row"
        :class="[e.Transport, { active: isActive(e) }]"
        :data-op="opKey(e)"
        @click.stop="onRowClick(e)"
      >
        <div class="item">
          <component :is="iconFor(e.Transport)" :size="11" :stroke-width="2" class="icon" />
          <span class="label">{{ label(e) }}</span>
          <!-- Per-op metrics badges, right-aligned so they don't
               compete with the op name but are visible at a glance. -->
          <span v-if="stats(e)" class="stat req" :title="tooltipForStats(e)">
            <Activity :size="9" :stroke-width="2.2" />
            {{ stats(e).count }}
          </span>
          <button
            v-if="hasErrors(e)"
            class="stat err clickable"
            :title="tooltipForStats(e) + ' · click for details'"
            @click.stop="openErrors({ service: data.name, op: opKey(e) })"
          >
            <AlertTriangle :size="9" :stroke-width="2.2" />
            {{ stats(e).errors }}
          </button>
        </div>
        <div v-if="!e.ServiceAutoRouted || resources(e).length || middlewareNames(e).length" class="chips">
          <!-- Owner chip mirrors real signature deps: it appears when the
               handler explicitly takes the service wrapper as a dep (so
               the chip matches the resource chips, which also reflect
               declared deps). Auto-routed ops — where the auto-mount
               adopted the op without the handler declaring the service
               — do NOT get a chip, because the function didn't name it. -->
          <span
            v-if="!e.ServiceAutoRouted"
            class="chip owner"
            :title="'Handler declares *' + data.name + 'Service as a dep'"
          >
            <Box :size="9" :stroke-width="2.2" />
            {{ data.name }}
          </span>
          <span
            v-for="r in resources(e)"
            :key="r"
            class="chip"
            :title="'Resource: ' + r"
          >
            <Database :size="9" :stroke-width="2.2" />
            {{ r }}
          </span>
          <!-- Per-op middleware chips. Distinct style (warm tint) so
               they read as "protection" rather than "dependency". -->
          <span
            v-for="m in middlewareNames(e)"
            :key="'mw:' + m"
            class="chip mw"
            :title="'Middleware: ' + m"
          >
            <Shield :size="9" :stroke-width="2.2" />
            {{ m }}
          </span>
        </div>
        <!-- Per-op source handle. Architecture builds edges using
             sourceHandle: 'op:<opName>' so the line anchors to this row. -->
        <Handle
          type="source"
          :position="Position.Right"
          :id="'op:' + opKey(e)"
          class="op-handle"
        />
      </div>
      <div v-if="hidden > 0" class="more">+ {{ hidden }} more</div>
      <div v-if="!total" class="empty-item">No endpoints</div>
    </div>
    <!-- Fallback service-level source handle for runtime-only attachments
         (resources attached via OnResourceUse but not claimed by any op). -->
    <Handle type="source" :position="Position.Right" id="svc" />
  </div>
</template>

<style scoped>
.service-node {
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: 10px;
  min-width: 260px;
  max-width: 300px;
  color: var(--text);
  box-shadow: var(--shadow-md);
  overflow: visible;       /* handles stick out — don't clip them */
  font-family: var(--font-sans);
  position: relative;
  transition: opacity 120ms;
}
.service-node.dim { opacity: 0.35; }
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
  cursor: pointer;
  border-top-left-radius: 10px;
  border-top-right-radius: 10px;
}
.header:hover { background: #1f2937; }
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
.endpoints { padding: 6px 6px 8px; display: flex; flex-direction: column; gap: 2px; }
.row {
  padding: 4px 8px 4px 10px;
  border-radius: 4px;
  cursor: pointer;
  transition: background 120ms, opacity 120ms;
  position: relative;       /* for the op-handle pinned to right edge */
}
.row:hover { background: var(--bg-hover); }
.row.active {
  background: var(--bg-active);
  outline: 1px solid var(--accent);
}
.has-selection .row:not(.active) { opacity: 0.45; }
.item {
  display: flex;
  align-items: center;
  gap: 8px;
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--text);
}
.icon { flex-shrink: 0; }
.row.rest .icon { color: var(--rest); }
.row.graphql .icon { color: var(--graphql); }
.row.websocket .icon { color: var(--ws); }
.label {
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  flex: 1;
  min-width: 0;
}

/* Per-op stat badges. Small, tabular-numeric so they don't jitter as
   counts grow. Red tint for error counts so they stand out on busy cards. */
.stat {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  font-family: var(--font-mono);
  font-size: 10px;
  font-variant-numeric: tabular-nums;
  padding: 1px 6px 1px 5px;
  border-radius: 8px;
  line-height: 1.4;
  flex-shrink: 0;
}
.stat.req { background: var(--bg-hover); color: var(--text-muted); }
.stat.err { background: var(--error-soft); color: var(--error); font-weight: 600; border: 1px solid transparent; }
.stat.clickable {
  cursor: pointer;
  padding: 1px 6px 1px 5px; /* keep sizing identical to non-clickable stat */
  font-family: var(--font-mono);
}
.stat.clickable:hover {
  border-color: var(--error);
  background: #fee2e2;
}
.chips {
  margin-top: 3px;
  padding-left: 19px;
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}
.chip {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  font-family: var(--font-mono);
  font-size: 10px;
  padding: 1px 6px 1px 5px;
  border-radius: 8px;
  background: var(--bg-active);
  color: var(--accent);
  border: 1px solid transparent;
  line-height: 1.5;
}
/* Owning-service chip: darker, reads as scope rather than dependency. */
.chip.owner {
  background: #111827;
  color: #f9fafb;
  font-weight: 600;
}
/* Middleware chip: warm amber so it distinguishes from resource (blue)
   and owner (black). Shield icon reinforces "protection" semantics. */
.chip.mw {
  background: #fef3c7;
  color: #92400e;
  border: 1px solid #fde68a;
}
.more, .empty-item {
  padding: 4px 10px;
  color: var(--text-dim);
  font-size: 11px;
  font-family: var(--font-sans);
}

/* Per-op handle: small dot on the right edge of each row. VueFlow auto-
   positions handles at the node boundary; override so each row anchors
   its own line. */
:deep(.op-handle) {
  top: 50% !important;
  right: -4px !important;
  transform: translateY(-50%);
  width: 8px;
  height: 8px;
  background: var(--accent);
  border: 2px solid var(--bg);
  opacity: 0.85;
}
.row:hover :deep(.op-handle) { opacity: 1; width: 10px; height: 10px; }

/* Fallback service handle sits at the card centre-right, transparent by
   default — only matters when the fallback edge actually uses it. */
:deep(.service-node > .vue-flow__handle-right) {
  opacity: 0;
  pointer-events: none;
}
</style>
