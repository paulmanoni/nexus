<script setup>
import { ref, computed, onMounted, onUnmounted, watch, nextTick } from 'vue'
import {
  Activity, Search, Trash2, ChevronUp, ChevronDown,
  CheckCircle2, XCircle, ArrowRight, Wifi, WifiOff,
} from 'lucide-vue-next'
import { subscribeEvents } from '../lib/api.js'
import { formatTime as fmtTime } from '../lib/time.js'
import TraceWaterfall from './TraceWaterfall.vue'
import StackTrace from './StackTrace.vue'

// ActivityRail is the persistent bottom strip on the architecture
// canvas — the live activity feed that used to be the Traces tab,
// folded into the same surface as the topology so an operator sees
// the world and what's happening to it side-by-side.
//
// Two states:
//   - collapsed: 40px header strip with connection pill + counter +
//                expand toggle. Events still accumulate underneath.
//   - expanded:  ~280px panel with filter chips + scrolling list.
//
// Subscribes to /__nexus/events independently from the canvas's own
// trace subscription (which only watches request.op for edge pulses).
// One extra socket per dashboard tab; trivial cost for a dev tool.
const events = ref([])
const MAX = 200 // smaller ring than Traces had — rail is for "what just
                // happened", not deep history. Pop a Cmd+K → traces or
                // open the waterfall modal for full per-trace detail.
const filter = ref('')
const kindFilter = ref('all') // 'all' | 'request' | 'auth' | 'error'
const connected = ref(false)
const expanded = ref(false)
let ws = null

// Persist expanded state across reloads so an operator's preference
// survives. session-scoped — open a fresh tab and you start collapsed.
const STORAGE_KEY = 'nexus.activityRail.expanded'
try {
  const v = sessionStorage.getItem(STORAGE_KEY)
  if (v === '1') expanded.value = true
} catch { /* private mode */ }
watch(expanded, (v) => {
  try { sessionStorage.setItem(STORAGE_KEY, v ? '1' : '0') } catch { /* ignore */ }
})

const FILTERS = [
  { id: 'all',     label: 'All' },
  { id: 'request', label: 'Requests' },
  { id: 'auth',    label: 'Auth' },
  { id: 'error',   label: 'Errors' },
]
function passesKind(e) {
  switch (kindFilter.value) {
    case 'all':     return true
    case 'request': return (e.kind || '').startsWith('request')
    case 'auth':    return (e.kind || '').startsWith('auth')
    case 'error':   return !!e.error || (typeof e.status === 'number' && e.status >= 400)
    default:        return true
  }
}

const filtered = computed(() => {
  const f = filter.value.toLowerCase().trim()
  const matchText = (e) => !f || JSON.stringify(e).toLowerCase().includes(f)
  const out = []
  for (let i = events.value.length - 1; i >= 0; i--) {
    const e = events.value[i]
    if (passesKind(e) && matchText(e)) out.push(e)
  }
  return out
})

// Unread badge — count of events seen since the rail was last
// expanded. Resets on expand. Lets a collapsed rail show "look at me"
// without the user scrolling back to find the new one.
const unread = ref(0)
watch(events, () => { if (!expanded.value) unread.value++ }, { flush: 'post' })
watch(expanded, (v) => { if (v) unread.value = 0 })

onMounted(() => {
  ws = subscribeEvents(
    e => {
      events.value.push(e)
      if (events.value.length > MAX) events.value.splice(0, events.value.length - MAX)
    },
    status => { connected.value = status === 'open' }
  )
})
onUnmounted(() => { if (ws) ws.close() })

function clear() {
  events.value = []
  unread.value = 0
}

function shortKind(k) {
  return (k || '').replace('request.', '').replace('auth.', 'auth ').replace('.', ' ')
}
function kindFamily(k) {
  if (!k) return 'other'
  if (k.startsWith('request')) return 'request'
  if (k.startsWith('auth'))    return 'auth'
  if (k.startsWith('span'))    return 'span'
  return 'other'
}
function transportClass(e) {
  const t = (e.transport || '').toLowerCase()
  if (t === 'rest' || t === 'graphql' || t === 'websocket') return t
  return 'other'
}
function shortTrace(id) {
  if (!id) return ''
  return id.length > 8 ? id.slice(0, 8) : id
}

// TraceWaterfall modal — clicking any trace id opens the per-id span
// tree without leaving the canvas.
const selectedTraceId = ref(null)
function openTrace(id, ev) {
  if (ev) ev.stopPropagation()
  if (!id) return
  selectedTraceId.value = id
}
</script>

<template>
  <div class="rail" :class="{ open: expanded }">
    <!-- Header strip — always visible. Click anywhere to toggle when
         collapsed; only the chevron toggles when expanded so the
         filter chips are clickable without collapsing. -->
    <header class="rail-head" @click="expanded ? null : (expanded = true)">
      <span class="title">
        <Activity :size="13" :stroke-width="2" />
        Activity
      </span>
      <span
        class="conn"
        :class="{ online: connected }"
        :title="connected ? 'Live' : 'Reconnecting…'"
      >
        <component :is="connected ? Wifi : WifiOff" :size="12" :stroke-width="2" />
        {{ connected ? 'Live' : 'Reconnecting' }}
      </span>
      <span class="counter">{{ filtered.length }} <span class="dim">/ {{ events.length }}</span></span>
      <span v-if="!expanded && unread > 0" class="unread" :title="`${unread} new since collapse`">+{{ unread }}</span>

      <span class="spacer" />

      <!-- Expanded-only controls. -->
      <template v-if="expanded">
        <div class="search">
          <Search :size="13" :stroke-width="2" class="search-ico" />
          <input v-model="filter" placeholder="Filter…" @click.stop />
        </div>
        <div class="chips" @click.stop>
          <button
            v-for="f in FILTERS"
            :key="f.id"
            class="chip"
            :class="{ active: kindFilter === f.id }"
            @click="kindFilter = f.id"
          >
            {{ f.label }}
          </button>
        </div>
        <button class="action ghost" @click.stop="clear" title="Clear feed">
          <Trash2 :size="12" :stroke-width="2" />
        </button>
      </template>

      <button
        class="toggle"
        :title="expanded ? 'Collapse activity' : 'Expand activity'"
        @click.stop="expanded = !expanded"
      >
        <component :is="expanded ? ChevronDown : ChevronUp" :size="14" :stroke-width="2" />
      </button>
    </header>

    <!-- Expanded body — event list. v-show keeps the subscription warm
         when collapsed without paying the render cost for hidden rows. -->
    <div v-show="expanded" class="rail-body">
      <div v-if="!filtered.length" class="empty">
        <Activity :size="18" :stroke-width="1.6" />
        <p v-if="!events.length">No events yet. Trigger a request to populate the feed.</p>
        <p v-else>No events match the current filter.</p>
      </div>
      <div
        v-for="e in filtered"
        :key="e.id"
        class="row"
        :class="[transportClass(e), { err: e.error || (e.status && e.status >= 400) }]"
      >
        <div class="row-main">
          <span class="ts">{{ fmtTime(e.timestamp) }}</span>
          <span class="kind" :class="kindFamily(e.kind)">{{ shortKind(e.kind) }}</span>
          <button
            v-if="e.traceId"
            class="trace"
            :title="e.traceId"
            @click="openTrace(e.traceId, $event)"
          >#{{ shortTrace(e.traceId) }}</button>
          <span v-if="e.service" class="service">{{ e.service }}</span>
          <span v-if="e.endpoint" class="endpoint">{{ e.endpoint }}</span>
          <span v-else-if="e.method && e.path" class="endpoint">{{ e.method }} {{ e.path }}</span>
          <span v-if="e.message" class="msg">
            <ArrowRight :size="10" :stroke-width="2" />
            {{ e.message }}
          </span>
          <span class="row-spacer" />
          <span
            v-if="typeof e.status === 'number' && e.status > 0"
            class="status"
            :class="e.status < 400 ? 'ok' : 'fail'"
          >
            <component :is="e.status < 400 ? CheckCircle2 : XCircle" :size="10" :stroke-width="2.2" />
            {{ e.status }}
          </span>
          <span v-if="e.durationMs > 0" class="dur">{{ e.durationMs }} ms</span>
          <span v-if="e.error" class="err-text" :title="e.error">{{ e.error }}</span>
        </div>
        <StackTrace :stack="e.stack || ''" />
      </div>
    </div>

    <TraceWaterfall
      :open="!!selectedTraceId"
      :trace-id="selectedTraceId"
      @close="selectedTraceId = null"
    />
  </div>
</template>

<style scoped>
.rail {
  position: absolute;
  left: 0;
  right: 0;
  bottom: 0;
  z-index: 10;
  background: var(--bg-card);
  border-top: 1px solid var(--border);
  box-shadow: 0 -4px 16px rgba(15, 23, 42, 0.06);
  display: flex;
  flex-direction: column;
  font-family: var(--font-sans);
  height: 40px;
  transition: height 200ms cubic-bezier(0.32, 0.72, 0, 1);
}
.rail.open { height: 320px; }

.rail-head {
  flex-shrink: 0;
  height: 40px;
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: 0 var(--space-3);
  cursor: pointer;
  user-select: none;
}
.rail.open .rail-head { cursor: default; }

.title {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--text);
}
.title svg { color: var(--cat-service); }

.conn {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 3px 8px;
  border-radius: 999px;
  font-size: 10.5px;
  font-weight: 500;
  color: var(--text-muted);
  background: var(--st-inactive-soft);
}
.conn.online { color: var(--st-healthy); background: var(--st-healthy-soft); }

.counter {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text);
  font-variant-numeric: tabular-nums;
}
.counter .dim { color: var(--text-dim); }

.unread {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  font-weight: 600;
  padding: 2px 7px;
  border-radius: 999px;
  background: var(--accent);
  color: white;
}

.spacer { flex: 1; }

.search {
  position: relative;
  width: 200px;
}
.search-ico {
  position: absolute;
  left: 8px;
  top: 50%;
  transform: translateY(-50%);
  color: var(--text-dim);
  pointer-events: none;
}
.search input {
  padding: 4px 10px 4px 28px;
  font-size: var(--fs-xs);
}

.chips {
  display: inline-flex;
  align-items: center;
  gap: 2px;
  padding: 2px;
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  background: var(--bg-subtle);
}
.chip {
  padding: 3px 9px;
  font-size: var(--fs-xs);
  font-weight: 500;
  background: transparent;
  border: 1px solid transparent;
  color: var(--text-muted);
  border-radius: var(--radius-sm);
  cursor: pointer;
}
.chip:hover { color: var(--text); }
.chip.active {
  background: var(--bg-card);
  color: var(--accent);
  box-shadow: var(--shadow-sm);
}

.action {
  display: inline-flex;
  align-items: center;
  padding: 4px 8px;
  font-size: var(--fs-xs);
}
.action.ghost {
  background: transparent;
  border: 1px solid var(--border);
  color: var(--text-muted);
}
.action.ghost:hover { background: var(--bg-hover); color: var(--text); }

.toggle {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  padding: 4px 6px;
  background: transparent;
  border: 1px solid transparent;
  color: var(--text-muted);
  cursor: pointer;
  border-radius: var(--radius-sm);
}
.toggle:hover { background: var(--bg-hover); color: var(--text); }

/* Body — same row grammar as the (now-deleted) Traces view but more
   compact: shorter time gutter, smaller font, single-line rows. */
.rail-body {
  flex: 1;
  overflow-y: auto;
  border-top: 1px solid var(--border);
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
}
.empty {
  display: grid;
  place-items: center;
  gap: var(--space-2);
  padding: var(--space-5) var(--space-4);
  color: var(--text-dim);
  font-family: var(--font-sans);
  font-size: var(--fs-sm);
  text-align: center;
}
.empty p { margin: 0; max-width: 320px; }

.row {
  position: relative;
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 4px var(--space-3) 4px calc(var(--space-3) + 6px);
  border-bottom: 1px solid var(--border);
}
.row-main {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  white-space: nowrap;
  overflow: hidden;
}
.row::before {
  content: '';
  position: absolute;
  left: 0; top: 0; bottom: 0;
  width: 3px;
  background: transparent;
}
.row.rest::before      { background: var(--rest); }
.row.graphql::before   { background: var(--graphql); }
.row.websocket::before { background: var(--ws); }
.row.err  { background: color-mix(in srgb, var(--st-error) 6%, transparent); }
.row.err::before { background: var(--st-error); }
.row:hover { background: var(--bg-hover); }

.ts {
  width: 64px;
  flex-shrink: 0;
  color: var(--text-dim);
  font-variant-numeric: tabular-nums;
}

.kind {
  flex-shrink: 0;
  font-size: 9px;
  font-weight: 600;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  padding: 1px 6px;
  border-radius: 999px;
  background: var(--bg-hover);
  color: var(--text-muted);
}
.kind.request { background: var(--accent-soft);            color: var(--accent); }
.kind.span    { background: color-mix(in srgb, var(--cat-queue) 12%, transparent);   color: var(--cat-queue); }
.kind.auth    { background: color-mix(in srgb, var(--cat-internet) 14%, transparent); color: var(--cat-internet); }

.trace {
  width: 84px;
  flex-shrink: 0;
  background: transparent;
  border: 1px solid transparent;
  padding: 1px 6px;
  border-radius: var(--radius-sm);
  color: var(--accent);
  font-weight: 600;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  cursor: pointer;
  text-align: left;
  font-variant-numeric: tabular-nums;
}
.trace:hover {
  background: var(--accent-soft);
  border-color: color-mix(in srgb, var(--accent) 25%, transparent);
}

.service {
  color: var(--text-muted);
  flex-shrink: 0;
  max-width: 130px;
  overflow: hidden;
  text-overflow: ellipsis;
}
.endpoint {
  color: var(--text);
  overflow: hidden;
  text-overflow: ellipsis;
  flex-shrink: 1;
  min-width: 0;
}
.msg {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  color: var(--graphql);
  overflow: hidden;
  text-overflow: ellipsis;
}

.row-spacer { flex: 1; }

.status {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  padding: 1px 7px;
  border-radius: 999px;
  font-weight: 600;
  font-size: var(--fs-xs);
  flex-shrink: 0;
}
.status.ok   { background: var(--st-healthy-soft); color: var(--st-healthy); }
.status.fail { background: var(--st-error-soft);   color: var(--st-error); }

.dur {
  color: var(--text-muted);
  font-variant-numeric: tabular-nums;
  flex-shrink: 0;
}
.err-text {
  color: var(--st-error);
  flex-shrink: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
}
</style>