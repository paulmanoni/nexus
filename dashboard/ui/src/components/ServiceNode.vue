<script setup>
import { computed, inject } from 'vue'
import { Handle, Position } from '@vue-flow/core'
import { Globe, Zap, Radio, Database, Box, Activity, AlertTriangle, Shield, Cloud, Link2, Gauge } from 'lucide-vue-next'
import CategoryIcon from './CategoryIcon.vue'
import DeploymentTag from './DeploymentTag.vue'

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

// Drawer plumbing — clicking a row opens the per-op detail drawer in
// addition to selecting it on the canvas. Toggle off (re-clicking the
// active row) clears selection AND closes the drawer.
const openDrawer = inject('nexus.openDrawer', () => {})
const closeDrawer = inject('nexus.closeDrawer', () => {})

// Overlay state — Set of active highlight modes from OverlayToggles.
// We use it to decide which rows pop and which fade.
const overlays = inject('nexus.overlays', { value: new Set() })

// rowOverlays returns the active overlay ids that this specific row
// satisfies. When at least one overlay is active and a row matches
// none of them, it dims; when it matches at least one, it ALSO gets a
// coloured ring class for the strongest matched overlay. Order of
// preference (errors > limits > auth) when multiple match — errors
// reads as the most actionable signal.
function rowOverlays(e) {
  if (!overlays.value.size) return []
  const matched = []
  if (overlays.value.has('errors') && (e.Stats?.errors || 0) > 0) matched.push('errors')
  if (overlays.value.has('limits') && e.RateLimit && e.RateLimit.rpm > 0) matched.push('limits')
  if (overlays.value.has('auth')) {
    const mw = Array.isArray(e.Middleware) ? e.Middleware : []
    if (mw.some(m => m === 'auth' || m.startsWith('auth') || m.startsWith('permission'))) {
      matched.push('auth')
    }
  }
  return matched
}
function rowOverlayClass(e) {
  if (!overlays.value.size) return ''
  const matched = rowOverlays(e)
  if (!matched.length) return 'overlay-dim'
  // Top-priority overlay wins the ring colour.
  return 'overlay-' + matched[0]
}

// Error-dialog opener. Provided by Architecture so clicking the red
// error badge on a row pops the recent-errors modal.
const openErrors = inject('nexus.openErrors', () => {})

// Architecture.vue does the sort + slice — `data.endpoints` is the
// visible subset, `data.totalEndpoints` is the full count, and
// `data.isExpanded` says whether the +N more toggle is currently open.
// ServiceNode just renders what it's given and emits toggle clicks
// back via the injected helper.
const displayed = computed(() => props.data.endpoints || [])
const total = computed(() => props.data.totalEndpoints ?? displayed.value.length)
const hidden = computed(() => Math.max(total.value - displayed.value.length, 0))
const isExpanded = computed(() => !!props.data.isExpanded)
const toggleExpanded = inject('nexus.toggleExpanded', () => {})

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
    closeDrawer()
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
  // Open the per-op drawer keyed by service.opName so it stays live
  // against subsequent /__nexus/live snapshots.
  openDrawer({ kind: 'op', key: `${e.Service}.${e.Name || opKey(e)}` })
}

const hasSelection = computed(() => !!selection.value)

// Module cards don't dim — they're the primary grouping, always
// relevant. Dimming applies only to service-dep and resource nodes
// when an op is selected.
const cardDimmed = computed(() => false)

// Header label + icon. The card represents a MODULE — the container
// of endpoints. Service is a separate organizational unit (the wrapper
// the handler took as a dep); when it differs from the module name it
// surfaces as a sub-subtitle below the kind line. The CategoryIcon
// reflects the kind: stacked-boxes for modules, single box for the
// service-only case (group registered outside any nexus.Module).
const isModule = computed(() => !!props.data.isModule)
const headerKind = computed(() => (isModule.value ? 'module' : 'service'))

// displayName uses the MODULE name as the card's primary label.
// Mixed-service modules + bare-service groups fall back to data.name
// (which is set to the service name when isModule is false).
const displayName = computed(() => props.data.name || props.data.service || '')

// hasServiceSubtitle reveals when the module's owning service has a
// distinct name worth showing — gives readers the service identity
// without making it the card title.
const hasServiceSubtitle = computed(() => {
  const s = props.data.service
  const n = props.data.name
  return !!(s && n && s !== n && props.data.isModule)
})
</script>

<template>
  <div class="service-node" :class="{ 'has-selection': hasSelection, dim: cardDimmed, remote: data.remote }">
    <!-- Target handle at the card level for any inbound edge (rare today;
         reserved for future service→service topology). -->
    <Handle type="target" :position="Position.Left" />
    <div class="header" @click.stop="clearOp">
      <CategoryIcon :type="headerKind" :size="32" />
      <div class="title">
        <div class="name-row">
          <span class="name">{{ displayName }}</span>
          <DeploymentTag v-if="data.deployment" :name="data.deployment" />
          <span
            v-if="data.remote"
            class="remote-badge"
            :title="data.deployment ? 'Peer deployment: ' + data.deployment : 'Remote service'"
          >
            <Cloud :size="10" :stroke-width="2" />
            remote
          </span>
        </div>
        <div class="kind">
          {{ headerKind }} · {{ total }} {{ total === 1 ? 'endpoint' : 'endpoints' }}
          <span v-if="hasServiceSubtitle" class="in-module">· {{ data.service }} service</span>
        </div>
      </div>
    </div>
    <div v-if="data.description" class="desc">{{ data.description }}</div>
    <div class="endpoints">
      <div
        v-for="(e, i) in displayed"
        :key="i"
        class="row"
        :class="[e.Transport, rowOverlayClass(e), { active: isActive(e) }]"
        :data-op="opKey(e)"
        @click.stop="onRowClick(e)"
      >
        <div class="item">
          <!-- Transport tile — tiny CategoryIcon-style square, hue tied
               to the transport (REST=green, GraphQL=pink, WS=amber) so
               row scanning reads at a glance. The 16px Lucide glyph
               inside identifies the transport when color isn't enough. -->
          <span class="t-tile">
            <component :is="iconFor(e.Transport)" :size="10" :stroke-width="2.2" />
          </span>
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
            @click.stop="openErrors({ service: e.Service, op: opKey(e) })"
          >
            <AlertTriangle :size="9" :stroke-width="2.2" />
            {{ stats(e).errors }}
          </button>
          <!-- Rate-limit chip — visible only when the op declared a
               limit at registration time. Hover for full RPM/burst/
               per-IP detail; click goes through the row click handler
               which opens the drawer's Rate limit section. -->
          <span
            v-if="e.RateLimit && e.RateLimit.rpm"
            class="stat rl"
            :title="`${e.RateLimit.rpm} RPM`
              + (e.RateLimit.burst ? ` · burst ${e.RateLimit.burst}` : '')
              + (e.RateLimit.perIP ? ' · per-IP' : '')"
          >
            <Gauge :size="9" :stroke-width="2.2" />
            {{ e.RateLimit.rpm }}/m
          </span>
        </div>
        <div
          v-if="!e.ServiceAutoRouted || resources(e).length || serviceDeps(e).length || middlewareNames(e).length"
          class="chips"
        >
          <!-- Owner-service chip — when the handler explicitly takes the
               service wrapper as a dep. Filled to read as "this row IS
               part of this service." Two suppressions:
                 1. Auto-routed ops: the function didn't name the
                    service, so don't fake one.
                 2. The owner equals the parent card's title (the common
                    case: module "users" wraps service "users"). The
                    chip would just repeat what the header already says,
                    adding noise without information. We still SHOW it
                    when the owner differs from the card title — e.g. a
                    "core" module wrapping users + articles services,
                    where each row's owner is meaningful per-row. -->
          <span
            v-if="!e.ServiceAutoRouted && (data.service || e.Service) && (data.service || e.Service) !== data.name"
            class="chip owner"
            :title="'Handler declares *' + (data.service || e.Service) + 'Service as a dep'"
          >
            <Box :size="9" :stroke-width="2.2" />
            {{ data.service || e.Service }}
          </span>
          <!-- Service-dep chips — OTHER services this endpoint calls
               into (handler took *FooService as a dep, where Foo isn't
               its own owning service). Outlined service hue so it's
               distinct from the filled owner chip and the indigo-tinted
               resource chips. -->
          <span
            v-for="s in serviceDeps(e)"
            :key="'svc:' + s"
            class="chip svc-dep"
            :title="'Calls service: ' + s"
          >
            <Link2 :size="9" :stroke-width="2.2" />
            {{ s }}
          </span>
          <span
            v-for="r in resources(e)"
            :key="r"
            class="chip res"
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
        <!-- Per-op target handle (left). Inbound edges from Internet
             use targetHandle: 'op:<opName>' so traffic LANDS on the
             specific row instead of the card centre. -->
        <Handle
          type="target"
          :position="Position.Left"
          :id="'op:' + opKey(e)"
          class="op-target-handle"
        />
        <!-- Per-op source handle (right). Outbound edges set
             sourceHandle: 'op:<opName>' so the line LEAVES from this
             row toward resources / service deps. -->
        <Handle
          type="source"
          :position="Position.Right"
          :id="'op:' + opKey(e)"
          class="op-handle"
        />
      </div>
      <button
        v-if="hidden > 0 && !isExpanded"
        class="more"
        @click.stop="toggleExpanded(data.groupKey)"
      >
        + {{ hidden }} more {{ hidden === 1 ? 'endpoint' : 'endpoints' }}
      </button>
      <button
        v-if="isExpanded && total > 0"
        class="more collapse"
        @click.stop="toggleExpanded(data.groupKey)"
      >
        Show fewer
      </button>
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
  border-radius: var(--radius-md);
  min-width: 260px;
  max-width: 300px;
  color: var(--text);
  box-shadow: var(--shadow-md);
  overflow: visible;       /* handles stick out — don't clip them */
  font-family: var(--font-sans);
  position: relative;
  transition: opacity 120ms, box-shadow 120ms, border-color 120ms;
}
.service-node:hover { box-shadow: var(--shadow-lg); }
.service-node.dim { opacity: 0.35; }
.header {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-3);
  background: var(--bg-card);
  color: var(--text);
  cursor: pointer;
  border-top-left-radius: var(--radius-md);
  border-top-right-radius: var(--radius-md);
  border-bottom: 1px solid var(--border);
  transition: background 120ms;
}
.header:hover { background: var(--bg-hover); }
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
  gap: var(--space-2);
  min-width: 0;
}
.name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: var(--fs-lg);
  font-weight: 600;
  color: var(--text);
  letter-spacing: -0.005em;
}
.kind {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-dim);
  text-transform: lowercase;
  letter-spacing: 0.02em;
}
/* Module sub-suffix on the kind line — appears only when the service
   name and module name differ, so the module-grouping context isn't
   lost when we lead the card with the service identity. */
.in-module { color: var(--text-dim); opacity: 0.85; }
.desc {
  padding: var(--space-2) var(--space-3);
  color: var(--text-muted);
  font-size: var(--fs-sm);
  border-bottom: 1px solid var(--border);
  line-height: var(--lh-body);
}
.endpoints {
  padding: var(--space-2);
  display: flex;
  flex-direction: column;
  gap: 2px;
  /* No max-height + scroll — Architecture.vue caps the visible row
     count at MAX_VISIBLE_ENDPOINTS and offers a "+N more" toggle.
     The card grows naturally to the height of the visible rows so
     dagre can measure it deterministically; per-op edge handles
     never desync from scrolled-out rows. */
}

/* Endpoint row — designed to read like an AWS list-row: an accent strip
   on the left calls out state (transport color when idle, brand accent
   when active), main content fills the row, badges right-align. */
.row {
  padding: 6px 8px 6px 12px;
  border-radius: var(--radius-sm);
  cursor: pointer;
  transition: background 120ms, opacity 120ms, box-shadow 120ms;
  position: relative;       /* anchor for the left accent strip + handle */
}
.row::before {
  content: '';
  position: absolute;
  left: 3px;
  top: 6px;
  bottom: 6px;
  width: 2px;
  border-radius: 2px;
  background: transparent;
  transition: background 120ms;
}
.row:hover { background: var(--bg-hover); }
.row:hover::before { background: var(--border-strong); }
.row.active {
  background: var(--bg-active);
}
.row.active::before { background: var(--accent); }
.has-selection .row:not(.active) { opacity: 0.45; }
/* Overlay highlight states — when OverlayToggles has any chip active,
   non-matching rows fade and matching rows get a coloured ring +
   coloured left bar. Selection still wins (selection overrides
   overlay dimming so the user's clicked row stays prominent). */
.row.overlay-dim:not(.active) { opacity: 0.32; }
.row.overlay-errors {
  background: color-mix(in srgb, var(--st-error) 6%, transparent);
}
.row.overlay-errors::before { background: var(--st-error); }
.row.overlay-limits {
  background: color-mix(in srgb, var(--cat-cron) 6%, transparent);
}
.row.overlay-limits::before { background: var(--cat-cron); }
.row.overlay-auth {
  background: color-mix(in srgb, var(--st-warn) 6%, transparent);
}
.row.overlay-auth::before { background: var(--st-warn); }

.item {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text);
}

/* Transport tile — same visual grammar as CategoryIcon at miniature size.
   Tinted background + tinted glyph keeps a row's transport readable at
   a glance without the row needing a wide left bar. */
.t-tile {
  display: inline-grid;
  place-items: center;
  width: 18px;
  height: 18px;
  border-radius: var(--radius-sm);
  flex-shrink: 0;
}
.row.rest      .t-tile { background: var(--rest-soft);    color: var(--rest); }
.row.graphql   .t-tile { background: var(--graphql-soft); color: var(--graphql); }
.row.websocket .t-tile { background: var(--ws-soft);      color: var(--ws); }

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
  padding: 2px 7px;
  border-radius: 999px;
  line-height: 1.4;
  flex-shrink: 0;
  border: 1px solid transparent;
}
.stat.req { background: var(--bg-hover); color: var(--text-muted); }
.stat.err {
  background: var(--st-error-soft);
  color: var(--st-error);
  font-weight: 600;
}
/* Rate-limit chip — same pill shape, tinted in the cron/throttle
   pink so a busy row scan reads "this row has a guard" at a glance. */
.stat.rl {
  background: color-mix(in srgb, var(--cat-cron) 12%, transparent);
  color: var(--cat-cron);
}
.stat.clickable { cursor: pointer; }
.stat.clickable:hover {
  border-color: var(--st-error);
  background: color-mix(in srgb, var(--st-error) 15%, transparent);
}

.chips {
  margin-top: 4px;
  padding-left: 28px; /* aligns under label, past the 18px tile + 8px gap + 2px breathing */
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}
.chip {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-family: var(--font-mono);
  font-size: 10px;
  padding: 2px 7px;
  border-radius: 999px;
  background: var(--bg-hover);
  color: var(--text-muted);
  border: 1px solid transparent;
  line-height: 1.4;
}
/* Resource chip — indigo-tinted. Identifies which DB/cache/queue the
   row touches; pairs with the resource node's category color on the
   right side of the canvas. */
.chip.res {
  background: color-mix(in srgb, var(--cat-database) 10%, transparent);
  color: var(--cat-database);
  border-color: color-mix(in srgb, var(--cat-database) 20%, transparent);
}
/* Service-dep chip — outlined service hue. Distinct from the FILLED
   owner chip ("I AM this service") and from resource chips (data, not
   compute). Reads as "this row CALLS this other service." */
.chip.svc-dep {
  background: color-mix(in srgb, var(--cat-service) 8%, transparent);
  color: var(--cat-service);
  border-color: color-mix(in srgb, var(--cat-service) 30%, transparent);
}
/* Owning-service chip: filled with the service category color so it
   reads as scope rather than a generic dependency. Same hue as the
   header CategoryIcon — visual continuity inside the card. */
.chip.owner {
  background: var(--cat-service);
  color: white;
  font-weight: 600;
  border-color: var(--cat-service);
}
/* Middleware chip: amber tint (status warn family) so it distinguishes
   from resource (indigo) and owner (filled). Shield icon reinforces
   "protection" semantics. */
.chip.mw {
  background: var(--st-warn-soft);
  color: #92400e;
  border-color: color-mix(in srgb, var(--st-warn) 30%, transparent);
}
.empty-item {
  padding: 4px 10px;
  color: var(--text-dim);
  font-size: 11px;
  font-family: var(--font-sans);
}

/* +N more / Show fewer toggle — full-width subtle button. Dashed
   border + low-contrast colour so it reads as an affordance without
   competing with real op rows. Hover lifts to the brand accent. */
.more {
  margin: 6px 4px 0;
  padding: 5px 10px;
  font-family: var(--font-sans);
  font-size: var(--fs-xs);
  font-weight: 500;
  color: var(--text-muted);
  background: transparent;
  border: 1px dashed var(--border-strong);
  border-radius: var(--radius-sm);
  cursor: pointer;
  transition: background 120ms, border-color 120ms, color 120ms;
}
.more:hover {
  background: var(--bg-hover);
  border-color: var(--accent);
  color: var(--accent);
}
.more.collapse {
  border-style: solid;
  border-color: var(--border);
}
.more.collapse:hover {
  border-color: var(--accent);
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
   "peer" category color (sky) + dashed border mirrors the external-
   service feel without breaking visual continuity with local cards. */
.service-node.remote {
  border-style: dashed;
  border-color: var(--cat-peer);
  background: color-mix(in srgb, var(--cat-peer) 3%, var(--bg-card));
}
.service-node.remote .header {
  background: color-mix(in srgb, var(--cat-peer) 6%, var(--bg-card));
}
.service-node.remote .header:hover {
  background: color-mix(in srgb, var(--cat-peer) 10%, var(--bg-card));
}
.remote-badge {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  font-size: var(--fs-xs);
  font-weight: 600;
  letter-spacing: 0.04em;
  text-transform: uppercase;
  color: var(--cat-peer);
  background: color-mix(in srgb, var(--cat-peer) 15%, transparent);
  padding: 1px 6px;
  border-radius: var(--radius-sm);
  flex-shrink: 0;
}
.remote-badge :deep(svg) { color: var(--cat-peer); }

/* Per-op handles — small dots flanking each row. VueFlow auto-positions
   handles at the node boundary; override so each row anchors its own
   line on both sides (inbound from Internet on the left, outbound to
   resources / service deps on the right). Hidden by default; lifted on
   hover so a curious user can confirm the anchor exists without seeing
   25 dots stacked when scanning a busy module card. */
:deep(.op-handle),
:deep(.op-target-handle) {
  top: 50% !important;
  transform: translateY(-50%);
  width: 8px;
  height: 8px;
  background: var(--accent);
  border: 2px solid var(--bg);
  opacity: 0;
  transition: opacity 120ms;
}
:deep(.op-handle)        { right: -4px !important; }
:deep(.op-target-handle) { left:  -4px !important; background: var(--cat-internet); }
.row:hover :deep(.op-handle),
.row:hover :deep(.op-target-handle),
.row.active :deep(.op-handle),
.row.active :deep(.op-target-handle) { opacity: 0.85; }

/* Fallback service handle sits at the card centre-right, transparent by
   default — only matters when the fallback edge actually uses it. */
:deep(.service-node > .vue-flow__handle-right) {
  opacity: 0;
  pointer-events: none;
}
</style>
