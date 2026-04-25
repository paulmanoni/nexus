<script setup>
import { ref, onMounted, onUnmounted, computed } from 'vue'
import { Search, Trash2, CheckCircle2, XCircle, ArrowRight, ChevronDown } from 'lucide-vue-next'
import { subscribeEvents } from '../lib/api.js'
import { formatTime as fmtTime } from '../lib/time.js'
import TraceWaterfall from '../components/TraceWaterfall.vue'

const events = ref([])
const MAX = 500
const filter = ref('')
const kindFilter = ref('all')
const connected = ref(false)
let ws = null

onMounted(() => {
  ws = subscribeEvents(
    e => {
      events.value.push(e)
      if (events.value.length > MAX) events.value.splice(0, events.value.length - MAX)
    },
    status => { connected.value = status === 'open' }
  )
})
onUnmounted(() => ws && ws.close())

const filtered = computed(() => {
  const f = filter.value.toLowerCase()
  return events.value
    .filter(e => {
      if (kindFilter.value !== 'all' && !e.kind.startsWith(kindFilter.value)) return false
      if (!f) return true
      return JSON.stringify(e).toLowerCase().includes(f)
    })
    .slice()
    .reverse()
})

function clear() { events.value = [] }

function shortKind(k) {
  return k.replace('request.', '').replace('.', ' ')
}

// Waterfall dialog: click a #traceId chip to open the span tree for
// that trace. Truncating the displayed id keeps the list readable while
// the full id rides along on the click handler for the fetch.
const selectedTraceId = ref(null)
function openTrace(id, ev) {
  if (ev) { ev.stopPropagation() }
  if (!id) return
  selectedTraceId.value = id
}
function shortTrace(id) {
  if (!id) return ''
  return id.length > 8 ? id.slice(0, 8) : id
}
</script>

<template>
  <div class="traces">
    <header>
      <span class="conn" :class="{ online: connected }" :title="connected ? 'Live' : 'Reconnecting…'">
        <span class="dot"></span>
        <span>{{ connected ? 'Live' : 'Reconnecting' }}</span>
      </span>
      <div class="search">
        <Search :size="14" :stroke-width="2" class="search-icon" />
        <input v-model="filter" placeholder="Filter by path, message, error…" />
      </div>
      <div class="select-wrap">
        <select v-model="kindFilter">
          <option value="all">All kinds</option>
          <option value="request">Requests</option>
          <option value="downstream">Downstream</option>
          <option value="log">Logs</option>
        </select>
        <ChevronDown :size="14" :stroke-width="2" class="select-chev" />
      </div>
      <button @click="clear">
        <Trash2 :size="14" :stroke-width="2" />
        <span>Clear</span>
      </button>
      <span class="counter">{{ events.length }} / {{ MAX }}</span>
    </header>
    <div class="list">
      <div
        v-for="e in filtered"
        :key="e.id"
        class="row"
        :class="[e.kind.replace('.', '-'), { err: e.error }]"
      >
        <span class="ts">{{ fmtTime(e.timestamp) }}</span>
        <button class="trace" :title="e.traceId" @click="openTrace(e.traceId, $event)">#{{ shortTrace(e.traceId) }}</button>
        <span class="kind">{{ shortKind(e.kind) }}</span>
        <span v-if="e.method" class="method">{{ e.method }}</span>
        <span v-if="e.path" class="path">{{ e.path }}</span>
        <span v-if="e.message" class="msg"><ArrowRight :size="11" :stroke-width="2" />{{ e.message }}</span>
        <span v-if="e.status" class="status" :class="e.status < 400 ? 'ok' : 'fail'">
          <component :is="e.status < 400 ? CheckCircle2 : XCircle" :size="11" :stroke-width="2" />
          {{ e.status }}
        </span>
        <span v-if="e.durationMs !== undefined && e.durationMs > 0" class="dur">{{ e.durationMs }}ms</span>
        <span v-if="e.error" class="err-text">{{ e.error }}</span>
      </div>
      <div v-if="!filtered.length" class="empty">No events yet. Trigger a request to see traces here.</div>
    </div>
    <TraceWaterfall
      :open="!!selectedTraceId"
      :trace-id="selectedTraceId"
      @close="selectedTraceId = null"
    />
  </div>
</template>

<style scoped>
.traces { display: flex; flex-direction: column; height: 100%; background: var(--bg); }
header {
  display: flex;
  gap: 10px;
  align-items: center;
  padding: 12px 16px;
  border-bottom: 1px solid var(--border);
  flex-shrink: 0;
  background: var(--bg);
}
.conn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 11px;
  font-weight: 600;
  color: var(--text-dim);
  padding: 4px 10px;
  border-radius: 10px;
  background: var(--bg-subtle);
  border: 1px solid var(--border);
}
.conn .dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--text-dim);
}
.conn.online { color: var(--success); }
.conn.online .dot {
  background: var(--success);
  box-shadow: 0 0 0 3px var(--success-soft);
}
.search { position: relative; flex: 1; }
.search-icon {
  position: absolute;
  left: 10px;
  top: 50%;
  transform: translateY(-50%);
  color: var(--text-dim);
  pointer-events: none;
}
.search input { padding-left: 32px; background: var(--bg-subtle); }
.select-wrap { position: relative; width: 150px; flex-shrink: 0; }
.select-wrap select { appearance: none; padding-right: 28px; background: var(--bg-subtle); }
.select-chev {
  position: absolute;
  right: 10px;
  top: 50%;
  transform: translateY(-50%);
  color: var(--text-dim);
  pointer-events: none;
}
.counter { color: var(--text-dim); font-size: 12px; font-variant-numeric: tabular-nums; }

.list {
  flex: 1;
  overflow-y: auto;
  font-family: var(--font-mono);
  font-size: 12px;
  padding: 4px 0;
}
.row {
  display: flex;
  gap: 12px;
  padding: 5px 16px;
  align-items: center;
  white-space: nowrap;
  overflow-x: hidden;
  border-bottom: 1px solid var(--border);
}
.row:hover { background: var(--bg-hover); }
.ts { color: var(--text-dim); font-variant-numeric: tabular-nums; width: 72px; flex-shrink: 0; }
.trace {
  color: var(--accent);
  font-weight: 600;
  width: 96px;
  flex-shrink: 0;
  font-variant-numeric: tabular-nums;
  font-family: var(--font-mono);
  font-size: 11px;
  background: transparent;
  border: 1px solid transparent;
  padding: 1px 6px;
  border-radius: 4px;
  cursor: pointer;
  text-align: left;
}
.trace:hover {
  background: var(--accent-soft);
  border-color: var(--accent-soft);
  color: var(--accent-hover);
}
.kind {
  color: var(--text-dim);
  font-size: 10.5px;
  text-transform: uppercase;
  letter-spacing: 0.05em;
  font-weight: 600;
  min-width: 90px;
  flex-shrink: 0;
}
.method { font-weight: 700; color: var(--accent); }
.path { color: var(--text); }
.msg {
  color: var(--graphql);
  display: inline-flex;
  align-items: center;
  gap: 3px;
}
.status {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  padding: 1px 7px;
  border-radius: 10px;
  font-weight: 600;
  font-size: 11px;
}
.status.ok { background: var(--success-soft); color: var(--success); }
.status.fail { background: var(--error-soft); color: var(--error); }
.dur { color: var(--text-dim); font-variant-numeric: tabular-nums; }
.err-text { color: var(--error); }
.row.err { background: var(--error-soft); }
.empty {
  color: var(--text-dim);
  padding: 60px 20px;
  text-align: center;
  font-family: var(--font-sans);
  font-size: 13px;
}
</style>
