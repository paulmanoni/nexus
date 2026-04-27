<script setup>
import { ref, onMounted, onUnmounted, markRaw, nextTick, provide, watch } from 'vue'
import { VueFlow, useVueFlow, Position, MarkerType } from '@vue-flow/core'
import { Background } from '@vue-flow/background'
import { Controls } from '@vue-flow/controls'
import dagre from 'dagre'

import ServiceNode from '../components/ServiceNode.vue'
import ServiceDepNode from '../components/ServiceDepNode.vue'
import WorkerNode from '../components/WorkerNode.vue'
import ResourceNode from '../components/ResourceNode.vue'
import InternetNode from '../components/InternetNode.vue'
import BoundaryNode from '../components/BoundaryNode.vue'
import ErrorDialog from '../components/ErrorDialog.vue'
import PacketOverlay from '../components/PacketOverlay.vue'
import GlobalMiddlewareBar from '../components/GlobalMiddlewareBar.vue'
import { fetchEndpoints, fetchResources, fetchStats, fetchWorkers, subscribeEvents } from '../lib/api.js'
import { usePoll } from '../lib/usePoll.js'

const nodes = ref([])
const edges = ref([])
// Node types: "service" now renders the module/group card (name retained
// for back-compat with packet animator CSS selectors); "serviceDep" is
// the small dep-node on the right for services consumed by endpoints.
const nodeTypes = {
  service: markRaw(ServiceNode),
  serviceDep: markRaw(ServiceDepNode),
  worker: markRaw(WorkerNode),
  resource: markRaw(ResourceNode),
  internet: markRaw(InternetNode),
  boundary: markRaw(BoundaryNode),
}

// INTERNET_ID is the fixed id of the single "Clients" node. Keep it a
// constant so edge-builders and the traffic animator agree on naming.
const INTERNET_ID = 'internet'

// Per-op selection store. ServiceNode writes here on click; ResourceNode
// + edge-styling read from it. Single source of truth means no props need
// to thread through the VueFlow custom-node API.
const opSelection = ref(null)  // { service, op, resources: string[] }
function setOp(sel) { opSelection.value = sel }
function clearOp() { opSelection.value = null }
provide('nexus.opSelection', opSelection)
provide('nexus.setOp', setOp)
provide('nexus.clearOp', clearOp)

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

const { fitView, onNodesInitialized, onPaneClick } = useVueFlow()
onNodesInitialized(() => fitView({ padding: 0.2, maxZoom: 1 }))
// Click the empty canvas → clear op selection. Lets users reset without
// having to find the card header.
onPaneClick(() => clearOp())

function estimateServiceHeight(data) {
  // Every endpoint renders — no MAX_VISIBLE cap. The chip row's
  // visual height grows with chip count (chips wrap to 2-3 lines
  // when many resources / middlewares pile on); approximate by
  // bucketing into 0/1/2 extra rows, which keeps cards from
  // overlapping in dagre's layout without measuring post-render.
  const eps = data.endpoints || []
  const desc = data.description ? 32 : 0
  let rows = 0
  for (const e of eps) {
    const hasOwnerChip = !e.ServiceAutoRouted
    const resCount = Array.isArray(e.Resources) ? e.Resources.length : 0
    const mwCount = Array.isArray(e.Middleware)
      ? e.Middleware.filter(m => m !== 'metrics').length
      : 0
    const chipCount = (hasOwnerChip ? 1 : 0) + resCount + mwCount
    if (chipCount === 0) {
      rows += 1            // single op line
    } else if (chipCount <= 3) {
      rows += 2            // op line + one chip line
    } else if (chipCount <= 6) {
      rows += 3            // op line + two chip lines
    } else {
      rows += 4            // op line + three chip lines
    }
  }
  // Header (38) + description + rows × 22 + bottom padding (16) +
  // small safety margin (12) so cards never butt against each
  // other under variable chip wrapping.
  return 38 + desc + rows*22 + 16 + 12
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
  // ranksep the horizontal gap between ranks. Bumped from
  // (50, 140) so module cards with many endpoints — which can be
  // ~600-800px tall in real projects — still have visible air
  // between them.
  g.setGraph({ rankdir: 'LR', nodesep: 80, ranksep: 160 })
  ns.forEach(n => {
    let w, h
    if (n.type === 'internet') { w = 160; h = 90 }
    else if (n.type === 'resource') { w = NODE_WIDTH_RESOURCE; h = estimateResourceHeight(n.data) }
    else if (n.type === 'serviceDep') { w = NODE_WIDTH_RESOURCE; h = estimateServiceDepHeight(n.data) }
    else if (n.type === 'worker') { w = NODE_WIDTH_RESOURCE; h = estimateWorkerHeight(n.data) }
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

function gridLayout(ns) {
  const cols = Math.min(ns.length, 3)
  const rowHeights = []
  ns.forEach((n, i) => {
    const row = Math.floor(i / cols)
    let h
    if (n.type === 'resource') h = estimateResourceHeight(n.data)
    else if (n.type === 'serviceDep') h = estimateServiceDepHeight(n.data)
    else if (n.type === 'worker') h = estimateWorkerHeight(n.data)
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

async function load() {
  let epData, rsData, statsData, wkData
  try {
    [epData, rsData, statsData, wkData] = await Promise.all([
      fetchEndpoints(),
      fetchResources(),
      fetchStats().catch(() => ({ stats: [] })), // graceful if stats endpoint absent
      fetchWorkers().catch(() => ({ workers: [] })),
    ])
  } catch (err) {
    console.error('[nexus] Architecture load failed:', err)
    return
  }
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
  // Grouping: by MODULE (the nexus.Module("name", ...) wrapper), with
  // the owning service name as a fallback for endpoints registered
  // outside any module. This is the core of the architecture-view
  // shift — modules own endpoints; services become dep nodes.
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
        // multiple services land in one module (rare), this stays ''
        // so the owner chip resolves row-by-row via e.Service
        // (handled by a per-row fallback in ServiceNode).
        service: e.Service,
        endpoints: [],
        description: serviceIndex[e.Service]?.Description || '',
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

  const groupNodes = [...groups.values()].map(g => ({
    id: g.key,
    type: 'service',
    position: { x: 0, y: 0 },
    data: {
      groupKey: g.key,
      name: g.name,
      isModule: g.isModule,
      service: g.service,
      description: g.description,
      endpoints: g.endpoints,
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

  const depServices = new Map() // name -> { Name, Description, ResourceDeps, ServiceDeps }
  const markDep = (name) => {
    if (!name) return
    // Skip names already represented by a module card. The card
    // carries the service identity (header shows the name); a
    // separate dep node would just duplicate it on the canvas.
    if (moduleCardsByName[name]) return
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
  // when pointing at "service X". Module cards win over plain dep
  // nodes; falls back to dep:X when no card claims the name.
  const resolveDepTarget = (name) => moduleCardsByName[name] || `dep:${name}`
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
  const workerNodes = (wkData.workers || []).map(w => ({
    id: `wk:${w.Name}`,
    type: 'worker',
    position: { x: 0, y: 0 },
    data: {
      name: w.Name,
      status: w.Status || 'unknown',
      lastError: w.LastError || '',
      resourceDeps: w.ResourceDeps || [],
      serviceDeps: w.ServiceDeps || [],
    },
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
  // Map each service name → the GROUP key that owns endpoints for that
  // service. Used by the aggregated fallback and the Internet inbound
  // lanes so edges land on the right module card even when grouping
  // differs from the service name.
  const serviceToGroup = {}
  for (const g of groups.values()) {
    for (const e of g.endpoints) {
      serviceToGroup[e.Service] = g.key
    }
  }
  // Build a per-endpoint lookup so we can locate the owning group by
  // (service, op) — needed to source edges from the right group card.
  const endpointGroup = {}
  for (const g of groups.values()) {
    for (const e of g.endpoints) {
      endpointGroup[`${e.Service}.${e.Name || `${e.Method} ${e.Path}`}`] = g.key
    }
  }
  for (const e of epData.endpoints || []) {
    const opName = e.Name || `${e.Method} ${e.Path}`
    const groupKey = endpointGroup[`${e.Service}.${opName}`]
    if (!groupKey) continue
    // Resource edges.
    for (const rName of e.Resources || []) {
      edgeList.push({
        id: `e:${groupKey}.${opName}->res:${rName}`,
        source: groupKey,
        sourceHandle: `op:${opName}`,
        target: `res:${rName}`,
        markerEnd: MarkerType.ArrowClosed,
        data: { service: e.Service, target: rName, targetKind: 'resource', op: opName },
      })
      claimed.add(`${e.Service}|res:${rName}`)
    }
    // Owning-service dep edge — ONLY when the handler explicitly took
    // *Service as a Go dep. Auto-routed endpoints skip this because
    // they don't actually depend on the service wrapper value; they
    // were adopted into the service for schema/metrics routing only.
    if (!e.ServiceAutoRouted && (depServices.has(e.Service) || moduleCardsByName[e.Service])) {
      const tgt = resolveDepTarget(e.Service)
      edgeList.push({
        id: `e:${groupKey}.${opName}->${tgt}`,
        source: groupKey,
        sourceHandle: `op:${opName}`,
        target: tgt,
        markerEnd: MarkerType.ArrowClosed,
        data: { service: e.Service, target: e.Service, targetKind: 'service', op: opName, owning: true },
      })
    }
    // Other-service dep edges. resolveDepTarget routes to the module
    // card when one exists (cross-module dep — the dep IS another
    // module on the canvas) and falls back to the small dep node
    // otherwise (purely service-level dep).
    for (const sName of e.ServiceDeps || []) {
      const tgt = resolveDepTarget(sName)
      edgeList.push({
        id: `e:${groupKey}.${opName}->${tgt}`,
        source: groupKey,
        sourceHandle: `op:${opName}`,
        target: tgt,
        markerEnd: MarkerType.ArrowClosed,
        data: { service: e.Service, target: sName, targetKind: 'service', op: opName },
      })
    }
  }
  // Aggregated fallback for runtime-attached resources no op claims.
  for (const r of rsData.resources || []) {
    for (const svc of r.attachedTo || []) {
      if (claimed.has(`${svc}|res:${r.name}`)) continue
      const groupKey = serviceToGroup[svc]
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
    for (const res of w.ResourceDeps || []) {
      edgeList.push({
        id: `e:wk:${w.Name}->res:${res}`,
        source: `wk:${w.Name}`,
        target: `res:${res}`,
        markerEnd: MarkerType.ArrowClosed,
        data: { service: w.Name, target: res, targetKind: 'resource', op: null, serviceLevel: true, worker: true },
      })
    }
    for (const other of w.ServiceDeps || []) {
      if (!depServices.has(other) && !moduleCardsByName[other]) continue
      const tgt = resolveDepTarget(other)
      edgeList.push({
        id: `e:wk:${w.Name}->${tgt}`,
        source: `wk:${w.Name}`,
        target: tgt,
        markerEnd: MarkerType.ArrowClosed,
        data: { service: w.Name, target: other, targetKind: 'service', op: null, serviceLevel: true, worker: true },
      })
    }
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
    const sourceID = moduleCardsByName[s.Name] || (depServices.has(s.Name) ? `dep:${s.Name}` : '')
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
      if (!depServices.has(other) && !moduleCardsByName[other]) continue
      const tgt = resolveDepTarget(other)
      edgeList.push({
        id: `e:${sourceID}->${tgt}`,
        source: sourceID,
        target: tgt,
        markerEnd: MarkerType.ArrowClosed,
        data: { service: s.Name, target: other, targetKind: 'service', op: null, serviceLevel: true },
      })
    }
  }
  // Internet → group edges. One per module-group, since modules are
  // the thing external traffic now "enters" in the visual model.
  for (const g of groups.values()) {
    edgeList.push({
      id: `e:internet->${g.key}`,
      source: INTERNET_ID,
      target: g.key,
      markerEnd: MarkerType.ArrowClosed,
      data: { service: g.service, target: g.name, targetKind: 'module', op: null, inbound: true, groupKey: g.key },
    })
  }

  const all = [internetNode, ...groupNodes, ...svcDepNodes, ...workerNodes, ...rsNodes]
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
  // Build the system boundary AFTER layout so we can size it to the
  // bounding box of all non-Internet nodes. Padding leaves a soft
  // margin between the border and the outermost cards.
  try {
    const boundary = buildBoundaryNode(laid)
    // Render boundary first so VueFlow paints it beneath real nodes.
    // Explicit zIndex keeps it safely behind even under future refactors.
    nodes.value = boundary ? [{ ...boundary, zIndex: -1 }, ...laid] : laid
    rawEdges.value = edgeList
    indexEndpointEdges(edgeList)
    indexEndpointGroups(groupNodes)
    edges.value = restyleEdges(edgeList, opSelection.value, flashedEdges.value)
    nextTick(() => fitView({ padding: 0.2, maxZoom: 1 }))
  } catch (err) {
    console.error('[nexus] Architecture render failed:', err, { groupCount: groupNodes.length, edgeCount: edgeList.length })
  }
}

function buildBoundaryNode(laid) {
  const BBOX_PAD = 28
  let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity
  let hits = 0
  for (const n of laid) {
    // Enclose only the "inside-the-system" nodes — module groups,
    // service-deps, resources. Internet is an outside caller, so
    // the boundary shouldn't ring around it.
    if (n.type !== 'service' && n.type !== 'resource' && n.type !== 'serviceDep' && n.type !== 'worker') continue
    const w = (n.type === 'resource' || n.type === 'serviceDep' || n.type === 'worker') ? NODE_WIDTH_RESOURCE : NODE_WIDTH_SERVICE
    const h = n.type === 'resource' ? estimateResourceHeight(n.data)
           : n.type === 'serviceDep' ? estimateServiceDepHeight(n.data)
           : n.type === 'worker' ? estimateWorkerHeight(n.data)
           : estimateServiceHeight(n.data)
    minX = Math.min(minX, n.position.x)
    minY = Math.min(minY, n.position.y)
    maxX = Math.max(maxX, n.position.x + w)
    maxY = Math.max(maxY, n.position.y + h)
    hits++
  }
  if (hits === 0) return null
  const x = minX - BBOX_PAD
  const y = minY - BBOX_PAD
  const width  = (maxX - minX) + BBOX_PAD * 2
  const height = (maxY - minY) + BBOX_PAD * 2
  return {
    id: 'boundary',
    type: 'boundary',
    position: { x, y },
    data: { width, height },
    // Boundary is purely decorative — no handles, no interactivity.
    selectable: false,
    draggable: false,
  }
}

// selectedMatches reports whether edge `e` belongs to the currently-
// selected op on the currently-selected module group. Edge ids are
// formatted `e:<groupKey>.<op>-><target>` so we compare against the
// selection's groupKey + op to decide whether to highlight.
function selectedMatches(sel, e) {
  if (!sel) return false
  if (sel.op !== e.data.op) return false
  // Source is the group that owns this edge — it's encoded as the
  // edge's source field (e.g. "mod:adverts"). Match against the
  // selection's groupKey for a direct hit.
  return e.source === sel.groupKey
}

// restyleEdges returns a fresh edges array with styling applied based on
// the current op selection + live-traffic flash state. Baseline shows
// every per-op line softly; selecting an op highlights that op's lines
// and dims the rest; a flashed edge id temporarily overrides both.
function restyleEdges(list, sel, flashed) {
  return list.map(e => {
    const base = { ...e }
    const isAggregated = e.data.op === null
    const isInbound = !!e.data.inbound

    // Flashed edges (live traffic) — brightest, always animated. Color
    // reflects request outcome: accent on success, red when the request
    // was stopped (rate-limited, auth-failed, validation-failed, etc.).
    if (flashed && flashed.has(e.id)) {
      const state = flashed.get(e.id)
      const stroke = state === 'error' ? 'var(--error)' : 'var(--accent)'
      base.style = { stroke, strokeWidth: 2.6, opacity: 1 }
      base.animated = true
      return base
    }
    if (!sel) {
      if (isInbound) {
        // Inbound lanes sit gray by default — they only come alive
        // when traffic actually flows, via the flashed branch above.
        base.style = { stroke: 'var(--border-strong)', strokeWidth: 1.5, opacity: 0.8 }
      } else if (isAggregated) {
        base.style = { stroke: 'var(--border-strong)', strokeWidth: 1.5, opacity: 1 }
      } else {
        base.style = { stroke: 'var(--accent)', strokeWidth: 1.4, opacity: 0.55 }
      }
      base.animated = false
    } else if (selectedMatches(sel, e)) {
      base.style = { stroke: 'var(--accent)', strokeWidth: 2.4, opacity: 1 }
      base.animated = true
    } else {
      base.style = (isAggregated || isInbound)
        ? { stroke: 'var(--border-strong)', strokeWidth: 1.5, opacity: 0.12 }
        : { stroke: 'var(--accent)', strokeWidth: 1.4, opacity: 0.12 }
      base.animated = false
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
    if (!e.data.op || e.data.inbound) continue
    const k = `${e.data.service}.${e.data.op}`
    const arr = endpointEdgeIdx.get(k) || []
    arr.push(e.id)
    endpointEdgeIdx.set(k, arr)
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

function onTraceEvent(ev) {
  // request.op carries the specific op name in Endpoint (emitted by the
  // metrics middleware per handler exit). request.start from the
  // framework trace layer only carries the HTTP path — too coarse to
  // identify a GraphQL operation — so we drive the per-op UI off
  // request.op exclusively. Result: packets land on the right row.
  if (ev.kind !== 'request.op') return
  if (!ev.service) return
  // Skip events we're replaying from the /events backlog on initial
  // subscribe — they're older than this mount so animating them would
  // misrepresent "live" state.
  if (ev.timestamp) {
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
  const inboundId = groupId ? `e:internet->${groupId}` : `e:internet->svc:${ev.service}`

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

// spawnPacketsForEdges reads the CURRENT screen positions of the
// involved source/target nodes and asks the overlay to shoot a dot
// from one to the other. For edges that target a specific op row
// (inbound Internet → service for the endpoint being hit, OR the
// per-op row's outbound edges), we aim at the row's exact y so the
// dot visibly lands on the endpoint the operator is interested in.
function spawnPacketsForEdges(ids, opName, state) {
  if (!packetOverlay.value) return
  const canvas = canvasEl.value
  if (!canvas) return
  const cr = canvas.getBoundingClientRect()
  const opts = { state: state === 'error' ? 'error' : 'ok' }
  ids.forEach((edgeId, i) => {
    const edge = rawEdges.value.find(e => e.id === edgeId)
    if (!edge) return
    const fromEl = canvas.querySelector(`.vue-flow__node[data-id="${CSS.escape(edge.source)}"]`)
    const toEl   = canvas.querySelector(`.vue-flow__node[data-id="${CSS.escape(edge.target)}"]`)
    if (!fromEl || !toEl) return
    const fr = fromEl.getBoundingClientRect()
    const tr = toEl.getBoundingClientRect()

    // Source y: if the source is a service card with a per-op handle,
    // aim from that row's right edge (matches where the line actually
    // leaves the card). Otherwise use the card vertical center.
    let fromY = fr.top + fr.height / 2
    if (edge.sourceHandle && edge.sourceHandle.startsWith('op:') && fromEl.matches('.vue-flow__node[data-type="service"]')) {
      const rowOp = edge.sourceHandle.slice(3)
      const row = fromEl.querySelector(`.row[data-op="${CSS.escape(rowOp)}"]`)
      if (row) fromY = row.getBoundingClientRect().top + row.getBoundingClientRect().height / 2
    }

    // Target y: inbound edges (Internet → service) aim at the row of
    // the endpoint the request actually hit so the packet lands ON
    // the endpoint, not just "in the card somewhere".
    let toY = tr.top + tr.height / 2
    if (edge.data?.inbound && opName && toEl.matches('.vue-flow__node[data-type="service"]')) {
      const row = toEl.querySelector(`.row[data-op="${CSS.escape(opName)}"]`)
      if (row) toY = row.getBoundingClientRect().top + row.getBoundingClientRect().height / 2
    }

    const from = { x: fr.right - cr.left, y: fromY - cr.top }
    const to   = { x: tr.left  - cr.left, y: toY   - cr.top }
    const stagger = i * 120 // ms; entry dot first, then downstream hops
    setTimeout(() => packetOverlay.value?.spawn(from, to, opts), stagger)
  })
}

const packetOverlay = ref(null)
const canvasEl = ref(null)

let traceSub = null
onMounted(() => {
  load()
  // Subscribe to the request trace stream so the graph lights up on
  // live traffic. Same socket the Traces tab uses; it multiplexes
  // fine — backlog replay is harmless because each flash has its own
  // short timeout.
  traceSub = subscribeEvents(onTraceEvent, null, 0)
})
usePoll(load, 5000) // refresh health
onUnmounted(() => {
  if (traceSub) traceSub.close()
  flashTimers.forEach(t => clearTimeout(t))
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
      <Background pattern-color="#d1d5db" :gap="22" :size="1.3" />
      <Controls :show-interactive="false" />
    </VueFlow>
    <GlobalMiddlewareBar />
    <PacketOverlay ref="packetOverlay" />
    <div v-if="!nodes.length" class="empty">
      No services registered yet.
    </div>
    <ErrorDialog
      :open="errorDialog.open"
      :service="errorDialog.service"
      :op="errorDialog.op"
      @close="closeErrors"
    />
  </div>
</template>

<style scoped>
.arch { width: 100%; height: 100%; position: relative; background: var(--canvas-bg); }
.empty {
  position: absolute;
  inset: 0;
  display: grid;
  place-items: center;
  color: var(--text-dim);
  pointer-events: none;
  font-size: 13px;
}
</style>
