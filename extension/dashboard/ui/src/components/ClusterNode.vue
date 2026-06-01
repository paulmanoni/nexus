<script setup>
import { computed, inject } from 'vue'
import { Handle, Position } from '@vue-flow/core'
import { ChevronRight, Activity, AlertTriangle } from 'lucide-vue-next'
import CategoryIcon from './CategoryIcon.vue'

// ClusterNode is the collapsed form of a drill-down group: one card standing
// in for many nodes (a deployment, or a tier like Services / Data). Clicking
// it expands the cluster — Architecture reruns layout to reveal the members.
// This is the default shape at scale: a 1000-node app opens as a handful of
// these instead of 1000 cards.
const props = defineProps(['data'])
const expand = inject('nexus.expandCluster', () => {})

// kind → CategoryIcon type. Deployments read as "peer" (server), tiers map
// to their dominant member category.
const iconType = computed(() => {
  const k = props.data.kind
  if (k === 'deployment') return 'peer'
  if (k === 'data') return 'database'
  if (k === 'workers') return 'worker'
  if (k === 'schedules') return 'cron'
  return 'module' // services tier / generic
})

const s = computed(() => props.data.summary || {})
// One-line member breakdown: only the non-zero parts, so a Services cluster
// doesn't advertise "0 crons".
const parts = computed(() => {
  const out = []
  const x = s.value
  if (x.modules)   out.push(`${x.modules} ${x.modules === 1 ? 'module' : 'modules'}`)
  if (x.services)  out.push(`${x.services} svc`)
  if (x.resources) out.push(`${x.resources} ${x.resources === 1 ? 'resource' : 'resources'}`)
  if (x.workers)   out.push(`${x.workers} ${x.workers === 1 ? 'worker' : 'workers'}`)
  if (x.crons)     out.push(`${x.crons} cron${x.crons === 1 ? '' : 's'}`)
  return out.join(' · ')
})
</script>

<template>
  <div class="cluster-node" :class="data.kind" @click.stop="expand(data.clusterKey)" title="Click to expand">
    <Handle type="target" :position="Position.Left" />
    <div class="head">
      <CategoryIcon :type="iconType" :size="32" />
      <div class="title">
        <div class="name-row">
          <span class="name">{{ data.label }}</span>
          <span class="count">{{ data.count }}</span>
        </div>
        <div class="kind">{{ data.kind }}</div>
      </div>
      <ChevronRight class="chev" :size="18" :stroke-width="2.2" />
    </div>
    <div class="meta">{{ parts }}</div>
    <div v-if="s.endpoints || s.requests || s.errors" class="stats">
      <span v-if="s.endpoints" class="pill">{{ s.endpoints }} ep</span>
      <span v-if="s.requests" class="pill req"><Activity :size="9" :stroke-width="2.2" /> {{ s.requests }}</span>
      <span v-if="s.errors" class="pill err"><AlertTriangle :size="9" :stroke-width="2.2" /> {{ s.errors }}</span>
    </div>
    <Handle type="source" :position="Position.Right" />
  </div>
</template>

<style scoped>
.cluster-node {
  background: var(--bg-card);
  border: 1px solid var(--border-strong);
  border-radius: var(--radius-md);
  /* Stacked-paper edge so a cluster reads as "many things, collapsed". */
  box-shadow: var(--shadow-md), 3px 3px 0 -1px var(--bg-card), 3px 3px 0 0 var(--border);
  min-width: 240px;
  color: var(--text);
  font-family: var(--font-sans);
  position: relative;
  cursor: pointer;
  padding: var(--space-3);
  transition: box-shadow 120ms, border-color 120ms, transform 120ms;
}
.cluster-node:hover {
  border-color: var(--accent);
  box-shadow: var(--shadow-lg), 3px 3px 0 -1px var(--bg-card), 3px 3px 0 0 var(--accent-soft);
}
.cluster-node:hover .chev { transform: translateX(2px); color: var(--accent); }
.head { display: flex; align-items: center; gap: var(--space-2); }
.title { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 2px; }
.name-row { display: flex; align-items: center; gap: var(--space-2); min-width: 0; }
.name {
  flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
  font-size: var(--fs-lg); font-weight: 600; letter-spacing: -0.005em;
}
.count {
  flex-shrink: 0; font-family: var(--font-mono); font-size: var(--fs-xs); font-weight: 600;
  background: var(--accent-soft); color: var(--accent);
  padding: 1px 7px; border-radius: 999px; line-height: 1.5;
}
.kind {
  font-family: var(--font-mono); font-size: var(--fs-xs); color: var(--text-dim);
  text-transform: lowercase; letter-spacing: 0.02em;
}
.chev { color: var(--text-dim); flex-shrink: 0; transition: transform 120ms, color 120ms; }
.meta {
  margin-top: var(--space-2); font-size: var(--fs-sm); color: var(--text-muted);
  white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
}
.stats { margin-top: var(--space-2); display: flex; gap: 4px; flex-wrap: wrap; }
.pill {
  display: inline-flex; align-items: center; gap: 3px;
  font-family: var(--font-mono); font-size: 10px; font-variant-numeric: tabular-nums;
  padding: 2px 7px; border-radius: 999px; background: var(--bg-hover); color: var(--text-muted);
}
.pill.req { color: var(--text-muted); }
.pill.err { background: var(--st-error-soft); color: var(--st-error); font-weight: 600; }
</style>
