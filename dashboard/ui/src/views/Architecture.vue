<script setup>
import { ref, onMounted, onUnmounted, markRaw, nextTick } from 'vue'
import { VueFlow, useVueFlow, Position, MarkerType } from '@vue-flow/core'
import { Background } from '@vue-flow/background'
import { Controls } from '@vue-flow/controls'
import dagre from 'dagre'

import ServiceNode from '../components/ServiceNode.vue'
import ResourceNode from '../components/ResourceNode.vue'
import { fetchEndpoints, fetchResources } from '../lib/api.js'

const nodes = ref([])
const edges = ref([])
const nodeTypes = {
  service: markRaw(ServiceNode),
  resource: markRaw(ResourceNode)
}

const { fitView, onNodesInitialized } = useVueFlow()
onNodesInitialized(() => fitView({ padding: 0.2, maxZoom: 1 }))

function estimateServiceHeight(data) {
  const eps = Math.min(data.endpoints?.length || 0, 6)
  const hidden = (data.endpoints?.length || 0) > 6 ? 1 : 0
  const desc = data.description ? 32 : 0
  return 54 + desc + (eps + hidden) * 22
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
  g.setGraph({ rankdir: 'LR', nodesep: 50, ranksep: 140 })
  ns.forEach(n => {
    const w = n.type === 'resource' ? NODE_WIDTH_RESOURCE : NODE_WIDTH_SERVICE
    const h = n.type === 'resource' ? estimateResourceHeight(n.data) : estimateServiceHeight(n.data)
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

function gridLayout(ns) {
  const cols = Math.min(ns.length, 3)
  const rowHeights = []
  ns.forEach((n, i) => {
    const row = Math.floor(i / cols)
    const h = n.type === 'resource' ? estimateResourceHeight(n.data) : estimateServiceHeight(n.data)
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
  const [epData, rsData] = await Promise.all([fetchEndpoints(), fetchResources()])
  const grouped = {}
  for (const e of epData.endpoints || []) {
    (grouped[e.Service] ||= []).push(e)
  }
  const svcNodes = (epData.services || []).map(s => ({
    id: `svc:${s.Name}`,
    type: 'service',
    position: { x: 0, y: 0 },
    data: {
      name: s.Name,
      description: s.Description,
      endpoints: grouped[s.Name] || []
    }
  }))
  const rsNodes = (rsData.resources || []).map(r => ({
    id: `res:${r.name}`,
    type: 'resource',
    position: { x: 0, y: 0 },
    data: r
  }))

  const edgeList = []
  for (const r of rsData.resources || []) {
    for (const svc of r.attachedTo || []) {
      edgeList.push({
        id: `e:${svc}->${r.name}`,
        source: `svc:${svc}`,
        target: `res:${r.name}`,
        markerEnd: MarkerType.ArrowClosed,
        style: { stroke: 'var(--border-strong)', strokeWidth: 1.5 }
      })
    }
  }

  const all = [...svcNodes, ...rsNodes]
  nodes.value = layout(all, edgeList)
  edges.value = edgeList
  nextTick(() => fitView({ padding: 0.2, maxZoom: 1 }))
}

let pollTimer = null
onMounted(() => {
  load()
  pollTimer = setInterval(load, 5000) // refresh health
})
onUnmounted(() => pollTimer && clearInterval(pollTimer))
</script>

<template>
  <div class="arch">
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
    <div v-if="!nodes.length" class="empty">
      No services registered yet.
    </div>
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
