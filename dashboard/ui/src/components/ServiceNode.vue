<script setup>
import { computed, inject } from 'vue'
import { Handle, Position } from '@vue-flow/core'
import { Globe, Zap, Radio, Database, Box, Activity, AlertTriangle, Shield, Layers, Cloud } from 'lucide-vue-next'

// ServiceNode (a.k.a the module/group card) renders one nexus.Module as
// a container of endpoint rows. Module is the outer grouping unit; each
// row is an endpoint. Services appear as separate "dep" nodes on the
// right, reached via edges from the specific row that takes them as a
// handler dependency.
//
// data.name is the group title (module name, or the service name when
// the endpoint was registered outside any nexus.Module). data.service,
// when set, is the OWNING service — used only for the owner chip so the
// row still surfaces which service wrapper the handler declared.
//
// Each op row carries its own VueFlow source handle (id "op:<opName>")
// so edges fan out from the specific row that uses a resource or
// service — the viewer sees, row by row, where each op's lines go.
const props = defineProps(['data'])

// Selection store lives in Architecture; used to highlight the active
// row + emphasise its lines.
const selection = inject('nexus.opSelection', { value: null })
const setOp = inject('nexus.setOp', () => {})
const clearOp = inject('nexus.clearOp', () => {})

// Error-dialog opener. Provided by Architecture so clicking the red
// error badge on a row pops the recent-errors modal.
const openErrors = inject('nexus.openErrors', () => {})

// Module cards now show every endpoint they own — modules with many
// routes (oats_applicant's device module has ~20) need to surface
// the full surface so the operator can see what's there. The
// architecture-layout height estimate (estimateServiceHeight in
// Architecture.vue) walks the same list to size the card; both
// must agree or cards overlap.
const displayed = computed(() => props.data.endpoints || [])
const hidden = computed(() => 0)
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

// group uniquely identifies this card's op selection. In module-based
// grouping, the owning service alone isn't enough to disambiguate
// (two services could share a module name across apps), so selection
// key is (groupKey, op) where groupKey is the card's id.
function groupKey() { return props.data.groupKey || props.data.name }

function isActive(e) {
  const sel = selection.value
  return sel && sel.groupKey === groupKey() && sel.op === opKey(e)
}

function onRowClick(e) {
  if (isActive(e)) {
    clearOp()
    return
  }
  // Selection payload: groupKey pins the active row; owningService
  // surfaces the *Service wrapper the handler declared (for chip /
  // service-dep dimming); resources & serviceDeps drive dep-node
  // highlighting on the right side of the canvas.
  setOp({
    groupKey: groupKey(),
    owningService: props.data.service || '',
    op: opKey(e),
    resources: resources(e),
    serviceDeps: serviceDeps(e),
    // autoRouted: handler took no service dep. Relevant for dep-edge
    // styling — auto-routed ops don't draw an edge to the owning service.
    autoRouted: !!e.ServiceAutoRouted,
  })
}

const hasSelection = computed(() => !!selection.value)

// Module cards don't dim — they're the primary grouping, always
// relevant. Dimming applies only to service-dep and resource nodes
// when an op is selected.
const cardDimmed = computed(() => false)

// Header label + icon. Modules get a Layers icon; service-named groups
// (registered outside any nexus.Module) get a Box icon so operators
// can tell at a glance which is which.
const isModule = computed(() => !!props.data.isModule)
const HeaderIcon = computed(() => (isModule.value ? Layers : Box))
</script>

<template>
  <div class="service-node" :class="{ 'has-selection': hasSelection, dim: cardDimmed, remote: data.remote }">
    <!-- Target handle at the card level for any inbound edge (rare today;
         reserved for future service→service topology). -->
    <Handle type="target" :position="Position.Left" />
    <div class="header" @click.stop="clearOp">
      <component :is="HeaderIcon" :size="13" :stroke-width="2" class="hdr-icon" />
      <span class="kind">{{ isModule ? 'module' : 'service' }}</span>
      <span class="name">{{ data.name }}</span>
      <!-- Remote badge — module lives in a peer deployment; routes
           don't run in this binary. The deployment tag is in the
           tooltip so operators can see WHICH peer at a glance. -->
      <span
        v-if="data.remote"
        class="remote-badge"
        :title="data.deployment ? 'Peer deployment: ' + data.deployment : 'Remote service'"
      >
        <Cloud :size="10" :stroke-width="2" />
        remote
      </span>
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
            v-if="!e.ServiceAutoRouted && data.service"
            class="chip owner"
            :title="'Handler declares *' + data.service + 'Service as a dep'"
          >
            <Box :size="9" :stroke-width="2.2" />
            {{ data.service }}
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
      <div v-if="!total && data.remote" class="empty-item remote-empty">
        <Cloud :size="11" :stroke-width="2" />
        Routes served by {{ data.deployment || 'peer deployment' }}
      </div>
      <div v-else-if="!total" class="empty-item">No endpoints</div>
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
  gap: 8px;
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
.hdr-icon { color: #a5b4fc; flex-shrink: 0; }
.kind {
  font-size: 9.5px;
  font-weight: 700;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: #a5b4fc;
  background: rgba(165, 180, 252, 0.12);
  padding: 1px 6px;
  border-radius: 6px;
}
.name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
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
.empty-item.remote-empty {
  display: flex;
  align-items: center;
  gap: 6px;
  color: var(--text-muted);
  font-style: italic;
}
.empty-item.remote-empty :deep(svg) { color: #60a5fa; flex-shrink: 0; }

/* Remote-card variant — clear visual distinction from local services
   so an operator scanning the topology can tell at a glance which
   modules run in this binary vs which are reached over HTTP. The
   accent color and dashed border mirror the "external" feeling of
   the InternetNode without being so different they break visual
   continuity with local cards. */
.service-node.remote {
  border-style: dashed;
  border-color: #60a5fa;
  background: linear-gradient(180deg, var(--bg-card) 0%, rgba(96, 165, 250, 0.04) 100%);
}
.service-node.remote .header {
  background: #1e3a5f;
}
.service-node.remote .header:hover { background: #243b5e; }
.remote-badge {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  font-size: 9.5px;
  font-weight: 700;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: #93c5fd;
  background: rgba(96, 165, 250, 0.18);
  padding: 1px 6px;
  border-radius: 6px;
}
.remote-badge :deep(svg) { color: #93c5fd; }

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
