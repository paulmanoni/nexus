<script setup>
import { computed } from 'vue'
import { Handle, Position } from '@vue-flow/core'
import { Cog, Database, Link2, AlertTriangle } from 'lucide-vue-next'

// WorkerNode renders a nexus.AsWorker entry on the architecture graph.
// Workers are long-lived background tasks (DB listeners, queue
// consumers, sweepers) — structurally similar to a service in that
// they depend on resources / other services, but they don't handle
// HTTP traffic, so they don't need the row-by-row endpoint layout
// ServiceNode uses. A single card per worker with a dep list is
// enough to tell the "what does this worker depend on" story.
const props = defineProps(['data'])

const statusClass = computed(() => props.data.status || 'unknown')
const hasDeps = computed(() => {
  const r = props.data.resourceDeps || []
  const s = props.data.serviceDeps || []
  return r.length + s.length > 0
})
</script>

<template>
  <div class="worker-node" :class="statusClass">
    <Handle type="target" :position="Position.Left" />
    <div class="head">
      <Cog :size="13" :stroke-width="2" class="icon" />
      <span class="name">{{ data.name }}</span>
      <span class="tag">worker</span>
    </div>
    <div class="row">
      <span class="status-dot" :class="statusClass" :title="data.status"></span>
      <span class="status-text">{{ data.status || 'unknown' }}</span>
    </div>
    <div v-if="data.lastError" class="err" :title="data.lastError">
      <AlertTriangle :size="10" :stroke-width="2" />
      <span class="err-msg">{{ data.lastError }}</span>
    </div>
    <div v-if="hasDeps" class="deps">
      <div v-for="r in data.resourceDeps || []" :key="'r:' + r" class="dep">
        <Database :size="10" :stroke-width="2" class="dep-ico" />
        <span class="dep-name">{{ r }}</span>
      </div>
      <div v-for="s in data.serviceDeps || []" :key="'s:' + s" class="dep svc">
        <Link2 :size="10" :stroke-width="2" class="dep-ico" />
        <span class="dep-name">{{ s }}</span>
      </div>
    </div>
    <Handle type="source" :position="Position.Right" />
  </div>
</template>

<style scoped>
.worker-node {
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 10px 12px;
  min-width: 200px;
  max-width: 240px;
  color: var(--text);
  box-shadow: var(--shadow-sm);
  font-family: var(--font-sans);
}
.worker-node.failed  { border-color: var(--error); }
.worker-node.stopped { opacity: 0.65; }

.head {
  display: flex;
  align-items: center;
  gap: 7px;
  font-family: var(--font-mono);
  font-size: 12px;
  font-weight: 600;
}
.icon { color: #059669; flex-shrink: 0; }
.name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.tag {
  font-family: var(--font-mono);
  font-size: 10px;
  font-weight: 700;
  padding: 1px 7px;
  border-radius: 10px;
  background: #d1fae5;
  color: #065f46;
  text-transform: lowercase;
  letter-spacing: 0.02em;
  flex-shrink: 0;
}
.row {
  margin-top: 6px;
  display: flex;
  align-items: center;
  gap: 6px;
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--text-muted);
}
.status-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--text-dim);
}
.status-dot.running { background: var(--success); box-shadow: 0 0 0 3px var(--success-soft); }
.status-dot.failed  { background: var(--error); }
.status-dot.stopped { background: var(--text-dim); }
.status-dot.starting { background: var(--accent); }
.status-text { text-transform: capitalize; }

.err {
  margin-top: 6px;
  display: inline-flex;
  gap: 5px;
  align-items: center;
  font-family: var(--font-mono);
  font-size: 10px;
  color: var(--error);
  overflow: hidden;
}
.err-msg { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

.deps {
  margin-top: 8px;
  padding-top: 7px;
  border-top: 1px dashed var(--border);
  display: flex;
  flex-direction: column;
  gap: 3px;
}
.dep {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-family: var(--font-mono);
  font-size: 10.5px;
  color: var(--text-muted);
}
.dep-ico { color: var(--accent); flex-shrink: 0; }
.dep.svc .dep-ico { color: #7c3aed; }
.dep-name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>