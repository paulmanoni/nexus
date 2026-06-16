<script setup>
import { ref, computed, onMounted, onUnmounted, markRaw, nextTick, provide, watch } from 'vue'
import { VueFlow, useVueFlow, Position, MarkerType } from '@vue-flow/core'
import { Background } from '@vue-flow/background'
import { Controls } from '@vue-flow/controls'
import { MiniMap } from '@vue-flow/minimap'
import {
  Hexagon, Search, SlidersHorizontal, Sun, Moon, LayoutGrid, Workflow,
  Maximize2, Zap, AlertTriangle, Gauge, ShieldCheck, Layers,
} from 'lucide-vue-next'
import { elkLayout, applyPositions } from '../lib/elkLayout.js'

import ServiceNode from '../components/ServiceNode.vue'
import ServiceDepNode from '../components/ServiceDepNode.vue'
import WorkerNode from '../components/WorkerNode.vue'
import CronNode from '../components/CronNode.vue'
import ResourceNode from '../components/ResourceNode.vue'
import InternetNode from '../components/InternetNode.vue'
import LaneNode from '../components/LaneNode.vue'
import ErrorDialog from '../components/ErrorDialog.vue'
import PacketOverlay from '../components/PacketOverlay.vue'
import Drawer from '../components/Drawer.vue'
import OpDetail from '../components/drawer/OpDetail.vue'
import ResourceDetail from '../components/drawer/ResourceDetail.vue'
import WorkerDetail from '../components/drawer/WorkerDetail.vue'
import CronDetail from '../components/drawer/CronDetail.vue'
import AuthDetail from '../components/drawer/AuthDetail.vue'
import CmdK from '../components/CmdK.vue'
import ClusterNode from '../components/ClusterNode.vue'
import Inspector from '../components/Inspector.vue'
import TweaksPanel from '../components/TweaksPanel.vue'
import PluginChips from '../components/PluginChips.vue'
import { subscribeEvents, subscribeLive, fetchConfig, triggerCron, setCronPaused } from '../lib/api.js'
import { buildClusters, computeVisible } from '../lib/topology.js'

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
  cluster: markRaw(ClusterNode),
  lane: markRaw(LaneNode),
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

// loadSet / saveSet persist a Set of keys to localStorage so drill-down +
// expand state survive a reload. Best-effort (private mode / quota safe).
function loadSet(key) {
  try { const r = localStorage.getItem(key); return new Set(r ? JSON.parse(r) : []) } catch { return new Set() }
}
function saveSet(key, set) {
  try { localStorage.setItem(key, JSON.stringify([...set])) } catch { /* best effort */ }
}

// expandedGroups holds the set of group-keys whose cards are currently
// rendering ALL their endpoints (toggle clicked). Survives WS snapshots
// because it lives outside latestSnapshot. Mutating triggers a load()
// rerun so the layout reflows around the now-taller card.
const expandedGroups = ref(loadSet('nexus.expandedGroups'))
watch(expandedGroups, () => saveSet('nexus.expandedGroups', expandedGroups.value), { deep: true })
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

// expandedClusters holds the keys of drill-down clusters the user has
// opened. Empty = everything collapsed (the default at scale: a handful of
// cluster cards instead of 1000 nodes). Expanding reveals a cluster's
// members and reruns layout. Like expandedGroups it lives outside the
// snapshot so it survives live frames; replaced wholesale so the watcher
// fires.
const expandedClusters = ref(loadSet('nexus.expandedClusters'))
function expandCluster(key) {
  if (!key || expandedClusters.value.has(key)) return
  const next = new Set(expandedClusters.value)
  next.add(key)
  expandedClusters.value = next
}
function collapseAllClusters() {
  if (expandedClusters.value.size === 0) return
  expandedClusters.value = new Set()
}
provide('nexus.expandCluster', expandCluster)
watch(expandedClusters, () => {
  saveSet('nexus.expandedClusters', expandedClusters.value)
  if (latestSnapshot.value) load()
})

// stampClusters tags each node with the cluster it belongs to (_cluster +
// label + kind), which topology.buildClusters groups on. Primary grouping is
// by MODULE: each nexus.Module becomes a cluster holding its card plus the
// resources / workers / crons it uses. A resource shared across modules goes
// to a "Shared data" cluster; cross-cutting consumed services + anything
// unattributable go to "Shared". Falls back to DEPLOYMENT tags, then TIER
// (Services / Data / Workers / Schedules), for apps that don't use modules.
// Internet has no cluster — it always renders standalone on the far left.
//
// ctx: { resolveCard(serviceName) -> cardId|null, svcToDep }
function stampClusters(allNodes, ctx) {
  const resolveCard = (ctx && ctx.resolveCard) || (() => null)
  const svcToDep = (ctx && ctx.svcToDep) || {}
  const isModuleId = (id) => typeof id === 'string' && id.startsWith('mod:')

  const useModules = allNodes.some(n => n.type === 'service' && n.data.isModule)
  if (useModules) {
    const SHARED = 'grp:shared', DATA = 'grp:data', OTHER = 'grp:other'
    const labelFor = (key) => {
      if (key === SHARED) return 'Shared'
      if (key === DATA) return 'Shared data'
      if (key === OTHER) return 'Other services'
      const i = key.indexOf(':')
      return i >= 0 ? key.slice(i + 1) : key
    }
    const moduleCardFor = (svcName) => {
      const c = resolveCard(svcName)
      return isModuleId(c) ? c : null
    }
    const set = (n, key) => {
      n.data._cluster = key
      n.data._clusterLabel = labelFor(key)
      n.data._clusterKind = key === DATA ? 'data' : 'module'
    }
    for (const n of allNodes) {
      if (n.type === 'internet') { n.data._cluster = null; continue }
      switch (n.type) {
        case 'service':
          // A module card is its own cluster; bare service cards (no module)
          // pool into "Other services" so they don't each become a cluster.
          set(n, n.data.isModule ? n.id : OTHER)
          break
        case 'resource': {
          const mods = new Set()
          for (const svc of n.data.attachedTo || []) {
            const m = moduleCardFor(svc)
            if (m) mods.add(m)
          }
          set(n, mods.size === 1 ? [...mods][0] : mods.size > 1 ? DATA : SHARED)
          break
        }
        case 'worker': {
          const m = moduleCardFor((n.data.serviceDeps || [])[0])
          set(n, m || SHARED)
          break
        }
        case 'cron': {
          const m = n.data.service ? moduleCardFor(n.data.service) : null
          set(n, m || SHARED)
          break
        }
        default: // serviceDep + anything else: cross-cutting
          set(n, SHARED)
      }
    }
    return
  }

  // --- Fallback: deployment tags, then tiers (apps without modules). ----
  const depOf = (n) => {
    if (n.type === 'service' || n.type === 'worker' || n.type === 'cron') return n.data.deployment || ''
    if (n.type === 'serviceDep') return svcToDep[n.data.name] || ''
    return ''
  }
  const useDeployments = allNodes.some(n => depOf(n))
  for (const n of allNodes) {
    if (n.type === 'internet') { n.data._cluster = null; continue }
    if (useDeployments) {
      const dep = depOf(n) || 'default'
      n.data._cluster = 'dep:' + dep
      n.data._clusterLabel = dep === 'default' ? 'Ungrouped' : dep
      n.data._clusterKind = 'deployment'
    } else {
      let key = 'tier:services', label = 'Services', kind = 'services'
      if (n.type === 'resource')   { key = 'tier:data';      label = 'Data';      kind = 'data' }
      else if (n.type === 'worker') { key = 'tier:workers';   label = 'Workers';   kind = 'workers' }
      else if (n.type === 'cron')   { key = 'tier:schedules'; label = 'Schedules'; kind = 'schedules' }
      n.data._cluster = key
      n.data._clusterLabel = label
      n.data._clusterKind = kind
    }
  }
}

// Dark-mode theme toggle. tokens.css already ships a [data-theme="dark"]
// block; we just flip the attribute on <html> and remember the choice.
// Dark is the redesign's default identity; honour a saved preference.
const theme = ref(typeof localStorage !== 'undefined' && localStorage.getItem('nexus.theme') === 'light' ? 'light' : 'dark')
function applyTheme() {
  if (typeof document !== 'undefined') document.documentElement.setAttribute('data-theme', theme.value)
}
function setTheme(v) {
  theme.value = v
  if (typeof localStorage !== 'undefined') localStorage.setItem('nexus.theme', theme.value)
  applyTheme()
}
function toggleTheme() { setTheme(theme.value === 'dark' ? 'light' : 'dark') }
applyTheme()

// ─── Redesign chrome state ─────────────────────────────────────────
// All persisted to localStorage so a reload keeps the operator's
// arrangement. `mode` switches ELK between the layered (LR ranks) and
// flow (organic stress) layouts; `live` gates the traffic pulse; the
// rest drive the Tweaks panel (density / accent / client glyph / lanes).
function lsGet(k, d) {
  try { const v = localStorage.getItem(k); return v == null ? d : JSON.parse(v) } catch { return d }
}
function lsSet(k, v) { try { localStorage.setItem(k, JSON.stringify(v)) } catch { /* best effort */ } }

const mode = ref(lsGet('nexus.mode', 'layered'))
const live = ref(lsGet('nexus.live', true))
const density = ref(lsGet('nexus.density', 'regular'))
const laneLabels = ref(lsGet('nexus.laneLabels', true))
const clientIcon = ref(lsGet('nexus.clientIcon', 'globe'))
const tweaksOpen = ref(false)
const selectedNodeId = ref(lsGet('nexus.selNode', null))

const DEFAULT_ACCENT = ''   // '' = the token default (teal / dark, signal / light)
const accent = ref(lsGet('nexus.accent', DEFAULT_ACCENT))

provide('nexus.selectedNodeId', selectedNodeId)
provide('nexus.density', density)
provide('nexus.clientIcon', clientIcon)

// applyAccent overrides the --accent family on <html> from a single hex,
// matching the redesign's live accent picker. Empty restores the token
// default for the active theme.
function hexA(hex, a) {
  const m = hex.replace('#', '')
  const r = parseInt(m.slice(0, 2), 16), g = parseInt(m.slice(2, 4), 16), b = parseInt(m.slice(4, 6), 16)
  return `rgba(${r},${g},${b},${a})`
}
function applyAccent(c) {
  if (typeof document === 'undefined') return
  const s = document.documentElement.style
  const keys = ['--accent', '--accent-2', '--accent-soft', '--accent-line', '--accent-glow', '--canvas-glow']
  if (!c) { keys.forEach(k => s.removeProperty(k)); return }
  s.setProperty('--accent', c)
  s.setProperty('--accent-2', c)
  s.setProperty('--accent-soft', hexA(c, 0.13))
  s.setProperty('--accent-line', hexA(c, 0.42))
  s.setProperty('--accent-glow', hexA(c, 0.42))
  s.setProperty('--canvas-glow', hexA(c, 0.09))
}
applyAccent(accent.value)

// Tweak setters — mutate + persist; layout-affecting ones rerun load().
function setAccent(c) { accent.value = (c === accent.value ? '' : c); applyAccent(accent.value); lsSet('nexus.accent', accent.value) }
function setDensity(d) { density.value = d; lsSet('nexus.density', d) }
function setMode(m) { mode.value = m }
function setLane(v) { laneLabels.value = v }
function setLive(v) { live.value = v }
function setClient(ic) { clientIcon.value = ic; lsSet('nexus.clientIcon', ic) }
function toggleLive() { setLive(!live.value) }

watch(mode, v => { lsSet('nexus.mode', v); if (latestSnapshot.value) load(); nextTick(() => fitView(FIT_OPTS)) })
watch(laneLabels, v => { lsSet('nexus.laneLabels', v); if (latestSnapshot.value) load() })
watch(live, v => lsSet('nexus.live', v))
watch(selectedNodeId, v => lsSet('nexus.selNode', v))

// App identity for the header brand block.
const config = ref({ Name: 'Nexus', Deployment: '', Version: '' })
const brandName = computed(() => config.value.Name || 'Nexus')
const brandEnv = computed(() => config.value.Deployment || 'development')
const brandBase = computed(() => (typeof location !== 'undefined' && location.port ? ':' + location.port : ':8080'))
// Background dot color — literal because SVG fill attrs don't resolve CSS
// vars; track the theme so dark/light stay correct.
const gridColor = computed(() => (theme.value === 'dark' ? '#161d27' : '#dde3ea'))

// onCanvasNodeClick — node selection drives the Inspector + edge focus.
// Lanes are inert; clusters drill in; everything else selects.
function onCanvasNodeClick({ node }) {
  if (!node || node.type === 'lane') return
  if (node.type === 'cluster') { expandCluster(node.data.clusterKey); return }
  selectedNodeId.value = node.id
  clearOp()
}

// MiniMap node color — match each node's category so the overview reads as
// a shrunk version of the canvas. SVG fill attribute can't resolve CSS
// vars, so use the literal hues from tokens.css §3.
function minimapNodeColor(n) {
  switch (n.type) {
    case 'lane':      return 'transparent'
    case 'cluster':   return '#2fb8e0'  // --cat-peer
    case 'resource':  return '#6aa6ff'  // --cat-database
    case 'worker':    return '#ff8a4c'  // --cat-worker
    case 'cron':      return '#f25fb0'  // --cat-cron
    case 'internet':  return '#8693a3'  // --cat-internet
    case 'serviceDep':return '#8b8bf0'  // --cat-service
    default:          return '#8b8bf0'  // service/module
  }
}

// LOD_NODE_THRESHOLD: above this many VISIBLE nodes, module cards render in
// a compact summary form (no endpoint rows) so a densely-expanded view
// stays legible and light. Count-based (not zoom) so layout only reruns on
// topology/expand changes, never on pan/zoom.
const LOD_NODE_THRESHOLD = 40

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

const { fitView, onNodesInitialized, onPaneClick, onNodeClick, onNodeDragStop, updateNodeInternals } = useVueFlow()
onNodeClick(onCanvasNodeClick)
// FIT_OPTS — shared between initial paint and topology-change re-fits.
// minZoom floors the auto-fit so a busy topology (many modules /
// endpoints) doesn't shrink cards into illegible postage stamps;
// instead we stop zooming out at 0.6 and let the user pan. maxZoom
// caps the other end so a single tiny graph doesn't blow up to 1.5x.
const FIT_OPTS = { padding: 0.2, minZoom: 0.6, maxZoom: 1 }
onNodesInitialized(() => fitView(FIT_OPTS))

// lastTopologyFingerprint is a sorted-id-list snapshot of the last
// rendered node set. load() compares the next render's fingerprint
// to this; an unchanged fingerprint means "same topology, just new
// counters" and we skip fitView so the user's pan/zoom survives the
// 5s poll. A changed fingerprint (new module appeared, etc.) calls
// fitView to bring the new node into view.
let lastTopologyFingerprint = ''
// layoutSeq guards against overlapping async layouts: ELK runs off the
// synchronous path, and the /__nexus/live socket can fire another load()
// before the previous layout resolves. Each load() claims the next seq;
// when its layout resolves it only commits if it's still the latest — a
// superseded run drops its result so positions never apply out of order.
let layoutSeq = 0

// userPositions tracks per-node drag overrides so polling doesn't snap a
// card back to the layout engine's slot after the user dropped it somewhere
// meaningful. Persisted in localStorage (per id) so the arrangement — and
// the drill-down state below — survives a full reload, not just a soft
// refresh. Stale ids for nodes that no longer exist simply go unused.
const userPositions = (() => {
  try {
    const raw = localStorage.getItem('nexus.archPositions')
    return raw ? new Map(Object.entries(JSON.parse(raw))) : new Map()
  } catch {
    return new Map()
  }
})()
function persistPositions() {
  try {
    localStorage.setItem('nexus.archPositions', JSON.stringify(Object.fromEntries(userPositions)))
  } catch { /* quota / private mode — best effort */ }
}

// lastPositions caches the most recently committed node positions, keyed by
// id. When a poll arrives with the SAME topology (same visible ids + LOD),
// load() reuses these instead of re-running ELK — node DATA still refreshes
// (traffic counts, errors), but the expensive layout is skipped. This is
// what keeps idle 2s polling cheap at 1000 nodes.
let lastPositions = new Map()
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
  selectedNodeId.value = null
})

// estimateServiceWidth scales the card width with the longest endpoint
// label so paths like `GET /api/v1/users/:id/permissions` stop ellipsis-
// truncating into illegibility. Width is fed BOTH into dagre (so
// neighbours don't overlap) and into the rendered card style (so the
// element actually renders that wide). Clamped between a comfortable
// minimum and a sensible maximum — past the cap, long paths fall back
// to truncation rather than letting one outlier card dominate the row.
// Calm module cards are a fixed size in the redesign — a header row + one
// muted meta line. ELK packs uniform boxes; the rendered padding flexes
// with density but the layout box stays constant.
const SERVICE_NODE_WIDTH = 214
const SERVICE_NODE_HEIGHT = 64
function estimateServiceWidth() { return SERVICE_NODE_WIDTH }
function estimateServiceHeight() { return SERVICE_NODE_HEIGHT }

function estimateResourceHeight(data) {
  const detailKeys = Object.keys(data.details || {}).slice(0, 3).length
  const desc = data.description ? 22 : 0
  return 40 + desc + (detailKeys ? detailKeys * 18 + 16 : 0)
}

const NODE_WIDTH_RESOURCE = 200
const GAP = 48

// layout positions the graph with ELK (async). nodeBoxSize supplies each
// node's reserved box; gridLayout is the synchronous fallback for the
// edgeless case and for an ELK failure (returned via Promise.resolve so the
// call site can always `await`).
// FLOW_OPTS swaps ELK's layered ranks for the stress (force-directed)
// algorithm so "Flow" mode reads as an organic web rather than tidy
// columns — matching the redesign's two layout personalities.
const FLOW_OPTS = {
  'elk.algorithm': 'org.eclipse.elk.stress',
  'elk.stress.desiredEdgeLength': '170',
  'elk.spacing.nodeNode': '70',
}
async function layout(ns, es, mode = 'layered') {
  if (es.length === 0) return gridLayout(ns)
  const sized = ns.map(n => {
    const { w, h } = nodeBoxSize(n)
    return { id: n.id, width: w, height: h }
  })
  try {
    const pos = await elkLayout(sized, es, mode === 'flow' ? FLOW_OPTS : {})
    return applyPositions(ns, pos)
  } catch (err) {
    console.warn('[nexus] ELK layout failed — falling back to grid:', err)
    return gridLayout(ns)
  }
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
  // Collapsed cluster card: fixed-ish box (head + meta + optional stats row).
  if (n.type === 'cluster') {
    const s = n.data.summary || {}
    const statsRow = (s.endpoints || s.requests || s.errors) ? 24 : 0
    return { w: 260, h: 96 + statsRow }
  }
  return { w: estimateServiceWidth(n.data), h: estimateServiceHeight(n.data) }
}

function gridLayout(ns) {
  const cols = Math.min(ns.length, 3)
  const rowHeights = []
  // Track the widest service card per column so the grid spaces columns
  // around the actual card widths instead of a constant — a wide module
  // (long REST paths) and a narrow one would overlap if we kept a fixed
  // column pitch.
  const colWidths = new Array(cols).fill(SERVICE_WIDTH_MIN)
  ns.forEach((n, i) => {
    const col = i % cols
    const row = Math.floor(i / cols)
    let h
    if (n.type === 'resource') h = estimateResourceHeight(n.data)
    else if (n.type === 'serviceDep') h = estimateServiceDepHeight(n.data)
    else if (n.type === 'worker') h = estimateWorkerHeight(n.data)
    else if (n.type === 'cron') h = estimateCronHeight(n.data)
    else {
      h = estimateServiceHeight(n.data)
      const w = estimateServiceWidth(n.data)
      if (w > colWidths[col]) colWidths[col] = w
    }
    rowHeights[row] = Math.max(rowHeights[row] || 0, h)
  })
  const rowY = [0]
  for (let r = 1; r < rowHeights.length; r++) {
    rowY.push(rowY[r - 1] + rowHeights[r - 1] + GAP)
  }
  const colX = [0]
  for (let c = 1; c < cols; c++) {
    colX.push(colX[c - 1] + colWidths[c - 1] + GAP)
  }
  return ns.map((n, i) => {
    const col = i % cols
    const row = Math.floor(i / cols)
    return {
      ...n,
      position: { x: colX[col], y: rowY[row] },
      targetPosition: Position.Left,
      sourcePosition: Position.Right
    }
  })
}

// mergeCardEdges collapses parallel edges between the same (source,target)
// pair into one card-level bundle. Calm cards expose no per-op handles, so
// the dozens of per-endpoint edges a module emits toward one resource become
// a single wire that accumulates the contributing ops (for flash lookups)
// and a count (for the bundle badge). Self-loops are dropped.
function mergeCardEdges(list) {
  const byPair = new Map()
  for (const e of list) {
    const s = e.source, t = e.target
    if (s === t) continue
    const k = s + ' ' + t
    const opsOf = Array.isArray(e.data && e.data.ops) ? e.data.ops
      : (e.data && e.data.op ? [e.data.op] : [])
    const prev = byPair.get(k)
    if (!prev) {
      byPair.set(k, {
        ...e,
        sourceHandle: null,
        targetHandle: null,
        data: { ...e.data, ops: [...opsOf], count: (e.data && e.data.count) || 1 },
      })
    } else {
      prev.data.count += (e.data && e.data.count) || 1
      const set = new Set(prev.data.ops || [])
      for (const o of opsOf) set.add(o)
      prev.data.ops = [...set]
      // A bundle of >1 distinct ops loses its single-op identity (op=null
      // marks it aggregated for restyle); preserve semantic flags.
      prev.data.op = prev.data.ops.length === 1 ? prev.data.ops[0] : null
      if (e.data && e.data.inbound) prev.data.inbound = true
      if (e.data && e.data.worker) prev.data.worker = true
      if (e.data && e.data.serviceLevel) prev.data.serviceLevel = true
      if (e.data && e.data.resourceLevel) prev.data.resourceLevel = true
    }
  }
  return [...byPair.values()]
}

// buildLaneNodes places a tier label above each populated column of the
// layered layout (Clients / Services / Data / Jobs). Rendered as inert Vue
// Flow nodes so they pan + zoom with the canvas, matching the redesign's
// lane bands. Only used in layered mode with lane labels enabled.
function buildLaneNodes(positioned) {
  const TIERS = [
    { key: 'clients', text: 'Clients', test: n => n.type === 'internet' },
    { key: 'services', text: 'Services', test: n => n.type === 'service' || n.type === 'serviceDep' || (n.type === 'cluster' && n.data.kind !== 'data') },
    { key: 'data', text: 'Data', test: n => n.type === 'resource' || (n.type === 'cluster' && n.data.kind === 'data') },
    { key: 'jobs', text: 'Jobs', test: n => n.type === 'worker' || n.type === 'cron' },
  ]
  const real = positioned.filter(n => n.type !== 'lane')
  if (!real.length) return []
  const minY = Math.min(...real.map(n => n.position.y))
  const lanes = []
  for (const t of TIERS) {
    const xs = real.filter(t.test).map(n => n.position.x)
    if (!xs.length) continue
    lanes.push({
      id: 'lane:' + t.key,
      type: 'lane',
      position: { x: Math.min(...xs), y: minY - 70 },
      draggable: false, selectable: false, connectable: false,
      data: { text: t.text }, zIndex: 0,
    })
  }
  return lanes
}

// latestSnapshot holds the most recent /__nexus/live frame. load() reads
// from it instead of fetching, so the WS push is the single source of
// state for the architecture view. null until the first snapshot arrives.
const latestSnapshot = ref(null)

async function load() {
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
    // Calm cards don't render rows, so there's no per-card truncation —
    // the full endpoint set ships in node data for health/auth tallies +
    // flash indexing, and the Inspector shows them grouped on selection.
    const displayed = sorted
    return { g, displayed, sorted, isExpanded, total: g.endpoints.length }
  })

  const groupNodes = sortedGroups.map(({ g, displayed, isExpanded, total }) => {
    const data = {
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
    }
    // cardWidth is the dagre-allocated width for THIS card, derived
    // from its longest endpoint label. Threaded via data → inline
    // style in ServiceNode so the rendered element exactly matches
    // the box dagre placed it in. Without this, dagre reserves the
    // grown width but the CSS clamp would still render at the old
    // max — neighbours then look wrongly spaced.
    data.cardWidth = estimateServiceWidth(data)
    return {
      id: g.key,
      type: 'service',
      position: { x: 0, y: 0 },
      data,
    }
  })

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

  // Resource → resource dep edges. Surfaces hard dependencies between
  // resources declared via resource.DependsOn(...names) — the canonical
  // pairing is a cache fronting a database, or a queue whose consumers
  // persist into Postgres. Each entry in r.dependsOn becomes one edge
  // res:<r.name> → res:<targetName>. Unknown targets render as
  // dangling edges with a console warning so authoring bugs are
  // obvious rather than silent.
  const resourceNames = new Set((rsData.resources || []).map(r => r.name))
  for (const r of rsData.resources || []) {
    const deps = Array.isArray(r.dependsOn) ? r.dependsOn : []
    for (const dep of deps) {
      if (!dep) continue
      if (!resourceNames.has(dep)) {
        if (typeof console !== 'undefined') {
          console.warn(`[nexus] resource ${r.name} depends on unknown resource "${dep}"`)
        }
        continue
      }
      if (dep === r.name) continue // self-loop bug
      edgeList.push({
        id: `e:res:${r.name}->res:${dep}`,
        source: `res:${r.name}`,
        target: `res:${dep}`,
        markerEnd: MarkerType.ArrowClosed,
        data: { service: r.name, target: dep, targetKind: 'resource', op: null, resourceLevel: true },
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
  // Inbound entry lane — one aggregated Internet → module edge per group.
  // Calm cards have no per-row handles to anchor a per-op inbound to, so a
  // single card-level lane carries all inbound traffic; the flash animator
  // lights it on any request.op for the module.
  for (const { g } of sortedGroups) {
    edgeList.push({
      id: `e:internet->${g.key}`,
      source: INTERNET_ID,
      target: g.key,
      markerEnd: MarkerType.ArrowClosed,
      data: { service: g.service, target: g.name, targetKind: 'module', op: null, inbound: true, groupKey: g.key },
    })
  }

  const all = [internetNode, ...groupNodes, ...svcDepNodes, ...workerNodes, ...cronNodes, ...rsNodes]

  // Hierarchical drill-down: tag every node with its cluster, group them,
  // then collapse to the visible set for the current expand state. Default
  // (nothing expanded) renders a handful of cluster cards instead of the
  // full graph; only the visible subgraph is laid out + rendered, which is
  // what keeps the canvas usable at 1000+ nodes. Edges crossing a collapsed
  // boundary are rerouted onto the cluster node (aggregated with a count).
  stampClusters(all, { resolveCard: resolveServiceCard, svcToDep })
  const clusters = buildClusters(all)
  // Clustering exists to keep 1000-node apps legible — but for normal-sized
  // topologies the redesign wants the calm module + resource cards shown
  // directly (the lane'd look from the screenshots), not collapsed cluster
  // pucks. Below the threshold we auto-expand every cluster so members
  // render; at scale we honour the user's manual drill-down state.
  const AUTO_EXPAND_BELOW = 90
  const effectiveExpanded = all.length <= AUTO_EXPAND_BELOW
    ? new Set(clusters.keys())
    : expandedClusters.value
  const { nodes: visNodes, edges: visEdges } = computeVisible(all, edgeList, clusters, effectiveExpanded)
  const visIds = new Set(visNodes.map(n => n.id))

  // Calm cards expose no per-op handles — collapse parallel edges to one
  // card-level bundle per (source,target) pair.
  const finalEdges = mergeCardEdges(visEdges)

  // Diagnostic: surface the built graph to window so operators can
  // verify service-level edges from DevTools without re-reading this
  // file. Cheap (single assignment per poll) and invaluable when an
  // expected dep edge doesn't render.
  if (typeof window !== 'undefined') {
    window.__nexusArch = {
      clusters: [...clusters.values()].map(c => ({ key: c.key, members: c.memberIds.size })),
      expanded: [...expandedClusters.value],
      visibleNodes: visNodes.map(n => n.id),
      edges: finalEdges.map(e => ({ id: e.id, source: e.source, target: e.target, count: e.data?.count ?? 1 })),
    }
  }
  // Perf gate: a topology fingerprint of the visible ids + layout mode.
  // When unchanged from the last commit, reuse cached positions and SKIP
  // ELK entirely — node DATA is still rebuilt each poll (live traffic/error
  // counts), but the expensive layout doesn't rerun. This is what keeps
  // idle 2s polling cheap at 1000 nodes. fitView likewise only fires when
  // the layout actually changed, so steady-state polling keeps pan + zoom.
  const fp = visNodes.map(n => n.id).sort().join('|') + '|' + mode.value + (laneLabels.value ? '|lanes' : '')
  const unchanged = fp === lastTopologyFingerprint && lastPositions.size > 0
  const seq = ++layoutSeq
  let laid
  if (unchanged) {
    laid = visNodes.map(n => ({
      ...n,
      position: lastPositions.get(n.id) || { x: 0, y: 0 },
      targetPosition: Position.Left,
      sourcePosition: Position.Right,
    }))
  } else {
    try {
      laid = await layout(visNodes, finalEdges, mode.value)
    } catch (err) {
      console.error('[nexus] Architecture layout failed:', err, { nodeCount: visNodes.length, edgeCount: finalEdges.length })
      return
    }
    // A newer load() started while ELK was working — drop this stale result.
    if (seq !== layoutSeq) return
  }
  try {
    // Apply user-drag overrides so dragged cards don't snap back to the
    // layout engine's slot on the next poll. The override map only kicks in
    // for ids the user explicitly moved.
    const placed = laid.map(n => {
      const moved = userPositions.get(n.id)
      return moved ? { ...n, position: moved } : n
    })
    // Lane labels float above each populated column in layered mode.
    const laneNodes = (mode.value === 'layered' && laneLabels.value) ? buildLaneNodes(placed) : []
    const nextNodes = [...placed, ...laneNodes]
    lastTopologyFingerprint = fp
    lastPositions = new Map(placed.map(n => [n.id, n.position]))
    nodes.value = nextNodes
    rawEdges.value = finalEdges
    indexEndpointEdges(finalEdges)
    // Only index groups that actually render — collapsed-cluster members are
    // hidden, and the flash/packet animator must not target a missing node.
    indexEndpointGroups(groupNodes.filter(g => visIds.has(g.id)))
    // Commit edges AFTER the nodes mount. Expanding a cluster adds member
    // nodes in the same flush as the edges that reference them; if edges are
    // assigned in the same tick, Vue Flow resolves them against nodes whose
    // handles aren't measured yet (and onlyRenderVisibleElements means an
    // off-viewport new node never measures) — so the edges silently fail to
    // route until a refresh re-fits the graph. Deferring one tick lets the
    // nodes mount; updateNodeInternals forces handle re-measure; and fitView
    // (on a real change) brings new nodes into view so virtualization renders
    // and measures them. This is the fix for "edges don't show until refresh
    // after expanding a group".
    nextTick(() => {
      edges.value = restyleEdges(finalEdges, selectedNodeId.value, flashedEdges.value)
      if (!unchanged) {
        fitView(FIT_OPTS)
        nextTick(() => updateNodeInternals(nextNodes.map(n => n.id)))
      }
    })
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
  borderStr: '#9ca3af',  // bumped from #d1d5db so service-level wires
                         // and aggregated edges stay legible against
                         // the canvas background — older value blended
                         // into bg-subtle.
  internet:  '#64748b',  // --cat-internet
  worker:    '#f97316',  // --cat-worker
  resource:  '#0ea5e9',  // --cat-database; used for resource→resource
                         // dep edges so the data-tier wiring reads as
                         // its own layer instead of grey-on-grey.
  error:     '#ef4444',  // --st-error
}

// buildMarker returns a Vue Flow markerEnd object whose arrowhead matches
// the path stroke. Smaller than the default ArrowClosed (14px instead of
// the ~20px default) so arrows read as direction cues, not as icons.
function buildMarker(color) {
  return { type: MarkerType.ArrowClosed, width: 14, height: 14, color }
}

// edgePalette resolves the live theme + accent colors from CSS vars so
// edges restyle correctly across light/dark and the Tweaks accent picker.
// SVG path strokes + marker fills need literal colors (CSS vars don't
// resolve in the marker attribute), so we read the computed values once per
// restyle pass rather than per edge.
function edgePalette() {
  const fb = { accent: '#2fe0c6', edge: '#2b333f', edgeStrong: '#3c4655', internet: '#8693a3', worker: '#ff8a4c', resource: '#6aa6ff', error: '#ff6166' }
  if (typeof document === 'undefined') return fb
  const cs = getComputedStyle(document.documentElement)
  const v = (n, f) => { const x = cs.getPropertyValue(n).trim(); return x || f }
  return {
    accent: v('--accent', fb.accent),
    edge: v('--edge', fb.edge),
    edgeStrong: v('--edge-strong', fb.edgeStrong),
    internet: v('--cat-internet', fb.internet),
    worker: v('--cat-worker', fb.worker),
    resource: v('--cat-database', fb.resource),
    error: v('--err', fb.error),
  }
}

// restyleEdges returns a fresh edges array with type/path/style applied
// based on the current NODE selection + live-traffic flash state. Every
// edge is a smoothstep (orthogonal, rounded) so the canvas reads as
// infrastructure. When a node is selected, edges touching it pop in the
// accent and everything else fades to scaffolding; idle edges paint by
// semantic kind (inbound / worker / resource-level / service-level / flow).
function restyleEdges(list, nodeSel, flashed) {
  const C = edgePalette()
  return list.map(e => {
    const base = { ...e }
    base.type = 'smoothstep'
    base.pathOptions = { borderRadius: 12, offset: 20 }

    const isAggregated   = e.data.op === null
    const isInbound      = !!e.data.inbound
    const isWorker       = !!e.data.worker
    const isServiceLvl   = !!e.data.serviceLevel
    const isResourceLvl  = !!e.data.resourceLevel
    const flashState     = flashed && flashed.get(e.id)
    const touches        = nodeSel && (e.source === nodeSel || e.target === nodeSel)

    let stroke, width, opacity, animated = false, dashed = false

    if (flashState) {
      stroke = flashState === 'error' ? C.error : C.accent
      width = 2.2; opacity = 1; animated = true
    } else if (nodeSel) {
      if (touches) { stroke = C.accent; width = 2; opacity = 1; animated = true }
      else { stroke = C.edge; width = 1; opacity = 0.12 }
    } else if (isWorker) {
      stroke = C.worker; width = 1.5; opacity = 0.85
    } else if (isInbound) {
      stroke = C.internet; width = 1.4; opacity = 0.7
    } else if (isResourceLvl) {
      stroke = C.resource; width = 1.4; opacity = 0.9; dashed = true
    } else if (isServiceLvl) {
      stroke = C.edgeStrong; width = 1.4; opacity = 0.9; dashed = true
    } else if (isAggregated) {
      stroke = C.edgeStrong; width = 1.4; opacity = 0.9
    } else {
      stroke = C.accent; width = 1.3; opacity = 0.6
    }

    const count = (e.data && e.data.count) || 1
    if (count > 1 && !flashState) {
      width = Math.max(width, Math.min(1.4 + Math.log2(count) * 0.5, 4))
    }
    base.style = { stroke, strokeWidth: width, opacity }
    if (dashed) base.style.strokeDasharray = '5 4'
    base.animated = animated
    base.markerEnd = buildMarker(stroke)
    const dimmed = nodeSel && !flashState && !touches
    if (count > 1 && !dimmed) {
      base.label = String(count)
      base.labelShowBg = true
      base.labelBgPadding = [5, 3]
      base.labelBgBorderRadius = 7
      base.labelBgStyle = { fill: 'var(--surface)', stroke: 'var(--line)', strokeWidth: 1 }
      base.labelStyle = { fill: 'var(--ink-3)', fontSize: '10px', fontFamily: 'var(--font-mono)', fontWeight: 600 }
    } else {
      base.label = undefined
    }
    return base
  })
}

// rawEdges holds the un-styled edge list so restyle calls don't stack
// styling on top of styling. flashedEdges is the map of edge id → state
// ('ok' | 'error') that should render in the bright "live-traffic"
// style right now; entries clear themselves via setTimeout.
const rawEdges = ref([])
const flashedEdges = ref(new Map())

// focusSet drives focus-mode dimming: when a row/op is selected, it's the
// selected card plus everything one edge away. ServiceNode dims any card
// not in the set so the selected neighbourhood pops out of a busy canvas.
// null = nothing selected → no dimming.
const focusSet = computed(() => {
  const id = selectedNodeId.value
  if (!id) return null
  const set = new Set([id])
  for (const e of rawEdges.value) {
    if (e.source === id) set.add(e.target)
    else if (e.target === id) set.add(e.source)
  }
  return set
})
provide('nexus.focusSet', focusSet)
watch([selectedNodeId, flashedEdges], () => {
  edges.value = restyleEdges(rawEdges.value, selectedNodeId.value, flashedEdges.value)
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
  // Live toggle off → no edge pulses / packet flights (scrubber replay
  // passes force=true and is exempt so rewinding still animates).
  if (!live.value && !force) return
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

// ─── Inspector model + live activity ──────────────────────────────
// `now` ticks once a second so relative "ago" labels + the rolling
// rate windows recompute without a per-event watcher.
const now = ref(0)

function iconForResource(kind) {
  if (kind === 'cache') return 'cache'
  if (kind === 'queue') return 'queue'
  if (kind === 'database') return 'database'
  return 'database'
}

// epAuth best-effort derives the redesign's auth label (null / 'required'
// / 'requires:<perm>') from an endpoint's middleware names.
function epAuth(e) {
  const mw = Array.isArray(e.Middleware) ? e.Middleware : []
  const perm = mw.find(m => m.startsWith('permission') || m.startsWith('requires'))
  if (perm) { const i = perm.indexOf(':'); return i >= 0 ? 'requires:' + perm.slice(i + 1) : 'required' }
  if (mw.some(m => m === 'auth' || m.startsWith('auth'))) return 'required'
  return null
}
function epKind(e) {
  if (e.Transport === 'rest') return 'rest'
  if (e.Transport === 'websocket') return 'ws'
  return (e.Method || '').toLowerCase().includes('mut') ? 'mutation' : 'query'
}

// rollingRps counts request.op events in the trailing 1.5s window
// (optionally filtered to a set of service names) and reports a per-second
// rate. Reads `now` so it refreshes on the tick.
function rollingRps(serviceSet) {
  void now.value
  const cut = Date.now() - 1500
  let n = 0
  for (const ev of eventHistory.value) {
    if (ev.kind !== 'request.op') continue
    if (serviceSet && !serviceSet.has(ev.service)) continue
    const ts = ev.timestamp ? new Date(ev.timestamp).getTime() : 0
    if (ts >= cut) n++
  }
  return Math.round(n / 1.5)
}

function agoLabel(ts) {
  const s = Math.max(1, Math.round((Date.now() - ts) / 1000))
  return s < 60 ? s + 's' : Math.round(s / 60) + 'm'
}

const totals = computed(() => {
  const snap = latestSnapshot.value
  if (!snap) return { services: 0, eps: 0, rps: 0, errs: 0 }
  const mods = new Set()
  for (const e of snap.endpoints || []) mods.add(e.Module ? 'mod:' + e.Module : 'svc:' + e.Service)
  let errs = 0
  for (const s of snap.stats || []) errs += s.errors || 0
  return { services: mods.size, eps: (snap.endpoints || []).length, rps: rollingRps(null), errs }
})
const liveActivity = computed(() => ({ rps: totals.value.rps, errs: totals.value.errs }))

// Overview extras — all from the live WS snapshot (no polling). The global
// middleware chain, GraphQL document-cache stats, and the auth summary the
// auth plugin contributes via RegisterSnapshotExtra.
const globalMiddleware = computed(() => {
  const snap = latestSnapshot.value
  if (!snap) return []
  return snap.global || []
})
const graphqlCache = computed(() => {
  const snap = latestSnapshot.value
  if (!snap || !Array.isArray(snap.graphqlCache)) return []
  return snap.graphqlCache.map(m => {
    const hits = m.Hits || 0, misses = m.Misses || 0
    const total = hits + misses
    return {
      path: m.path,
      hits, misses, size: m.Size || 0, capacity: m.Capacity || 0,
      hitRate: total ? Math.round((hits / total) * 100) : 0,
    }
  })
})
const authSummary = computed(() => {
  const snap = latestSnapshot.value
  const a = snap && snap.extra && snap.extra.auth
  if (!a) return null
  return { identities: a.identities || [], cachingEnabled: !!a.cachingEnabled }
})
provide('nexus.authSummary', authSummary)

const inspectorTarget = computed(() => {
  const id = selectedNodeId.value
  const snap = latestSnapshot.value
  if (!id || !snap) return null

  if (id === 'internet') {
    return { id, kind: 'clients', kindLabel: 'client', icon: 'internet', label: 'Clients',
      desc: 'External traffic — web, mobile & partner integrations', rps: rollingRps(null) }
  }
  if (id.startsWith('res:')) {
    const r = (snap.resources || []).find(x => 'res:' + x.name === id)
    if (!r) return null
    return {
      id, kind: 'resource', kindLabel: r.kind || 'resource', icon: iconForResource(r.kind),
      label: r.name, desc: r.description || '',
      resource: {
        healthy: r.healthy !== false, kind: r.kind || 'resource',
        details: r.details || {}, attachedTo: r.attachedTo || [], dependsOn: r.dependsOn || [],
      },
    }
  }
  if (id.startsWith('wk:')) {
    const w = (snap.workers || []).find(x => 'wk:' + x.Name === id)
    if (!w) return null
    return {
      id, kind: 'worker', kindLabel: 'worker', icon: 'worker', label: w.Name, desc: w.Description || '',
      worker: {
        status: w.Status || 'unknown', lastError: w.LastError || '',
        resourceDeps: w.ResourceDeps || [], serviceDeps: w.ServiceDeps || [],
      },
    }
  }
  if (id.startsWith('cron:')) {
    const c = (snap.crons || []).find(x => 'cron:' + x.name === id)
    if (!c) return null
    return {
      id, kind: 'cron', kindLabel: 'cron', icon: 'cron', label: c.name, desc: c.description || '',
      cron: {
        schedule: c.schedule || '—', paused: !!c.paused, running: !!c.running,
        nextRun: c.nextRun || null, lastRun: c.lastRun || null,
        history: Array.isArray(c.history) ? c.history.slice(0, 10) : [], service: c.service || '',
      },
    }
  }
  if (id.startsWith('dep:')) {
    const name = id.slice(4)
    const s = (snap.services || []).find(x => x.Name === name)
    return { id, kind: 'service', kindLabel: 'service', icon: 'service', label: name, desc: (s && s.Description) || 'Consumed service' }
  }
  if (id.startsWith('cluster:')) return null

  // Module / service group card.
  const groupKeyOf = (e) => e.Module ? 'mod:' + e.Module : 'svc:' + e.Service
  const eps = (snap.endpoints || []).filter(e => groupKeyOf(e) === id)
  if (!eps.length && !id.startsWith('mod:') && !id.startsWith('svc:')) return null
  const statsByKey = {}
  for (const s of snap.stats || []) statsByKey[s.key] = s
  const rlByKey = {}
  for (const r of snap.ratelimits || []) rlByKey[r.key] = r
  const serviceSet = new Set(eps.map(e => e.Service).filter(Boolean))

  let reqs = 0, errs = 0
  const items = eps.map(e => {
    const st = statsByKey[`${e.Service}.${e.Name}`] || null
    const cnt = st ? st.count || 0 : 0
    const err = st ? st.errors || 0 : 0
    reqs += cnt; errs += err
    return {
      kind: epKind(e),
      name: e.Transport === 'rest' ? e.Path : (e.Name || e.Path || ''),
      method: e.Transport === 'rest' ? e.Method : null,
      auth: epAuth(e),
      p50: st && st.p50 != null ? st.p50 : null,
      reqs: cnt,
      errors: err,
      endpoint: { ...e, Stats: st, RateLimitRecord: rlByKey[`${e.Service}.${e.Name}`] || null },
    }
  })

  // Crons + workers attributed to this module.
  const crons = []
  for (const c of snap.crons || []) {
    if (c.service && serviceSet.has(c.service)) {
      crons.push({ name: c.name, kind: 'cron', schedule: c.schedule || '—', last: c.paused ? 'paused' : (c.lastRun ? 'ran' : 'idle') })
    }
  }
  for (const w of snap.workers || []) {
    if ((w.ServiceDeps || []).some(s => serviceSet.has(s))) {
      crons.push({ name: w.Name, kind: 'worker', schedule: 'long-running', last: (w.Status || 'running') })
    }
  }

  // Recent traces for this module from the live event ring (WS, not polled).
  void now.value
  const traces = []
  for (let i = eventHistory.value.length - 1; i >= 0 && traces.length < 12; i--) {
    const ev = eventHistory.value[i]
    if (ev.kind !== 'request.op' || !serviceSet.has(ev.service)) continue
    const status = typeof ev.status === 'number' ? ev.status : (ev.error ? 500 : 200)
    const ms = typeof ev.durationMs === 'number' ? Math.round(ev.durationMs) : 0
    const ts = ev.timestamp ? new Date(ev.timestamp).getTime() : Date.now()
    traces.push({ op: ev.endpoint || '—', method: (ev.method || '').toUpperCase() || 'OP', status, ms, ago: agoLabel(ts), traceId: ev.traceId || '' })
  }

  // Live rate limits — effective limit + operator-override badge per op,
  // straight from the WS ratelimits snapshot (falls back to the declared
  // limit on the endpoint when no live record exists yet).
  const limitOps = []
  let anyOverridden = false
  for (const e of eps) {
    const rec = rlByKey[`${e.Service}.${e.Name}`]
    const eff = rec ? rec.effective : (e.RateLimit && e.RateLimit.rpm ? e.RateLimit : null)
    if (eff && eff.rpm > 0) {
      const overridden = !!(rec && rec.overridden)
      if (overridden) anyOverridden = true
      limitOps.push({
        op: e.Name || `${e.Method} ${e.Path}`,
        rpm: eff.rpm, burst: eff.burst || 0, perIP: !!eff.perIP, overridden,
      })
    }
  }
  limitOps.sort((a, b) => b.rpm - a.rpm)
  const limited = limitOps.length > 0
  const limits = limited
    ? { ops: limitOps, count: limitOps.length, overridden: anyOverridden, maxRpm: limitOps[0].rpm }
    : null

  const errPct = reqs ? Math.round((errs / reqs) * 1000) / 10 : 0
  const groupName = eps[0] ? (eps[0].Module || eps[0].Service) : id.replace(/^(mod:|svc:)/, '')
  const isModule = id.startsWith('mod:')
  return {
    id, kind: 'module', icon: isModule ? 'module' : 'service', label: groupName, desc: '',
    count: eps.length, rps: rollingRps(serviceSet), errPct, limited,
    endpoints: items, crons, traces, limits,
  }
})

// Highlight chips (errors / limits / auth) toggle the diagnostic overlay
// set; nodes + edges dim/pop accordingly via the injected overlays.
function toggleOverlay(id) {
  const next = new Set(overlays.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  setOverlay(next)
}
function fit() { fitView(FIT_OPTS) }

// Inspector → drawer bridges: clicking an endpoint row opens the deep-dive
// drawer (tester / waterfall / rate-limit editor); the error badge opens
// the recent-errors dialog.
function onInspectorOp(endpoint) {
  if (!endpoint) return
  openDrawer({ kind: 'op', key: `${endpoint.Service}.${endpoint.Name}` })
}
function onInspectorErrors(epView) {
  const e = epView && epView.endpoint
  if (!e) return
  openErrors({ service: e.Service, op: e.Name || `${e.Method} ${e.Path}` })
}
// Cross-link from a detail panel (e.g. a resource's attached service) to
// select that node.
function onInspectorPick(nodeId) {
  if (nodeId) selectedNodeId.value = nodeId
}
// Cron actions from the Inspector — trigger / pause / resume. The /__nexus/live
// WS pushes the updated cron state right after, so no manual refetch.
async function onCronAction({ name, action }) {
  try {
    if (action === 'trigger') await triggerCron(name)
    else if (action === 'pause') await setCronPaused(name, true)
    else if (action === 'resume') await setCronPaused(name, false)
  } catch (err) {
    console.error('[nexus] cron action failed:', action, name, err)
  }
}

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
  // App identity for the header brand block.
  fetchConfig().then(cfg => { if (cfg) { config.value = cfg; if (cfg.Name) document.title = cfg.Name } })
  // 1s heartbeat drives relative "ago" labels + rolling rate windows.
  nowTimer = setInterval(() => { now.value = (now.value + 1) % 100000 }, 1000)
})
let nowTimer = null
onUnmounted(() => {
  if (traceSub) traceSub.close()
  if (liveSub) liveSub.close()
  flashTimers.forEach(t => clearTimeout(t))
  if (nowTimer) clearInterval(nowTimer)
  window.removeEventListener('keydown', onGlobalKey)
})
</script>

<template>
  <div class="app" :class="'density-' + density">
    <!-- Header — brand, single Architecture tab, plugins / jump / tweaks / theme -->
    <header class="hdr">
      <div class="brand">
        <div class="brand-mark"><Hexagon :size="18" :stroke-width="2" /></div>
        <div>
          <div class="brand-name">{{ brandName }}</div>
          <div class="brand-sub">nexus · {{ brandEnv }} · {{ brandBase }}</div>
        </div>
      </div>
      <nav class="tabs"><button class="tab is-active">Architecture</button></nav>
      <div class="hdr-spacer"></div>
      <div class="hdr-tools">
        <PluginChips />
        <button class="icon-btn" @click="cmdkOpen = true" title="Jump to anything">
          <Search :size="15" :stroke-width="2" /><span class="kbd">⌘K</span><span>jump</span>
        </button>
        <button class="icon-btn" :class="{ 'is-on': tweaksOpen }" @click="tweaksOpen = !tweaksOpen" title="Tweaks">
          <SlidersHorizontal :size="15" :stroke-width="2" />
        </button>
        <button class="icon-btn" @click="toggleTheme" :title="theme === 'dark' ? 'Light theme' : 'Dark theme'">
          <component :is="theme === 'dark' ? Sun : Moon" :size="15" :stroke-width="2" />
        </button>
      </div>
    </header>

    <div class="body">
      <div class="canvas-wrap" :class="{ 'has-hl': overlays.size }" ref="canvasEl">
        <VueFlow
          :nodes="nodes"
          :edges="edges"
          :node-types="nodeTypes"
          :min-zoom="0.1"
          :max-zoom="1.5"
          style="width: 100%; height: 100%"
        >
          <Background :pattern-color="gridColor" :gap="22" :size="1.4" />
          <Controls :show-interactive="false" />
          <MiniMap pannable zoomable :node-color="minimapNodeColor" />
        </VueFlow>

        <!-- Floating toolbar (top-left): layout mode + fit/live -->
        <div class="float-tl">
          <div class="seg">
            <button :class="{ 'is-active': mode === 'layered' }" @click="setMode('layered')"><LayoutGrid :size="14" :stroke-width="2" />Layered</button>
            <button :class="{ 'is-active': mode === 'flow' }" @click="setMode('flow')"><Workflow :size="14" :stroke-width="2" />Flow</button>
          </div>
          <div class="seg">
            <button @click="fit"><Maximize2 :size="14" :stroke-width="2" />Fit</button>
            <button :class="{ 'is-active': live }" @click="toggleLive"><Zap :size="14" :stroke-width="2" />Live</button>
          </div>
          <div class="seg" v-if="expandedClusters.size > 0">
            <button @click="collapseAllClusters"><Layers :size="14" :stroke-width="2" />Collapse</button>
          </div>
        </div>

        <!-- Highlight filter cluster (bottom-left) -->
        <div class="float-bl">
          <span class="hl-label">Highlight</span>
          <button class="hl-chip" data-k="errors" :class="{ 'is-on': overlays.has('errors') }" @click="toggleOverlay('errors')"><AlertTriangle :size="14" :stroke-width="2" />Errors</button>
          <button class="hl-chip" data-k="limits" :class="{ 'is-on': overlays.has('limits') }" @click="toggleOverlay('limits')"><Gauge :size="14" :stroke-width="2" />Limits</button>
          <button class="hl-chip" data-k="auth" :class="{ 'is-on': overlays.has('auth') }" @click="toggleOverlay('auth')"><ShieldCheck :size="14" :stroke-width="2" />Auth</button>
        </div>

        <!-- Activity pill (top-right) -->
        <div class="activity">
          <span class="live"><span class="live-dot"></span>Live</span>
          <span class="meta"><b>{{ liveActivity.rps }}</b>/s · <b>{{ liveActivity.errs }}</b> errors</span>
        </div>

        <PacketOverlay ref="packetOverlay" />
        <div v-if="!nodes.length" class="empty">No services registered yet.</div>
      </div>

      <Inspector
        :target="inspectorTarget"
        :totals="totals"
        :app-name="brandName"
        :global-middleware="globalMiddleware"
        :graphql-cache="graphqlCache"
        :auth-summary="authSummary"
        @open-op="onInspectorOp"
        @open-errors="onInspectorErrors"
        @cron="onCronAction"
        @pick="onInspectorPick"
        @close="selectedNodeId = null"
      />
    </div>

    <TweaksPanel
      v-if="tweaksOpen"
      :theme="theme"
      :accent="accent"
      :density="density"
      :mode="mode"
      :lane-labels="laneLabels"
      :live="live"
      :client-icon="clientIcon"
      @set-theme="setTheme"
      @set-accent="setAccent"
      @set-density="setDensity"
      @set-mode="setMode"
      @set-lane="setLane"
      @set-live="setLive"
      @set-client="setClient"
      @close="tweaksOpen = false"
    />

    <!-- Teleported overlays: deep-dive drawer, error dialog, Cmd-K -->
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
.app { display: flex; flex-direction: column; height: 100%; background: var(--bg); font-family: var(--font-sans); }

/* ---- header ---- */
.hdr {
  display: flex; align-items: center; gap: 18px;
  height: 56px; padding: 0 16px 0 18px;
  background: var(--surface); border-bottom: 1px solid var(--line);
  position: relative; z-index: 30; flex-shrink: 0;
}
.brand { display: flex; align-items: center; gap: 11px; }
.brand-mark {
  width: 30px; height: 30px; border-radius: 8px;
  background: linear-gradient(150deg, var(--accent), var(--accent-2));
  display: grid; place-items: center; color: var(--accent-ink);
  box-shadow: 0 3px 14px var(--accent-glow), inset 0 1px 0 rgba(255, 255, 255, .3);
}
.brand-name { font-weight: 600; font-size: 14.5px; letter-spacing: -.01em; color: var(--ink); }
.brand-sub { font-family: var(--font-mono); font-size: 10.5px; color: var(--ink-3); margin-top: 1px; letter-spacing: .02em; }

.tabs { display: flex; align-items: center; gap: 2px; margin-left: 6px; }
.tab {
  font-size: 13px; color: var(--ink-2); font-weight: 480;
  padding: 7px 12px; border-radius: var(--r-sm); cursor: pointer;
  border: none; background: none; font-family: inherit; position: relative;
}
.tab.is-active { color: var(--ink); background: var(--surface-3); font-weight: 560; }
.tab.is-active::after {
  content: ""; position: absolute; left: 12px; right: 12px; bottom: -1px; height: 2px;
  background: var(--accent); border-radius: 2px; box-shadow: 0 0 8px var(--accent-glow);
}
.hdr-spacer { flex: 1; }
.hdr-tools { display: flex; align-items: center; gap: 8px; }
.icon-btn {
  height: 34px; min-width: 34px; padding: 0 9px; border-radius: var(--r-sm);
  border: 1px solid var(--glass-line); background: var(--glass);
  -webkit-backdrop-filter: blur(18px) saturate(1.5); backdrop-filter: blur(18px) saturate(1.5);
  color: var(--ink-2); display: inline-flex; align-items: center; gap: 7px;
  cursor: pointer; font-family: inherit; font-size: 12.5px; font-weight: 500;
  transition: all var(--speed) var(--ease);
}
.icon-btn:hover { border-color: var(--accent-line); color: var(--ink); }
.icon-btn .kbd {
  font-family: var(--font-mono); font-size: 10.5px; color: var(--ink-3);
  border: 1px solid var(--line); border-radius: 5px; padding: 1px 5px; background: var(--surface-2);
}
.icon-btn.is-on { color: var(--accent); border-color: var(--accent-line); background: var(--accent-soft); }

/* ---- body / canvas ---- */
.body { flex: 1; display: flex; min-height: 0; position: relative; }
.canvas-wrap {
  flex: 1; min-width: 0; position: relative;
  background:
    radial-gradient(1200px 600px at 56% 13%, var(--canvas-glow), transparent 70%),
    var(--bg);
}

/* floating toolbar (top-left) */
.float-tl { position: absolute; top: 14px; left: 14px; z-index: 12; display: flex; gap: 10px; align-items: flex-start; }
.seg {
  display: inline-flex; border-radius: var(--r-sm); padding: 3px; gap: 2px;
  background: var(--glass); border: 1px solid var(--glass-line); box-shadow: var(--shadow-card);
  -webkit-backdrop-filter: blur(18px) saturate(1.5); backdrop-filter: blur(18px) saturate(1.5);
}
.seg button {
  font-family: inherit; font-size: 12.5px; font-weight: 520; color: var(--ink-2);
  border: none; background: none; padding: 6px 12px; border-radius: 6px; cursor: pointer;
  display: inline-flex; align-items: center; gap: 7px; transition: all var(--speed) var(--ease);
}
.seg button:hover { color: var(--ink); }
.seg button.is-active { background: var(--surface-3); color: var(--ink); box-shadow: var(--glow); }

/* highlight cluster (bottom-left) */
.float-bl {
  position: absolute; bottom: 14px; left: 14px; z-index: 12;
  display: flex; align-items: center; gap: 4px;
  border-radius: var(--r-sm); padding: 5px 6px 5px 12px;
  background: var(--glass); border: 1px solid var(--glass-line); box-shadow: var(--shadow-card);
  -webkit-backdrop-filter: blur(18px) saturate(1.5); backdrop-filter: blur(18px) saturate(1.5);
}
.float-bl .hl-label {
  font-size: 10.5px; font-weight: 650; letter-spacing: .09em; color: var(--ink-3);
  text-transform: uppercase; margin-right: 6px;
}
.hl-chip {
  font-family: inherit; font-size: 12.5px; font-weight: 520; color: var(--ink-2);
  border: 1px solid transparent; background: none; padding: 5px 11px; border-radius: 7px;
  cursor: pointer; display: inline-flex; align-items: center; gap: 6px; transition: all var(--speed) var(--ease);
}
.hl-chip:hover { background: var(--surface-3); color: var(--ink); }
.hl-chip[data-k="errors"].is-on { color: var(--err); background: var(--err-soft); border-color: var(--err); }
.hl-chip[data-k="limits"].is-on { color: var(--warn); background: var(--warn-soft); border-color: var(--warn); }
.hl-chip[data-k="auth"].is-on { color: var(--authc); background: var(--auth-soft); border-color: var(--authc); }

/* activity pill (top-right) */
.activity {
  position: absolute; top: 14px; right: 14px; z-index: 12;
  display: flex; align-items: center; gap: 12px;
  border-radius: var(--r-sm); padding: 8px 13px;
  background: var(--glass); border: 1px solid var(--glass-line); box-shadow: var(--shadow-card);
  -webkit-backdrop-filter: blur(18px) saturate(1.5); backdrop-filter: blur(18px) saturate(1.5);
}
.activity .live { display: inline-flex; align-items: center; gap: 6px; font-size: 12px; font-weight: 560; color: var(--accent); }
.live-dot {
  width: 7px; height: 7px; border-radius: 50%; background: var(--accent);
  box-shadow: 0 0 10px 1px var(--accent-glow); animation: pulse 1.8s infinite;
}
@keyframes pulse {
  0% { box-shadow: 0 0 0 0 var(--accent-soft); }
  70% { box-shadow: 0 0 0 7px transparent; }
  100% { box-shadow: 0 0 0 0 transparent; }
}
.activity .meta { font-family: var(--font-mono); font-size: 11.5px; color: var(--ink-2); }
.activity .meta b { color: var(--ink); font-weight: 600; }

.empty {
  position: absolute; inset: 0; display: grid; place-items: center;
  color: var(--ink-3); pointer-events: none; font-size: 13px;
}

/* lane labels honour the dim state when a highlight overlay is active */
.canvas-wrap.has-hl :deep(.vue-flow__edge.hl-edge .vue-flow__edge-path) { filter: drop-shadow(0 0 5px var(--accent-glow)); }

/* edge polish */
:deep(.vue-flow__edge .vue-flow__edge-path) {
  transition: stroke 160ms ease, opacity 160ms ease, stroke-width 160ms ease;
}
:deep(.vue-flow__edge:hover .vue-flow__edge-path) { opacity: 1 !important; }
:deep(.vue-flow__edge:hover) { cursor: pointer; }
</style>
