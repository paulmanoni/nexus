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
// RENDER_CAP bounds how many rows the v-for actually paints. Above
// this, older events stay in events.value (counter on the header
// reflects the full ring, filter matches still find them) but the
// DOM tree skips them. Without this the activity rail painted 200
// rows per flush — each row 5–6 helper-function calls plus a
// StackTrace child component — which dropped frames noticeably
// under a high-QPS app.
const RENDER_CAP = 80
const filter = ref('')
const kindFilter = ref('all') // 'all' | 'request' | 'auth' | 'error'
const connected = ref(false)
const expanded = ref(false)
let ws = null

// decorateEvent precomputes the per-row display strings the template
// would otherwise recompute on every flush. Stashed as non-enumerable
// fields so they don't round-trip through JSON.
//   __t  formatted timestamp
//   __k  kind family ('request' | 'auth' | 'span' | 'other')
//   __sk short kind ('start' | 'end' | 'op' | …)
//   __tx transport class ('rest' | 'graphql' | 'websocket' | 'other')
//   __ok status >= 200 && < 400
//   __sd has stack to render
function decorateEvent(e) {
  if (e.__decorated) return
  let ts = ''
  try { ts = fmtTime(e.timestamp) } catch { ts = '' }
  const kind = e.kind || ''
  let kf = 'other'
  if (kind.startsWith('request'))    kf = 'request'
  else if (kind.startsWith('auth'))  kf = 'auth'
  else if (kind.startsWith('span'))  kf = 'span'
  const sk = kind.replace('request.', '').replace('auth.', 'auth ').replace('.', ' ')
  const t = (e.transport || '').toLowerCase()
  const tx = (t === 'rest' || t === 'graphql' || t === 'websocket') ? t : 'other'
  const props = {
    __decorated: true,
    __t:  { value: ts, enumerable: false },
    __k:  { value: kf, enumerable: false },
    __sk: { value: sk, enumerable: false },
    __tx: { value: tx, enumerable: false },
    __sd: { value: !!e.stack, enumerable: false },
  }
  try {
    Object.defineProperty(e, '__t',  props.__t)
    Object.defineProperty(e, '__k',  props.__k)
    Object.defineProperty(e, '__sk', props.__sk)
    Object.defineProperty(e, '__tx', props.__tx)
    Object.defineProperty(e, '__sd', props.__sd)
    Object.defineProperty(e, '__decorated', { value: true, enumerable: false })
  } catch {
    // Frozen / sealed event — fall back to plain assignment.
    e.__t = ts; e.__k = kf; e.__sk = sk; e.__tx = tx; e.__sd = !!e.stack
    e.__decorated = true
  }
}

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

// filtered returns matching events newest-first, capped at
// RENDER_CAP. The header badge still uses events.length for the
// total — the cap only governs how many rows the DOM paints.
const filtered = computed(() => {
  const f = filter.value.toLowerCase().trim()
  const out = []
  for (let i = events.value.length - 1; i >= 0; i--) {
    if (out.length >= RENDER_CAP) break
    const e = events.value[i]
    if (!passesKind(e)) continue
    if (f) {
      let h = e.__h
      if (h === undefined) {
        h = JSON.stringify(e).toLowerCase()
        try { Object.defineProperty(e, '__h', { value: h, enumerable: false }) }
        catch { e.__h = h }
      }
      if (!h.includes(f)) continue
    }
    out.push(e)
  }
  return out
})

// totalMatching counts every event passing the filter — even those
// past RENDER_CAP. Header shows "<rendered> / <totalMatching>" so
// the user knows when the rail is truncating output.
const totalMatching = computed(() => {
  const f = filter.value.toLowerCase().trim()
  if (!f && kindFilter.value === 'all') return events.value.length
  let n = 0
  for (let i = 0; i < events.value.length; i++) {
    const e = events.value[i]
    if (!passesKind(e)) continue
    if (f) {
      let h = e.__h
      if (h === undefined) {
        h = JSON.stringify(e).toLowerCase()
        try { Object.defineProperty(e, '__h', { value: h, enumerable: false }) }
        catch { e.__h = h }
      }
      if (!h.includes(f)) continue
    }
    n++
  }
  return n
})

// Unread badge — count of events seen since the rail was last
// expanded. Resets on expand. Lets a collapsed rail show "look at me"
// without the user scrolling back to find the new one. Tracked
// directly on event ingestion so it's accurate even before the
// batched flush lands new events into events.value.
const unread = ref(0)
watch(expanded, (v) => { if (v) unread.value = 0 })

// Batched ingestion: a WS burst (especially WebSocket-heavy apps with
// 5–50 frames per second) used to push each event synchronously,
// re-running the filtered computed and re-rendering the v-for per
// event. That blocked interaction — typing in the filter or clicking
// chips felt frozen because the main thread was diffing 200-row
// lists between every event.
//
// Now incoming events accumulate in a pending buffer; a single
// requestAnimationFrame flush moves them into events.value at most
// once per frame (~16ms). Result: filtered re-runs once per frame
// regardless of incoming rate, and the rail stays interactive even
// under sustained bursts.
let pendingBuf = []
let flushScheduled = false
function scheduleFlush() {
  if (flushScheduled) return
  flushScheduled = true
  requestAnimationFrame(() => {
    flushScheduled = false
    if (!pendingBuf.length) return
    const incoming = pendingBuf
    pendingBuf = []
    let next = events.value.concat(incoming)
    if (next.length > MAX) next = next.slice(next.length - MAX)
    events.value = next
  })
}

onMounted(() => {
  ws = subscribeEvents(
    e => {
      // Decorate before queueing so the v-for body has no function
      // calls — display strings are cached on the event object.
      decorateEvent(e)
      pendingBuf.push(e)
      // Increment unread eagerly — it's a single ref bump, doesn't
      // trigger the v-for diff. The user sees "look at me" pulse in
      // real time even though the visible list updates per-frame.
      if (!expanded.value) unread.value++
      scheduleFlush()
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
      <span class="counter">{{ filtered.length }} <span class="dim">/ {{ totalMatching }}</span></span>
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
        :class="[e.__tx, { err: e.error || (e.status && e.status >= 400) }]"
      >
        <div class="row-main">
          <span class="ts">{{ e.__t }}</span>
          <span class="kind" :class="e.__k">{{ e.__sk }}</span>
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
        <StackTrace v-if="e.__sd" :stack="e.stack" />
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