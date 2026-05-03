<script setup>
import { ref, computed, onMounted, onUnmounted, markRaw, nextTick, provide, watch } from 'vue'
import { VueFlow, useVueFlow, Position, MarkerType } from '@vue-flow/core'
import { Background } from '@vue-flow/background'
import { Controls } from '@vue-flow/controls'
import dagre from 'dagre'
import { ShieldCheck } from 'lucide-vue-next'

import ServiceNode from '../components/ServiceNode.vue'
import ServiceDepNode from '../components/ServiceDepNode.vue'
import WorkerNode from '../components/WorkerNode.vue'
import CronNode from '../components/CronNode.vue'
import ResourceNode from '../components/ResourceNode.vue'
import InternetNode from '../components/InternetNode.vue'
import ErrorDialog from '../components/ErrorDialog.vue'
import PacketOverlay from '../components/PacketOverlay.vue'
import GlobalMiddlewareBar from '../components/GlobalMiddlewareBar.vue'
import ActivityRail from '../components/ActivityRail.vue'
import OverlayToggles from '../components/OverlayToggles.vue'
import TimeScrubber from '../components/TimeScrubber.vue'
import Drawer from '../components/Drawer.vue'
import OpDetail from '../components/drawer/OpDetail.vue'
import ResourceDetail from '../components/drawer/ResourceDetail.vue'
import WorkerDetail from '../components/drawer/WorkerDetail.vue'
import CronDetail from '../components/drawer/CronDetail.vue'
import AuthDetail from '../components/drawer/AuthDetail.vue'
import CmdK from '../components/CmdK.vue'
import { subscribeEvents, subscribeLive } from '../lib/api.js'

const nodes = ref([])
const edges = ref([])
// Node types: "service" now renders the module/group card (name retained
// for back-compat with packet animator CSS selectors); "serviceDep" is
// the small dep-node on the right for services consumed by endpoints.
const nodeTypes = {
  service: markRaw(ServiceNode),
  serviceDep: markRaw(ServiceDepNode),
  worker: markRaw(WorkerNode),
  cron: markRaw(CronNode),
  resource: markRaw(ResourceNode),
  internet: markRaw(InternetNode),
}

// INTERNET_ID is the fixed id of the single "Clients" node. Keep it a
// constant so edge-builders and the traffic animator agree on naming.
const INTERNET_ID = 'internet'

// MAX_VISIBLE_ENDPOINTS caps rows shown per module card by default. Top
// N by traffic (alphabetical tiebreak) appear; the rest hide behind a
// "+N more endpoints" toggle. Without the cap, modules with many ops
// turned the canvas into a wall of text and edges anchored to scrolled-
// out rows desynced from their handles.
const MAX_VISIBLE_ENDPOINTS = 12

// expandedGroups holds the set of group-keys whose cards are currently
// rendering ALL their endpoints (toggle clicked). Survives WS snapshots
// because it lives outside latestSnapshot. Mutating triggers a load()
// rerun so dagre re-lays-out around the now-taller card.
const expandedGroups = ref(new Set())
function toggleExpanded(groupKey) {
  if (!groupKey) return
  // Replace the Set wholesale so Vue's reactivity fires; mutating the
  // existing Set in place doesn't trigger watchers.
  const next = new Set(expandedGroups.value)
  if (next.has(groupKey)) next.delete(groupKey)
  else next.add(groupKey)
  expandedGroups.value = next
}
provide('nexus.expandedGroups', expandedGroups)
provide('nexus.toggleExpanded', toggleExpanded)

// Per-op selection store. ServiceNode writes here on click; ResourceNode
// + edge-styling read from it. Single source of truth means no props need
// to thread through the VueFlow custom-node API.
const opSelection = ref(null)  // { service, op, resources: string[] }

function setOp(sel) {
  opSelection.value = sel
}
function clearOp() {
  opSelection.value = null
}
provide('nexus.opSelection', opSelection)
provide('nexus.setOp', setOp)
provide('nexus.clearOp', clearOp)

// Overlay highlight modes — Set of active overlay ids ('errors',
// 'limits', 'auth'). Op rows + edges read this to decide whether to
// dim themselves (no overlay matches) or pop with a coloured ring
// (matches an active overlay). OR semantics across overlays.
const overlays = ref(new Set())
function setOverlay(next) { overlays.value = next instanceof Set ? next : new Set(next) }
provide('nexus.overlays', overlays)
provide('nexus.setOverlay', setOverlay)

// Time scrubber state — capture every WS snapshot into a ring buffer
// so the user can rewind the canvas to a recent moment. scrubIndex =
// null means "follow live"; an integer pins the canvas at that
// history index.
//
// Capacity: 30 minutes at the 2s server snapshotInterval. ~900 frames,
// each typically 5-15 KB → 5-15 MB of in-tab memory for a real app.
// That's the practical sweet spot for dev/debug sessions: long enough
// to rewind to "what was the state when the bug fired?" without paying
// for hour-plus windows we'd more cleanly back with server-side
// per-bucket storage. Bump SCRUB_HISTORY_CAP if you need longer.
const SCRUB_HISTORY_CAP = 900
const snapshotHistory = ref([])
const scrubIndex = ref(null)

// Event history ring buffer — every trace event the WS streams in
// gets stashed with its timestamp so the scrubber can REPLAY the
// flashes + packet animations that happened at the moment the user
// rewinds to. Without this, scrubbing back swaps the snapshot but
// leaves the canvas eerily idle, hiding what was actually flowing.
//
// Cap covers a busy app for 30 min at modest event rates; oldest
// frames evict first when the cap is hit.
const EVENT_HISTORY_CAP = 5000
const eventHistory = ref([])
// SCRUB_REPLAY_WINDOW_MS is how far back from the pinned snapshot's
// timestamp we look for events to replay. Matches the 2s server
// snapshot interval so each frame replays the events that were
// fresh AT that moment.
const SCRUB_REPLAY_WINDOW_MS = 2000

function setScrubIndex(idx) {
  scrubIndex.value = idx
  // Always wipe in-flight flashes / packet timers when changing
  // scrub state — leftovers from the previous frame would otherwise
  // bleed into the new one. Both paths (resume live + pin past) need
  // this clear.
  flashedEdges.value = new Map()
  flashTimers.forEach(t => clearTimeout(t))
  flashTimers.clear()
  if (idx === null) {
    const last = snapshotHistory.value[snapshotHistory.value.length - 1]
    if (last) latestSnapshot.value = last.snap
  } else {
    const e = snapshotHistory.value[idx]
    if (e) {
      latestSnapshot.value = e.snap
      // Replay every event whose timestamp falls inside the snapshot's
      // window. nextTick ensures the layout has rebuilt so flashEdges
      // / spawnPacketsForEdges find the right edge SVG paths to ride.
      nextTick(() => replayEventsAt(e.ts))
    }
  }
  if (latestSnapshot.value) load()
}

function replayEventsAt(targetTimeMs) {
  if (!eventHistory.value.length) return
  const lo = targetTimeMs - SCRUB_REPLAY_WINDOW_MS
  const hi = targetTimeMs
  for (const ev of eventHistory.value) {
    if (!ev.timestamp) continue
    const evTime = new Date(ev.timestamp).getTime()
    if (!evTime) continue
    if (evTime >= lo && evTime <= hi) {
      // Force-replay flag: bypass onTraceEvent's "skip backlog older
      // than mount" filter, which exists to avoid replaying old
      // events on initial subscribe but is exactly what we want here.
      onTraceEvent(ev, true)
    }
  }
}

provide('nexus.scrubHistory', snapshotHistory)
provide('nexus.scrubIndex', scrubIndex)
provide('nexus.setScrubIndex', setScrubIndex)

// Drawer store. The drawer is the single click-to-open detail surface
// for any node — kicks in when the user clicks an endpoint row, and
// (eventually) resource/worker/cron cards. Held as { kind, key } so the
// content stays *live* against subsequent /__nexus/live snapshots
// instead of snapshotting the payload at click time.
const drawer = ref(null) // { kind: 'op', key: 'svc.opName' }
function openDrawer(spec) { drawer.value = spec }
function closeDrawer() { drawer.value = null }
provide('nexus.openDrawer', openDrawer)
provide('nexus.closeDrawer', closeDrawer)

// Each drawer kind re-resolves its target from the latest snapshot
// every time the WS pump pushes a new frame. Stats / health / status
// stay current; if the target disappears between snapshots (deployment
// change), the drawer renders nothing and the user can close it.
const drawerOp = computed(() => {
  if (drawer.value?.kind !== 'op') return null
  const snap = latestSnapshot.value
  if (!snap) return null
  const want = drawer.value.key
  for (const e of snap.endpoints || []) {
    const k = `${e.Service}.${e.Name}`
    if (k !== want) continue
    const stat = (snap.stats || []).find(s => s.key === k) || null
    // Attach the live rate-limit record (declared + effective +
    // overridden) so the drawer's Rate limit section can show both
    // baseline and any operator override without a second fetch.
    const rl = (snap.ratelimits || []).find(r => r.key === k) || null
    return { ...e, Stats: stat, RateLimitRecord: rl }
  }
  return null
})
const drawerResource = computed(() => {
  if (drawer.value?.kind !== 'resource') return null
  const snap = latestSnapshot.value
  if (!snap) return null
  return (snap.resources || []).find(r => r.name === drawer.value.key) || null
})
const drawerWorker = computed(() => {
  if (drawer.value?.kind !== 'worker') return null
  const snap = latestSnapshot.value
  if (!snap) return null
  return (snap.workers || []).find(w => w.Name === drawer.value.key) || null
})
const drawerCron = computed(() => {
  if (drawer.value?.kind !== 'cron') return null
  const snap = latestSnapshot.value
  if (!snap) return null
  return (snap.crons || []).find(c => c.name === drawer.value.key) || null
})

// Drawer header copy — title varies by kind. Subtitle gives one
// supporting line of context (service / kind / status).
const drawerTitle = computed(() => {
  if (!drawer.value) return ''
  if (drawer.value.kind === 'op') {
    const e = drawerOp.value
    if (!e) return ''
    if (e.Transport === 'rest')    return `${e.Method} ${e.Path}`
    if (e.Transport === 'graphql') return `${e.Method} ${e.Name}`
    return e.Path || e.Name || ''
  }
  if (drawer.value.kind === 'resource') return drawerResource.value?.name || ''
  if (drawer.value.kind === 'worker')   return drawerWorker.value?.Name || ''
  if (drawer.value.kind === 'cron')     return drawerCron.value?.name || ''
  if (drawer.value.kind === 'auth')     return 'Auth'
  return ''
})
const drawerSubtitle = computed(() => {
  if (!drawer.value) return ''
  if (drawer.value.kind === 'op') {
    // Prefer the MODULE name in the header subtitle — it's the
    // organizational unit the operator scans for on the canvas.
    // Falls back to the registered service name only when the
    // endpoint wasn't declared inside any nexus.Module.
    const e = drawerOp.value
    if (!e) return ''
    return e.Module || e.Service || ''
  }
  if (drawer.value.kind === 'resource') return drawerResource.value?.kind || ''
  if (drawer.value.kind === 'worker')   return 'worker · ' + (drawerWorker.value?.Status || 'unknown')
  if (drawer.value.kind === 'cron')     return 'cron · ' + (drawerCron.value?.schedule || '')
  if (drawer.value.kind === 'auth')     return 'cached identities · live rejections'
  return ''
})

// ─── Cmd-K palette ─────────────────────────────────────────────────
// Flat search index built from the latest snapshot so a keypress can
// fly to any node without scanning the canvas. Selecting a result
// routes through openDrawer — same destination as a click on the node.
const cmdkOpen = ref(false)
const cmdkItems = computed(() => {
  const snap = latestSnapshot.value
  if (!snap) return []
  const out = []
  // Global / non-node entries — Auth surfaces here so a keypress
  // opens the cached-identities + rejections drawer without needing
  // a click target on the canvas.
  out.push({
    id: 'auth',
    kind: 'auth',
    label: 'Auth',
    sublabel: 'cached identities · rejections',
    searchKey: 'auth identities sessions rejections',
    drawerSpec: { kind: 'auth' },
  })
  for (const e of snap.endpoints || []) {
    const label = e.Transport === 'rest'
      ? `${e.Method} ${e.Path}`
      : e.Transport === 'graphql'
        ? `${e.Method} ${e.Name}`
        : (e.Path || e.Name || '')
    const sub = `${e.Service || ''} · ${e.Transport || ''}`
    out.push({
      id: 'op:' + e.Service + '.' + e.Name,
      kind: 'op',
      label,
      sublabel: sub,
      searchKey: `${label} ${sub}`.toLowerCase(),
      drawerSpec: { kind: 'op', key: `${e.Service}.${e.Name}` },
    })
  }
  for (const r of snap.resources || []) {
    out.push({
      id: 'res:' + r.name,
      kind: 'resource-' + (r.kind || 'database'),
      label: r.name,
      sublabel: 'resource · ' + (r.kind || ''),
      searchKey: `${r.name} ${r.kind || ''} resource`.toLowerCase(),
      drawerSpec: { kind: 'resource', key: r.name },
    })
  }
  for (const w of snap.workers || []) {
    out.push({
      id: 'wk:' + w.Name,
      kind: 'worker',
      label: w.Name,
      sublabel: 'worker · ' + (w.Status || ''),
      searchKey: `${w.Name} worker ${w.Status || ''}`.toLowerCase(),
      drawerSpec: { kind: 'worker', key: w.Name },
    })
  }
  for (const c of snap.crons || []) {
    out.push({
      id: 'cron:' + c.name,
      kind: 'cron',
      label: c.name,
      sublabel: 'cron · ' + (c.schedule || ''),
      searchKey: `${c.name} cron ${c.schedule || ''}`.toLowerCase(),
      drawerSpec: { kind: 'cron', key: c.name },
    })
  }
  return out
})
function onCmdK(spec) {
  if (!spec) return
  openDrawer(spec)
}
function onGlobalKey(e) {
  if ((e.metaKey || e.ctrlKey) && (e.key === 'k' || e.key === 'K')) {
    e.preventDefault()
    cmdkOpen.value = !cmdkOpen.value
  }
}

// Error-dialog state. Dialog lazy-loads events via the per-op endpoint
// when opened — keeps /stats hot-path lean, and supports thousands of
// events via virtualized scrolling.
const errorDialog = ref({ open: false, service: '', op: '' })
function openErrors(payload) {
  errorDialog.value = {
    open: true,
    service: payload.service,
    op: payload.op,
  }
}
function closeErrors() { errorDialog.value = { ...errorDialog.value, open: false } }
provide('nexus.openErrors', openErrors)

const { fitView, onNodesInitialized, onPaneClick, onNodeDragStop } = useVueFlow()
onNodesInitialized(() => fitView({ padding: 0.2, maxZoom: 1 }))

// lastTopologyFingerprint is a sorted-id-list snapshot of the last
// rendered node set. load() compares the next render's fingerprint
// to this; an unchanged fingerprint means "same topology, just new
// counters" and we skip fitView so the user's pan/zoom survives the
// 5s poll. A changed fingerprint (new module appeared, etc.) calls
// fitView to bring the new node into view.
let lastTopologyFingerprint = ''

// userPositions tracks per-node drag overrides so polling doesn't
// snap a card back to dagre's slot after the user dropped it
// somewhere meaningful. Persisted in sessionStorage so a soft
// browser refresh keeps the layout — but not localStorage, so the
// arrangement resets on a fresh tab (avoids stale positions
// surviving real topology changes for too long).
const userPositions = (() => {
  try {
    const raw = sessionStorage.getItem('nexus.archPositions')
    return raw ? new Map(Object.entries(JSON.parse(raw))) : new Map()
  } catch {
    return new Map()
  }
})()
function persistPositions() {
  try {
    sessionStorage.setItem('nexus.archPositions', JSON.stringify(Object.fromEntries(userPositions)))
  } catch { /* quota / private mode — best effort */ }
}
onNodeDragStop(({ node }) => {
  if (!node || !node.id) return
  userPositions.set(node.id, { x: node.position.x, y: node.position.y })
  persistPositions()
})

// Click the empty canvas → clear op selection AND close the drawer.
// Backdrop clicks in the drawer go straight to closeDrawer; this is the
// other reset path for users who want to dismiss everything in one go.
onPaneClick(() => {
  clearOp()
  closeDrawer()
})

function estimateServiceHeight(data) {
  // data.endpoints is the VISIBLE slice (sorted + truncated unless
  // expanded). The chip row's visual height grows with chip count
  // (chips wrap to 2-3 lines when many resources / middlewares pile
  // on); approximate by bucketing into 0/1/2 extra rows so cards
  // don't overlap in dagre's layout without post-render measurement.
  const eps = data.endpoints || []
  const total = data.totalEndpoints ?? eps.length
  const desc = data.description ? 32 : 0
  let rows = 0
  for (const e of eps) {
    const hasOwnerChip = !e.ServiceAutoRouted
    const resCount = Array.isArray(e.Resources) ? e.Resources.length : 0
    const sdCount = Array.isArray(e.ServiceDeps) ? e.ServiceDeps.length : 0
    const mwCount = Array.isArray(e.Middleware)
      ? e.Middleware.filter(m => m !== 'metrics').length
      : 0
    const chipCount = (hasOwnerChip ? 1 : 0) + resCount + sdCount + mwCount
    if (chipCount === 0)       rows += 1
    else if (chipCount <= 3)   rows += 2
    else if (chipCount <= 6)   rows += 3
    else                       rows += 4
  }
  // Toggle button row — present whenever the card hides some endpoints
  // (collapsed and total > visible) OR shows them all (expanded with
  // a "Show fewer" affordance). About one row's worth of vertical real
  // estate including its top margin.
  const showToggle = total > eps.length || (data.isExpanded && total > 0)
  const toggleH = showToggle ? 36 : 0
  // Header now hosts a 32px CategoryIcon tile + a two-line title (name
  // + "service · N endpoints"); padding 12px top+bottom. ~56-60px tall.
  // Old 38 underestimated by ~20px and let dagre pack neighbouring
  // cards under the header → visible overlap.
  const HEADER = 60
  const FOOTER = 16 + 16
  return HEADER + desc + rows * 24 + toggleH + FOOTER
}

function estimateResourceHeight(data) {
  const detailKeys = Object.keys(data.details || {}).slice(0, 3).length
  const desc = data.description ? 22 : 0
  return 40 + desc + (detailKeys ? detailKeys * 18 + 16 : 0)
}

const NODE_WIDTH_SERVICE = 260
const NODE_WIDTH_RESOURCE = 200
const GAP = 48

function layout(ns, es) {
  if (es.length > 0) return dagreLayout(ns, es)
  return gridLayout(ns)
}

function dagreLayout(ns, es) {
  const g = new dagre.graphlib.Graph().setDefaultEdgeLabel(() => ({}))
  // nodesep controls vertical gap between cards in the same rank,
  // ranksep the horizontal gap between ranks. Generous values so
  // module cards with many endpoints (commonly 200-600px tall) keep
  // visible air between them and never visibly overlap when
  // estimateServiceHeight is slightly off.
  g.setGraph({ rankdir: 'LR', nodesep: 120, ranksep: 220 })
  ns.forEach(n => {
    let w, h
    if (n.type === 'internet') { w = 160; h = 90 }
    else if (n.type === 'resource') { w = NODE_WIDTH_RESOURCE; h = estimateResourceHeight(n.data) }
    else if (n.type === 'serviceDep') { w = NODE_WIDTH_RESOURCE; h = estimateServiceDepHeight(n.data) }
    else if (n.type === 'worker') { w = NODE_WIDTH_RESOURCE; h = estimateWorkerHeight(n.data) }
    else if (n.type === 'cron') { w = NODE_WIDTH_RESOURCE; h = estimateCronHeight(n.data) }
    else { w = NODE_WIDTH_SERVICE; h = estimateServiceHeight(n.data) }
    g.setNode(n.id, { width: w, height: h })
  })
  es.forEach(e => g.setEdge(e.source, e.target))
  dagre.layout(g)
  return ns.map(n => {
    const p = g.node(n.id)
    return {
      ...n,
      position: { x: p.x - p.width / 2, y: p.y - p.height / 2 },
      targetPosition: Position.Left,
      sourcePosition: Position.Right
    }
  })
}

function estimateServiceDepHeight(data) {
  const r = Array.isArray(data.resourceDeps) ? data.resourceDeps.length : 0
  const s = Array.isArray(data.serviceDeps) ? data.serviceDeps.length : 0
  const depsH = (r + s) > 0 ? 16 + (r + s) * 16 : 0
  return (data.description ? 60 : 40) + depsH
}

function estimateWorkerHeight(data) {
  const r = Array.isArray(data.resourceDeps) ? data.resourceDeps.length : 0
  const s = Array.isArray(data.serviceDeps) ? data.serviceDeps.length : 0
  const depsH = (r + s) > 0 ? 16 + (r + s) * 16 : 0
  const errH = data.lastError ? 16 : 0
  return 68 + depsH + errH
}

function estimateCronHeight(data) {
  // header (60) + schedule row (~22) + optional last-error (~22) +
  // padding. Matches the BaseNodeCard padding/footer reservations the
  // service-card estimator uses.
  const errH = data.lastRun?.error ? 22 : 0
  return 60 + 22 + errH + 16
}

// nodeBoxSize re-derives the (width, height) dagre reserved for a node
// — used by the deployment-frame computation post-layout. Same per-
// type logic as dagreLayout's setNode pass; kept in one helper so
// they can't drift apart.
function nodeBoxSize(n) {
  if (n.type === 'internet')   return { w: 160, h: 90 }
  if (n.type === 'resource')   return { w: NODE_WIDTH_RESOURCE, h: estimateResourceHeight(n.data) }
  if (n.type === 'serviceDep') return { w: NODE_WIDTH_RESOURCE, h: estimateServiceDepHeight(n.data) }
  if (n.type === 'worker')     return { w: NODE_WIDTH_RESOURCE, h: estimateWorkerHeight(n.data) }
  if (n.type === 'cron')       return { w: NODE_WIDTH_RESOURCE, h: estimateCronHeight(n.data) }
  return { w: NODE_WIDTH_SERVICE, h: estimateServiceHeight(n.data) }
}

function gridLayout(ns) {
  const cols = Math.min(ns.length, 3)
  const rowHeights = []
  ns.forEach((n, i) => {
    const row = Math.floor(i / cols)
    let h
    if (n.type === 'resource') h = estimateResourceHeight(n.data)
    else if (n.type === 'serviceDep') h = estimateServiceDepHeight(n.data)
    else if (n.type === 'worker') h = estimateWorkerHeight(n.data)
    else if (n.type === 'cron') h = estimateCronHeight(n.data)
    else h = estimateServiceHeight(n.data)
    rowHeights[row] = Math.max(rowHeights[row] || 0, h)
  })
  const rowY = [0]
  for (let r = 1; r < rowHeights.length; r++) {
    rowY.push(rowY[r - 1] + rowHeights[r - 1] + GAP)
  }
  return ns.map((n, i) => {
    const col = i % cols
    const row = Math.floor(i / cols)
    return {
      ...n,
      position: { x: col * (NODE_WIDTH_SERVICE + GAP), y: rowY[row] },
      targetPosition: Position.Left,
      sourcePosition: Position.Right
    }
  })
}

// latestSnapshot holds the most recent /__nexus/live frame. load() reads
// from it instead of fetching, so the WS push is the single source of
// state for the architecture view. null until the first snapshot arrives.
const latestSnapshot = ref(null)

function load() {
  const snap = latestSnapshot.value
  if (!snap) return
  // Shape the snapshot fields back into the {endpoints, services, …}
  // payloads the rest of this function was written against. Cheap,
  // and means we didn't have to rewrite the group/edge builder below.
  const epData = { services: snap.services || [], endpoints: snap.endpoints || [] }
  const rsData = { resources: snap.resources || [] }
  const statsData = { stats: snap.stats || [] }
  const wkData = { workers: snap.workers || [] }
  const crData = { crons: snap.crons || [] }
  // Index stats by "service.op". The stats key stays service-scoped
  // even after the UI regroups by module, because the metrics
  // middleware keys its counters by the owning service name.
  const statsByKey = {}
  for (const s of statsData.stats || []) statsByKey[s.key] = s
  const withStats = (e) => ({
    ...e,
    Stats: statsByKey[`${e.Service}.${e.Name}`] || null,
  })

  // ---------------------------------------------------------------
  // Hierarchy: MODULE → CONTROLLERS → SERVICES.
  //
  //   - Module is the container card (one nexus.Module wrapper = one
  //     card on the canvas).
  //   - Each endpoint row inside the card represents a controller
  //     (handler) declared in that module.
  //   - Services that controllers depend on (via *Service wrappers
  //     in the handler signature, or via the owning service's
  //     constructor params) render as small dep nodes on the right —
  //     not as cards. They're consumed, not containers.
  //
  // Endpoints registered outside any nexus.Module fall back to the
  // owning service name as the group key (no module to land in).
  // ---------------------------------------------------------------
  const groups = new Map() // groupKey -> { key, name, isModule, service, endpoints[], description }
  const serviceIndex = {}
  for (const s of epData.services || []) serviceIndex[s.Name] = s
  for (const e of epData.endpoints || []) {
    const moduleName = e.Module || ''
    const groupKey = moduleName ? `mod:${moduleName}` : `svc:${e.Service}`
    let g = groups.get(groupKey)
    if (!g) {
      g = {
        key: groupKey,
        name: moduleName || e.Service,
        isModule: !!moduleName,
        // service: the single owning service for this group, if every
        // endpoint in the group shares one. When endpoints from
        // multiple services land in one module (the oats core/
        // controllers shape), this stays '' so each row resolves
        // its own service via e.Service in ServiceNode.
        service: e.Service,
        // deployment: the DeployAs tag from the module declaration,
        // pulled off the first endpoint. Drives the deployment-frame
        // bbox computation post-layout — modules sharing a tag end up
        // wrapped in a single labelled VPC-style container.
        deployment: e.Deployment || '',
        endpoints: [],
        description: serviceIndex[moduleName || e.Service]?.Description || '',
      }
      groups.set(groupKey, g)
    }
    if (g.service && g.service !== e.Service) g.service = ''
    g.endpoints.push(withStats(e))
  }
  // Remote service placeholders — modules from peer deployments
  // register a service with Remote: true via the shadow generator's
  // nexus.RemoteService(...) option. Those have no endpoints in this
  // binary so the endpoint loop above doesn't create a group for
  // them; add one here so the architecture view shows the full
  // topology, with the remote module rendered as a card alongside
  // local ones.
  for (const s of epData.services || []) {
    if (!s.Remote) continue
    const groupKey = `mod:${s.Name}`
    if (groups.has(groupKey)) continue // local endpoint already created the group
    groups.set(groupKey, {
      key: groupKey,
      name: s.Name,
      isModule: true,
      service: s.Name,
      endpoints: [],
      description: s.Description || '',
      remote: true,
      deployment: s.Deployment || '',
    })
  }

  // Build the "displayed" view per group: sort by traffic (desc), tie-
  // break alphabetically, then truncate to MAX_VISIBLE_ENDPOINTS unless
  // the user has expanded this group via the +N more toggle. Edge
  // construction below iterates `displayed` so per-op edges only land
  // on rows that actually render — Vue Flow needs the targetHandle to
  // exist for routing to the right anchor.
  const labelOfEp = (e) => e.Name || `${e.Method} ${e.Path}` || ''
  const sortEndpoints = (eps) => [...eps].sort((a, b) => {
    const ca = a.Stats?.count || 0
    const cb = b.Stats?.count || 0
    if (cb !== ca) return cb - ca
    return labelOfEp(a).localeCompare(labelOfEp(b))
  })
  const sortedGroups = [...groups.values()].map(g => {
    const sorted = sortEndpoints(g.endpoints)
    const isExpanded = expandedGroups.value.has(g.key)
    const displayed = isExpanded ? sorted : sorted.slice(0, MAX_VISIBLE_ENDPOINTS)
    return { g, displayed, sorted, isExpanded, total: g.endpoints.length }
  })

  const groupNodes = sortedGroups.map(({ g, displayed, isExpanded, total }) => ({
    id: g.key,
    type: 'service',
    position: { x: 0, y: 0 },
    data: {
      groupKey: g.key,
      name: g.name,
      isModule: g.isModule,
      service: g.service,
      description: g.description,
      // Only the visible slice ships in `endpoints`. Total + isExpanded
      // let the card render the +N more / Show fewer toggle.
      endpoints: displayed,
      totalEndpoints: total,
      isExpanded,
      remote: !!g.remote,
      deployment: g.deployment || '',
    },
  }))

  // ---------------------------------------------------------------
  // Service-as-dep nodes: one per distinct service that some endpoint
  // takes as a handler dependency (the owning service when the handler
  // declared it, plus any services in ServiceDeps). These live on the
  // right of the canvas alongside resources.
  // ---------------------------------------------------------------
  // moduleCardsByName: every name that has a module-typed group on
  // the canvas. When a service name is ALSO the name of a module
  // card (the common case — local module "users" with service
  // "users"; remote module placeholder also shares the name), the
  // module card represents the dep — no separate small dep node is
  // needed, and edges should land on the card. Built before
  // depServices so the markDep guard below can consult it.
  const moduleCardsByName = {}
  for (const g of groups.values()) {
    if (g.isModule) moduleCardsByName[g.name] = g.key
  }
  // serviceToCard: maps a SERVICE NAME (whatever the registered service
  // wrapper called itself — e.g. "User service") to the module card
  // that owns its endpoints (e.g. "mod:users"). Lets the dashboard
  // route service-level edges (constructor deps, worker deps, cross-
  // service deps) onto the module CARD instead of spawning a separate
  // mini "dep" node when the service's name and module's name diverge.
  // Without this, a module named "users" with a service called "User
  // service" would render BOTH a "users" card and a sidekick "User
  // service" dep — the resource edge would land on the dep, looking
  // detached from the card the user actually clicks.
  const serviceToCard = {}
  for (const g of groups.values()) {
    for (const e of g.endpoints) {
      if (e.Service && !serviceToCard[e.Service]) {
        serviceToCard[e.Service] = g.key
      }
    }
  }
  // service-name → deployment-tag map; lets workers and crons (which
  // don't carry a deployment field of their own) infer membership of
  // a deployment frame via their service deps. Lifted into a ref so
  // drag-time recomputation reads the same map without re-scanning.
  // Service-name → deployment-tag map. Lets workers and crons (which
  // don't carry a Deployment field of their own) inherit the tag of
  // a service they depend on, so the deployment-tag pill shows up on
  // every card that conceptually belongs to a deployment unit.
  const svcToDep = {}
  for (const e of epData.endpoints || []) {
    if (e.Service && e.Deployment && !svcToDep[e.Service]) {
      svcToDep[e.Service] = e.Deployment
    }
  }
  // resolveServiceCard returns the canonical canvas node id for a
  // service name. Module-name match wins (rare but explicit); otherwise
  // we look at where the service's endpoints actually live.
  const resolveServiceCard = (name) => moduleCardsByName[name] || serviceToCard[name] || null

  const depServices = new Map() // name -> { Name, Description, ResourceDeps, ServiceDeps }
  const markDep = (name) => {
    if (!name) return
    // Skip names already represented by a module card OR by a card
    // that owns this service's endpoints (resolveServiceCard handles
    // the divergent-name case — e.g. service "User service" lives in
    // module card "users"). In both cases the card already carries the
    // service identity and a separate dep node would duplicate it.
    if (resolveServiceCard(name)) return
    if (!depServices.has(name)) {
      const s = serviceIndex[name] || { Name: name, Description: '' }
      depServices.set(name, {
        Name: s.Name,
        Description: s.Description || '',
        ResourceDeps: Array.isArray(s.ResourceDeps) ? s.ResourceDeps : [],
        ServiceDeps: Array.isArray(s.ServiceDeps) ? s.ServiceDeps : [],
      })
    }
  }

  // resolveDepTarget returns the canvas node ID an edge should target
  // when pointing at "service X". Prefer a module card (matched by
  // module name OR by the service name's owning card via
  // resolveServiceCard); fall back to a plain dep node only when the
  // service has no endpoints on the canvas.
  const resolveDepTarget = (name) => resolveServiceCard(name) || `dep:${name}`
  for (const e of epData.endpoints || []) {
    // Only endpoints whose handler explicitly took *Service as a Go
    // dep add the service as a per-row architecture dep. Auto-routed
    // endpoints (adopted into a service without declaring it) skip
    // this — they're conceptually owned by the service via metrics
    // accounting, but they don't depend on the service wrapper value.
    if (!e.ServiceAutoRouted) markDep(e.Service)
    for (const s of e.ServiceDeps || []) markDep(s)
  }
  // Service-level dep edges (populated below) also create dep nodes
  // for any resource / service the SERVICE CONSTRUCTOR names, even
  // if no individual endpoint row uses them. This reflects the
  // "service depends on X" relationship at the service layer.
  for (const s of epData.services || []) {
    if (Array.isArray(s.ServiceDeps) && s.ServiceDeps.length > 0) {
      markDep(s.Name) // make sure the originating service appears
      for (const d of s.ServiceDeps) markDep(d)
    }
  }
  // Workers may reference services that no endpoint uses — mark them
  // here so the dep node exists by the time workerNodes renders.
  for (const w of wkData.workers || []) {
    for (const s of w.ServiceDeps || []) markDep(s)
  }
  const svcDepNodes = [...depServices.values()].map(s => ({
    id: `dep:${s.Name}`,
    type: 'serviceDep',
    position: { x: 0, y: 0 },
    // Pass constructor-level deps through so the node itself can list
    // them inline (belt-and-braces for cases where the edges are hard
    // to spot visually). The edges still encode the same info for the
    // graph layout; this just surfaces it on the card.
    data: {
      name: s.Name,
      description: s.Description || '',
      resourceDeps: s.ResourceDeps || [],
      serviceDeps: s.ServiceDeps || [],
    },
  }))

  // Single "Clients" node representing external traffic sources. Lives
  // on the far-left of the dagre layout because it has no incoming edges.
  const internetNode = {
    id: INTERNET_ID,
    type: 'internet',
    position: { x: 0, y: 0 },
    data: {},
  }
  const rsNodes = (rsData.resources || []).map(r => ({
    id: `res:${r.name}`,
    type: 'resource',
    position: { x: 0, y: 0 },
    data: r
  }))

  // Workers — long-lived background tasks registered via nexus.AsWorker.
  // They're peers of services: they have dep nodes (resources + other
  // services) but no HTTP traffic. Each worker becomes one card on
  // the graph; edges to/from their deps share the same service-level
  // styling so the "background worker uses X" relationship is
  // visually consistent with "service uses X".
  const workerNodes = (wkData.workers || []).map(w => {
    // Infer deployment from any service dep that carries a tag.
    // Lets workers ride along on their owning service's deployment
    // pill without the framework needing to track this directly.
    let dep = ''
    for (const s of w.ServiceDeps || []) {
      if (svcToDep[s]) { dep = svcToDep[s]; break }
    }
    return {
      id: `wk:${w.Name}`,
      type: 'worker',
      position: { x: 0, y: 0 },
      data: {
        name: w.Name,
        status: w.Status || 'unknown',
        lastError: w.LastError || '',
        resourceDeps: w.ResourceDeps || [],
        serviceDeps: w.ServiceDeps || [],
        deployment: dep,
      },
    }
  })

  // Crons — one node per app.Cron(...). Linked to a service when the
  // job's .Service was set; that draws a cron→service edge so an
  // operator sees which service "owns" each scheduled task.
  const cronNodes = (crData.crons || []).map(c => ({
    id: `cron:${c.name}`,
    type: 'cron',
    position: { x: 0, y: 0 },
    // c.service points at the service name the cron belongs to; we
    // resolve that to a deployment tag so the cron card carries the
    // same deployment pill its service does.
    data: { ...c, deployment: c.service ? (svcToDep[c.service] || '') : '' },
  }))

  // ---------------------------------------------------------------
  // Edges. In the module-first model:
  //   1. Per-op row → resource   (unchanged — endpoint uses resource)
  //   2. Per-op row → serviceDep (NEW — endpoint uses another service)
  //   3. Per-op row → owningDep  (NEW — endpoint declared its own
  //      service wrapper as a dep; auto-routed endpoints omitted)
  //   4. Aggregated fallback for runtime-only resource attachments.
  //   5. Internet → module group (inbound traffic lane).
  // ---------------------------------------------------------------
  const edgeList = []
  const claimed = new Set()
  // Per-op edge construction. One line per (source, target, op) so the
  // line ANCHORS to the row's per-op handle on both ends — outbound
  // emerges from the row's right, inbound (built separately below)
  // lands on the row's left. Trades visual density for accuracy: a
  // module with 25 endpoints all hitting main-db now draws 25 lines
  // instead of one, but each line ties to its own row.
  //
  // Dedupe is by (source, target, op) so the same op claiming the same
  // dep twice (rare; service-deps + handler-deps overlap) doesn't push
  // duplicate edges. Service-level edges (no op) keep (source, target)
  // dedup since they don't have a row to anchor to.
  const edgeByKey = new Map()
  function pushOpEdge(src, tgt, base) {
    // Drop self-loops. They happen when an endpoint declares its owning
    // *Service (or a ServiceDeps name) that resolves to THE SAME module
    // card the endpoint already lives in — e.g. an endpoint in module
    // "users" that takes *UsersService as a dep. Useless arc.
    if (src === tgt) return
    const op = base.op || ''
    const k = op ? `${src}->${tgt}@${op}` : `${src}->${tgt}`
    if (edgeByKey.has(k)) return
    const edge = {
      id: `e:${k}`,
      source: src,
      // Per-row source handle — only set when the edge belongs to a
      // specific op. Service-level / fallback edges leave it unset and
      // emerge from the card's default right side instead.
      sourceHandle: op ? `op:${op}` : undefined,
      target: tgt,
      markerEnd: MarkerType.ArrowClosed,
      data: { ...base, ops: op ? [op] : [] },
    }
    edgeByKey.set(k, edge)
    edgeList.push(edge)
  }
  // Per-op outbound edges. Iterates `displayed` (visible rows only) so
  // every edge has a sourceHandle that actually renders. Hidden rows
  // get no outbound edges — when the user expands the card via +N more,
  // load() reruns and the edges materialise.
  for (const { g, displayed } of sortedGroups) {
    for (const e of displayed) {
      const opName = e.Name || `${e.Method} ${e.Path}`
      // Resource edges.
      for (const rName of e.Resources || []) {
        pushOpEdge(g.key, `res:${rName}`, {
          service: e.Service, target: rName, targetKind: 'resource', op: opName,
        })
        claimed.add(`${e.Service}|res:${rName}`)
      }
      // Owning-service dep edge — ONLY when the handler explicitly took
      // *Service as a Go dep. Auto-routed endpoints skip this because
      // they don't actually depend on the service wrapper value; they
      // were adopted into the service for schema/metrics routing only.
      if (!e.ServiceAutoRouted && (depServices.has(e.Service) || resolveServiceCard(e.Service))) {
        const tgt = resolveDepTarget(e.Service)
        // Skip self-edges: when the resolved target is the same module
        // card we're emitting from (the common case after
        // resolveServiceCard funnels "User service" → "mod:users"),
        // pushOpEdge already drops src===tgt — but we can short-circuit
        // here so we don't even count the edge as claimed.
        if (tgt !== g.key) {
          pushOpEdge(g.key, tgt, {
            service: e.Service, target: e.Service, targetKind: 'service', op: opName, owning: true,
          })
        }
      }
      // Other-service dep edges. resolveDepTarget routes to the module
      // card when one exists (cross-module dep — the dep IS another
      // module on the canvas) and falls back to the small dep node
      // otherwise (purely service-level dep).
      for (const sName of e.ServiceDeps || []) {
        const tgt = resolveDepTarget(sName)
        pushOpEdge(g.key, tgt, {
          service: e.Service, target: sName, targetKind: 'service', op: opName,
        })
      }
    }
  }
  // Aggregated fallback for runtime-attached resources no op claims.
  for (const r of rsData.resources || []) {
    for (const svc of r.attachedTo || []) {
      if (claimed.has(`${svc}|res:${r.name}`)) continue
      const groupKey = serviceToCard[svc]
      if (!groupKey) continue
      edgeList.push({
        id: `e:${groupKey}->res:${r.name}`,
        source: groupKey,
        sourceHandle: 'svc',
        target: `res:${r.name}`,
        markerEnd: MarkerType.ArrowClosed,
        data: { service: svc, target: r.name, targetKind: 'resource', op: null },
      })
    }
  }

  // Worker-level dep edges — one line per (worker, resource) and
  // (worker, service) tuple, styled as service-level so the same
  // packet-animation rules apply. Workers get pulses on their
  // resource edges whenever the worker reports activity (phase 4+).
  for (const w of wkData.workers || []) {
    const wSrc = `wk:${w.Name}`
    for (const res of w.ResourceDeps || []) {
      edgeList.push({
        id: `e:${wSrc}->res:${res}`,
        source: wSrc,
        target: `res:${res}`,
        markerEnd: MarkerType.ArrowClosed,
        data: { service: w.Name, target: res, targetKind: 'resource', op: null, serviceLevel: true, worker: true },
      })
    }
    for (const other of w.ServiceDeps || []) {
      if (!depServices.has(other) && !resolveServiceCard(other)) continue
      const tgt = resolveDepTarget(other)
      // Skip self-loops — a worker that shares a name with a module
      // card it depends on would otherwise produce wk:X → wk:X.
      if (tgt === wSrc) continue
      edgeList.push({
        id: `e:${wSrc}->${tgt}`,
        source: wSrc,
        target: tgt,
        markerEnd: MarkerType.ArrowClosed,
        data: { service: w.Name, target: other, targetKind: 'service', op: null, serviceLevel: true, worker: true },
      })
    }
  }

  // Cron edges — when a job declared .Service("foo"), draw an edge
  // from the cron node onto that service's card. Same semantics as
  // worker→service: the cron "belongs to" the service for purposes of
  // ownership / on-call grouping. resolveServiceCard gracefully
  // handles divergent service/module naming.
  for (const c of crData.crons || []) {
    if (!c.service) continue
    const tgt = resolveDepTarget(c.service)
    const src = `cron:${c.name}`
    if (!tgt || tgt === src) continue
    edgeList.push({
      id: `e:${src}->${tgt}`,
      source: src,
      target: tgt,
      markerEnd: MarkerType.ArrowClosed,
      data: { service: c.service, target: c.service, targetKind: 'service', op: null, serviceLevel: true, cron: true },
    })
  }

  // Service-level dep edges: edges originating at a service-dep node
  // (not at an endpoint row) that point to resources / other services
  // the SERVICE CONSTRUCTOR depends on. Backend populates these via
  // nexus.ProvideService(NewXService) — e.g. NewAdvertsService(app,
  // users *UsersService, db *DBManager) records (users, db) as deps
  // of AdvertsService, which the UI then draws as dep-node→dep-node
  // (or module-card→module-card when both names resolve to module
  // cards) lines so the service layer's architecture is visible even
  // when no individual endpoint touches those dependencies directly.
  for (const s of epData.services || []) {
    // Source: prefer the service's owning module card so service-level
    // edges land on the visible card the user clicks (not a sidekick
    // dep node when the service name doesn't match the module name).
    const sourceID = resolveServiceCard(s.Name) || (depServices.has(s.Name) ? `dep:${s.Name}` : '')
    if (!sourceID) continue
    for (const res of s.ResourceDeps || []) {
      edgeList.push({
        id: `e:${sourceID}->res:${res}`,
        source: sourceID,
        target: `res:${res}`,
        markerEnd: MarkerType.ArrowClosed,
        data: { service: s.Name, target: res, targetKind: 'resource', op: null, serviceLevel: true },
      })
    }
    for (const other of s.ServiceDeps || []) {
      if (!depServices.has(other) && !resolveServiceCard(other)) continue
      const tgt = resolveDepTarget(other)
      // Skip self-loops — a service whose constructor lists itself in
      // ServiceDeps (or aliases that resolve back to its own module
      // card) would otherwise draw a useless mod:X → mod:X arc.
      if (tgt === sourceID) continue
      edgeList.push({
        id: `e:${sourceID}->${tgt}`,
        source: sourceID,
        target: tgt,
        markerEnd: MarkerType.ArrowClosed,
        data: { service: s.Name, target: other, targetKind: 'service', op: null, serviceLevel: true },
      })
    }
  }
  // Internet → endpoint-row edges. One per VISIBLE endpoint, with
  // targetHandle pointing at the row's per-op target handle so the
  // line LANDS on the row the request actually hits. Empty / all-
  // hidden modules (typically remote-deployment placeholders OR
  // collapsed groups whose top-N happens to be empty — rare) fall back
  // to one card-level inbound so the topology still shows the module
  // gets traffic conceptually.
  for (const { g, displayed } of sortedGroups) {
    if (displayed.length === 0) {
      edgeList.push({
        id: `e:internet->${g.key}`,
        source: INTERNET_ID,
        target: g.key,
        markerEnd: MarkerType.ArrowClosed,
        data: { service: g.service, target: g.name, targetKind: 'module', op: null, inbound: true, groupKey: g.key },
      })
      continue
    }
    for (const e of displayed) {
      const opName = e.Name || `${e.Method} ${e.Path}`
      edgeList.push({
        id: `e:internet->${g.key}@${opName}`,
        source: INTERNET_ID,
        target: g.key,
        targetHandle: 'op:' + opName,
        markerEnd: MarkerType.ArrowClosed,
        data: {
          service: e.Service,
          target: g.name,
          targetKind: 'module',
          op: opName,
          // ops singleton so indexEndpointEdges + selectedMatches treat
          // it like a per-op edge for flash/highlight purposes.
          ops: [opName],
          inbound: true,
          groupKey: g.key,
        },
      })
    }
  }

  const all = [internetNode, ...groupNodes, ...svcDepNodes, ...workerNodes, ...cronNodes, ...rsNodes]
  // Diagnostic: surface the built graph to window so operators can
  // verify service-level edges from DevTools without re-reading this
  // file. Cheap (single assignment per poll) and invaluable when an
  // expected dep edge doesn't render.
  if (typeof window !== 'undefined') {
    window.__nexusArch = {
      groupNodes: groupNodes.map(n => n.id),
      svcDepNodes: svcDepNodes.map(n => n.id),
      resourceNodes: rsNodes.map(n => n.id),
      edges: edgeList.map(e => ({ id: e.id, source: e.source, target: e.target, op: e.data?.op ?? null, serviceLevel: !!e.data?.serviceLevel })),
    }
  }
  const laid = layout(all, edgeList)
  try {
    // Apply user-drag overrides so dragged cards don't snap back to
    // dagre's slot on the next 5s poll. dagre still computes the
    // baseline layout for first paint and for any node the user
    // hasn't touched; the override map only kicks in for ids the
    // user explicitly moved.
    const nextNodes = laid.map(n => {
      const moved = userPositions.get(n.id)
      return moved ? { ...n, position: moved } : n
    })
    // Topology fingerprint: a sorted list of node ids. fitView only
    // fires when the set CHANGES — initial paint and when a new
    // module/service/resource appears mid-session. Steady-state
    // polling (every 5s) keeps the user's pan + zoom intact.
    const nextFingerprint = nextNodes.map(n => n.id).sort().join('|')
    const topologyChanged = nextFingerprint !== lastTopologyFingerprint
    lastTopologyFingerprint = nextFingerprint
    nodes.value = nextNodes
    rawEdges.value = edgeList
    indexEndpointEdges(edgeList)
    indexEndpointGroups(groupNodes)
    edges.value = restyleEdges(edgeList, opSelection.value, flashedEdges.value)
    if (topologyChanged) {
      nextTick(() => fitView({ padding: 0.2, maxZoom: 1 }))
    }
  } catch (err) {
    console.error('[nexus] Architecture render failed:', err, { groupCount: groupNodes.length, edgeCount: edgeList.length })
  }
}

// selectedMatches reports whether edge `e` carries the currently-
// selected op on the currently-selected module group. Per-op edges
// store the op in data.ops; a match means "the edge runs from this
// group AND the selected op is one of the contributors."
//
// Inbound per-op edges (Internet → row) are matched by (target,
// op) instead of (source, op) — their source is INTERNET_ID, not
// the group key. Inbound aggregated edges (empty module fallback)
// match by target alone since they have no op.
function selectedMatches(sel, e) {
  if (!sel) return false
  if (e.data.inbound) {
    if (e.target !== sel.groupKey) return false
    if (!e.data.op) return true              // empty-module card-level fallback
    return e.data.op === sel.op
  }
  if (e.source !== sel.groupKey) return false
  const ops = e.data.ops
  if (Array.isArray(ops) && ops.length > 0) return ops.includes(sel.op)
  return false
}

// EDGE_COLOR is the hex palette restyleEdges paints with. Hardcoded (rather
// than var(--*) lookups) because SVG marker fill/stroke attributes don't
// resolve CSS vars — only the path's inline style does. Comments map back
// to the source token in tokens.css; bump both together.
const EDGE_COLOR = {
  accent:    '#4f46e5',  // --accent
  border:    '#e5e7eb',  // --border
  borderStr: '#d1d5db',  // --border-strong
  internet:  '#64748b',  // --cat-internet
  worker:    '#f97316',  // --cat-worker
  error:     '#ef4444',  // --st-error
}

// buildMarker returns a Vue Flow markerEnd object whose arrowhead matches
// the path stroke. Smaller than the default ArrowClosed (14px instead of
// the ~20px default) so arrows read as direction cues, not as icons.
function buildMarker(color) {
  return { type: MarkerType.ArrowClosed, width: 14, height: 14, color }
}

// restyleEdges returns a fresh edges array with type/path/style applied
// based on the current op selection + live-traffic flash state. Every
// edge becomes a smoothstep (orthogonal with rounded corners) so the
// canvas reads as infrastructure, not a tangle of bezier curves. Five
// semantic kinds get distinct idle styling: inbound (entry lane from
// Internet), service-level (constructor wires — dashed), aggregated
// (multi-op shared edge), per-op (specific endpoint use), and selected.
function restyleEdges(list, sel, flashed) {
  return list.map(e => {
    const base = { ...e }
    // Orthogonal routing with rounded corners — AWS-console-style. The
    // offset gives the path a generous "elbow" before turning, which
    // keeps lines from cramming into a node's edge.
    base.type = 'smoothstep'
    base.pathOptions = { borderRadius: 12, offset: 20 }

    const isAggregated = e.data.op === null
    const isInbound    = !!e.data.inbound
    const isWorker     = !!e.data.worker
    const isServiceLvl = !!e.data.serviceLevel
    const flashState   = flashed && flashed.get(e.id)

    let stroke, width, opacity, animated = false, dashed = false

    if (flashState) {
      // Live traffic — brightest, always animated. Color reflects request
      // outcome so error flows visually distinct from successes.
      stroke = flashState === 'error' ? EDGE_COLOR.error : EDGE_COLOR.accent
      width = 2.2
      opacity = 1
      animated = true
    } else if (sel) {
      // Selection mode: highlight the matching edges, fade everything else
      // down to a barely-there scaffolding so the focused path pops.
      if (selectedMatches(sel, e)) {
        stroke = EDGE_COLOR.accent
        width = 2
        opacity = 1
        animated = true
      } else {
        stroke = EDGE_COLOR.border
        width = 1
        opacity = 0.12
      }
    } else {
      // Idle baselines — semantic by edge kind. Worker edges paint in
      // the worker category color (orange) so an active background task
      // stays visible against the constructor-wires/inbound styling.
      // Constructor wires get a dashed stroke so they read as "wired
      // but not actively used per request"; inbound lanes use the
      // slate internet color so the entry boundary is visually distinct
      // from intra-service traffic.
      if (isWorker) {
        stroke = EDGE_COLOR.worker
        width = 1.5
        opacity = 0.85
      } else if (isInbound) {
        stroke = EDGE_COLOR.internet
        width = 1.3
        opacity = 0.5
      } else if (isServiceLvl) {
        stroke = EDGE_COLOR.borderStr
        width = 1.2
        opacity = 0.7
        dashed = true
      } else if (isAggregated) {
        stroke = EDGE_COLOR.borderStr
        width = 1.2
        opacity = 0.8
      } else {
        stroke = EDGE_COLOR.accent
        width = 1.2
        opacity = 0.4
      }
    }

    base.style = { stroke, strokeWidth: width, opacity }
    if (dashed) base.style.strokeDasharray = '5 4'
    base.animated = animated
    base.markerEnd = buildMarker(stroke)
    return base
  })
}

// rawEdges holds the un-styled edge list so restyle calls don't stack
// styling on top of styling. flashedEdges is the map of edge id → state
// ('ok' | 'error') that should render in the bright "live-traffic"
// style right now; entries clear themselves via setTimeout.
const rawEdges = ref([])
const flashedEdges = ref(new Map())
watch([opSelection, flashedEdges], () => {
  edges.value = restyleEdges(rawEdges.value, opSelection.value, flashedEdges.value)
}, { deep: true })

// flashEdges triggers the live-traffic pulse: adds ids to the flashed
// map with state, schedules their removal after FLASH_MS. Subsequent
// flashes of the same id overwrite state + reset the timer — most
// recent event wins.
const FLASH_MS = 900
const flashTimers = new Map()
function flashEdges(ids, state) {
  if (!ids.length) return
  const s = state === 'error' ? 'error' : 'ok'
  const next = new Map(flashedEdges.value)
  for (const id of ids) {
    next.set(id, s)
    const prev = flashTimers.get(id)
    if (prev) clearTimeout(prev)
    flashTimers.set(id, setTimeout(() => {
      const m = new Map(flashedEdges.value)
      m.delete(id)
      flashedEdges.value = m
      flashTimers.delete(id)
    }, FLASH_MS))
  }
  flashedEdges.value = next
}

// onTraceEvent maps an incoming request.start event to the edges that
// should light up: inbound lane Internet→module-group, plus any per-op
// edges the handler declared (resources / other services). We stash the
// endpoint → edge map at load time so lookups are constant-time here.
const endpointEdgeIdx = new Map() // "svc.opName" → [edge id, ...]
const serviceEdgeIdx = new Map()  // svc name → [service-level edge id, ...]
function indexEndpointEdges(edgeList) {
  endpointEdgeIdx.clear()
  serviceEdgeIdx.clear()
  for (const e of edgeList) {
    if (e.data.serviceLevel && e.data.service) {
      // Service-level edges (dep:svc → res / dep:svc → dep) are
      // keyed by the originating service so a request.op against
      // any endpoint in that service can flash these too.
      const arr = serviceEdgeIdx.get(e.data.service) || []
      arr.push(e.id)
      serviceEdgeIdx.set(e.data.service, arr)
      continue
    }
    if (e.data.inbound) continue
    // Aggregated edges carry an ops list — index the edge id under
    // every op it represents so a flash on any of those ops lights
    // up the single shared edge.
    const ops = Array.isArray(e.data.ops) ? e.data.ops : []
    for (const op of ops) {
      if (!op) continue
      const k = `${e.data.service}.${op}`
      const arr = endpointEdgeIdx.get(k) || []
      arr.push(e.id)
      endpointEdgeIdx.set(k, arr)
    }
  }
}
// endpointGroupIdx maps "<service>.<op>" → module-group node id so the
// trace-event handler can locate the right inbound lane (Internet →
// group) after the module-first regrouping.
const endpointGroupIdx = new Map()
function indexEndpointGroups(groupNodes) {
  endpointGroupIdx.clear()
  for (const n of groupNodes) {
    for (const e of n.data.endpoints || []) {
      const op = e.Name || `${e.Method} ${e.Path}`
      endpointGroupIdx.set(`${e.Service}.${op}`, n.id)
    }
  }
}

function onTraceEvent(ev, force = false) {
  // request.op carries the specific op name in Endpoint (emitted by the
  // metrics middleware per handler exit). request.start from the
  // framework trace layer only carries the HTTP path — too coarse to
  // identify a GraphQL operation — so we drive the per-op UI off
  // request.op exclusively. Result: packets land on the right row.
  if (ev.kind !== 'request.op') return
  if (!ev.service) return
  // Skip events we're replaying from the /events backlog on initial
  // subscribe — they're older than this mount so animating them would
  // misrepresent "live" state. The scrubber's replay path passes
  // force=true to bypass this filter, since replaying past events at
  // a pinned snapshot is exactly what the user asked for.
  if (!force && ev.timestamp) {
    const evTime = new Date(ev.timestamp).getTime()
    if (evTime && evTime < mountedAtMs) return
  }
  const failed = typeof ev.status === 'number' ? ev.status >= 400 : !!ev.error
  // Locate the module-group that owns this endpoint so the inbound
  // lane lands on the correct card. Falls back to the old svc: id
  // shape when the endpoint hasn't been grouped yet (rare race).
  const groupId = ev.endpoint
    ? endpointGroupIdx.get(`${ev.service}.${ev.endpoint}`)
    : null
  // Prefer the per-op inbound edge so the flash + packet land on the
  // specific row. Fall back to the aggregated card-level inbound for
  // empty-module placeholders, then to the legacy svc: shape if the
  // group hasn't materialised yet.
  const perOpInbound = groupId && ev.endpoint ? `e:internet->${groupId}@${ev.endpoint}` : null
  const aggInbound   = groupId ? `e:internet->${groupId}` : `e:internet->svc:${ev.service}`
  const inboundId = (perOpInbound && rawEdges.value.find(e => e.id === perOpInbound))
    ? perOpInbound
    : aggInbound

  // On error we ONLY light up the inbound lane — downstream resource/
  // service-dep edges never ran, so animating them would falsely
  // suggest the mutation reached the DB. The packet's red "stop" mark
  // at the op row makes the rejection visible.
  const outboundIds = []
  if (ev.endpoint) {
    const opKey = `${ev.service}.${ev.endpoint}`
    for (const id of endpointEdgeIdx.get(opKey) || []) outboundIds.push(id)
  }
  // Service-level edges (dep:svc → its constructor deps) also pulse
  // on any op activity for that service — so operators can see the
  // service "using" its declared deps, not just the ops that
  // explicitly touch them.
  const serviceIds = []
  for (const id of serviceEdgeIdx.get(ev.service) || []) serviceIds.push(id)
  const flashIds = failed ? [inboundId] : [inboundId, ...outboundIds, ...serviceIds]
  flashEdges(flashIds, failed ? 'error' : 'ok')
  spawnPacketsForEdges(flashIds, ev.endpoint, failed ? 'error' : 'ok')
}

const mountedAtMs = Date.now()

// spawnPacketsForEdges asks the overlay to fly a packet along each
// edge's actual SVG path — not a straight line. The overlay's spawn()
// reads getPointAtLength every frame, so packets ride the smoothstep
// elbows, anchor to per-op row handles automatically (Vue Flow already
// routes the path from the right handle), and track pan/zoom during
// transit. opName is no longer needed for row aiming — the path
// already starts/ends at the right point on the card.
function spawnPacketsForEdges(ids, _opName, state) {
  if (!packetOverlay.value) return
  const canvas = canvasEl.value
  if (!canvas) return
  const opts = { state: state === 'error' ? 'error' : 'ok' }
  ids.forEach((edgeId, i) => {
    const pathEl = canvas.querySelector(
      `.vue-flow__edge[data-id="${CSS.escape(edgeId)}"] .vue-flow__edge-path`
    )
    if (!pathEl) return
    const stagger = i * 120 // entry dot first, then downstream hops
    setTimeout(() => packetOverlay.value?.spawn(pathEl, canvas, opts), stagger)
  })
}

const packetOverlay = ref(null)
const canvasEl = ref(null)

// Re-layout when the user toggles a card's +N more / Show fewer. We
// have to rebuild edges (per-op edges depend on which rows are visible)
// and rerun dagre so neighbouring cards reflow around the now-taller
// (or shorter) card.
watch(expandedGroups, () => {
  if (latestSnapshot.value) load()
}, { deep: true })

let traceSub = null
let liveSub = null
onMounted(() => {
  // /__nexus/live pushes a fresh state snapshot every ~2s. First frame
  // is the initial render; later frames keep the graph live without the
  // 5s polling tax. The WS auto-reconnects on close.
  liveSub = subscribeLive(snap => {
    // Always record into the scrub-history ring. When the user is
    // scrubbing, we still capture frames in the background so they
    // can advance time forward without losing what just happened.
    snapshotHistory.value.push({ ts: Date.now(), snap })
    if (snapshotHistory.value.length > SCRUB_HISTORY_CAP) {
      snapshotHistory.value.shift()
    }
    // Render the new frame only when streaming live; while paused
    // the canvas reflects the user's pinned scrubIndex instead.
    if (scrubIndex.value === null) {
      latestSnapshot.value = snap
      load()
    }
  })
  // Per-request trace stream — drives the live edge pulse and packet
  // animation. Separate socket from /live; the two streams are
  // independent and each owns its reconnect logic.
  //
  // Every event also goes into eventHistory so the scrubber can
  // replay flashes + packets at a pinned past moment. Live processing
  // is suppressed while scrubbing — animations should reflect the
  // pinned frame, not whatever just streamed in the background.
  traceSub = subscribeEvents(ev => {
    eventHistory.value.push(ev)
    if (eventHistory.value.length > EVENT_HISTORY_CAP) eventHistory.value.shift()
    if (scrubIndex.value === null) onTraceEvent(ev)
  }, null, 0)
  // Cmd-K toggle. CmdK owns its own internal navigation keys; we just
  // own the open/close shortcut here.
  window.addEventListener('keydown', onGlobalKey)
})
onUnmounted(() => {
  if (traceSub) traceSub.close()
  if (liveSub) liveSub.close()
  flashTimers.forEach(t => clearTimeout(t))
  window.removeEventListener('keydown', onGlobalKey)
})
</script>

<template>
  <div class="arch" ref="canvasEl">
    <VueFlow
      :nodes="nodes"
      :edges="edges"
      :node-types="nodeTypes"
      :min-zoom="0.25"
      :max-zoom="1.5"
    >
      <!-- Dot grid (Vue Flow's Background defaults to dots). Color +
           gap match the --canvas-dot token in tokens.css; keep literals
           here because Background takes string props, not CSS vars. -->
      <Background pattern-color="#cbd5e1" :gap="18" :size="1.4" />
      <Controls :show-interactive="false" />
    </VueFlow>
    <GlobalMiddlewareBar />
    <!-- Canvas-level utility strip (top-right). Currently just the
         Auth opener, but reserved as the home for future global
         actions (theme toggle, settings, …) so the canvas itself
         hosts everything that used to live in the top tab bar. -->
    <div class="canvas-utility">
      <TimeScrubber />
      <button
        class="utility-btn"
        title="Open Auth drawer (cached identities + rejections)"
        @click="openDrawer({ kind: 'auth' })"
      >
        <ShieldCheck :size="14" :stroke-width="2" />
        Auth
      </button>
    </div>
    <PacketOverlay ref="packetOverlay" />
    <!-- Overlay toggles — diagnostic paint mode. Floating chip group
         that dims non-matching op rows so error / limit / auth
         coverage is visible at a glance across the whole canvas. -->
    <OverlayToggles />
    <!-- Activity rail — bottom strip with the live trace feed. Folds
         what used to be the Traces tab onto the canvas. Subscribes
         independently from the canvas's own event listener (which
         only watches request.op for edge pulses) so the rail always
         reflects the full /__nexus/events stream, including spans +
         auth rejects + errors. -->
    <ActivityRail />
    <div v-if="!nodes.length" class="empty">
      No services registered yet.
    </div>
    <ErrorDialog
      :open="errorDialog.open"
      :service="errorDialog.service"
      :op="errorDialog.op"
      @close="closeErrors"
    />
    <Drawer
      :open="!!drawer"
      :title="drawerTitle"
      :subtitle="drawerSubtitle"
      @close="closeDrawer"
    >
      <OpDetail       v-if="drawer?.kind === 'op'       && drawerOp"       :op="drawerOp" />
      <ResourceDetail v-if="drawer?.kind === 'resource' && drawerResource" :resource="drawerResource" />
      <WorkerDetail   v-if="drawer?.kind === 'worker'   && drawerWorker"   :worker="drawerWorker" />
      <CronDetail     v-if="drawer?.kind === 'cron'     && drawerCron"     :cron="drawerCron" />
      <AuthDetail     v-if="drawer?.kind === 'auth'" />
    </Drawer>
    <CmdK
      :open="cmdkOpen"
      :items="cmdkItems"
      @close="cmdkOpen = false"
      @select="onCmdK"
    />
  </div>
</template>

<style scoped>
.arch { width: 100%; height: 100%; position: relative; background: var(--canvas-bg); }

/* Canvas-level utility strip — sits on top of the VueFlow surface in
   the top-right, above any node. Reserved for global controls (Auth
   drawer opener for now; theme + settings can land here next). z-
   index above the canvas grid but below the drawer's backdrop. */
.canvas-utility {
  position: absolute;
  top: 12px;
  right: 12px;
  z-index: 12;
  display: flex;
  gap: var(--space-2);
}
.utility-btn {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 6px 12px;
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  color: var(--text-muted);
  font-size: var(--fs-sm);
  font-weight: 500;
  cursor: pointer;
  box-shadow: var(--shadow-sm);
  transition: background 120ms, color 120ms, border-color 120ms;
}
.utility-btn:hover {
  background: var(--bg-hover);
  color: var(--text);
  border-color: var(--border-strong);
}

.empty {
  position: absolute;
  inset: 0;
  display: grid;
  place-items: center;
  color: var(--text-dim);
  pointer-events: none;
  font-size: 13px;
}

/* Vue Flow edge polish — smooth transitions on stroke + opacity changes
   so the selection / flash state-machine reads as movement, not as a
   hard cut. Hover lifts opacity to 1 so a user can confirm "yes, this
   line goes there" by mousing over without committing to a click. */
:deep(.vue-flow__edge .vue-flow__edge-path) {
  transition: stroke 160ms ease, opacity 160ms ease, stroke-width 160ms ease;
}
:deep(.vue-flow__edge:hover .vue-flow__edge-path) {
  opacity: 1 !important;
}
:deep(.vue-flow__edge:hover) { cursor: pointer; }
</style>
