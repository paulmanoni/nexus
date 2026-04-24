<script setup>
import { ref, computed, watch } from 'vue'
import { X, Activity, Loader2, CheckCircle2, XCircle, Globe } from 'lucide-vue-next'
import { fetchTrace } from '../lib/api.js'

// TraceWaterfall renders one trace's span tree as a Gantt-style chart.
// Each row is a span; bar width is durationMs / totalMs, bar offset is
// startMs / totalMs. Rows are indented by depth in the parent tree so
// the caller/callee relationship is obvious at a glance.
//
// The server (dashboard.traceByID) returns spans sorted by startMs and
// already normalizes timestamps against the earliest event — we only
// reshape into a flat list with depth + a "max endMs" denominator.
const props = defineProps({
  open: Boolean,
  traceId: String,
})
const emit = defineEmits(['close'])

const loading = ref(false)
const error = ref(null)
const spans = ref([])
const selected = ref(null) // spanId of the span whose attrs are expanded

// Map each span to its depth in the tree. Walk parents iteratively so we
// handle deep chains without recursion; unknown parents default to depth 0
// (treat as root — happens if the parent event aged out of the ring buffer
// but the child is still present).
function withDepth(list) {
  const byId = new Map(list.map(s => [s.spanId, s]))
  const depthCache = new Map()
  function depthOf(s) {
    if (!s) return 0
    if (depthCache.has(s.spanId)) return depthCache.get(s.spanId)
    if (!s.parentId) { depthCache.set(s.spanId, 0); return 0 }
    const parent = byId.get(s.parentId)
    const d = parent ? depthOf(parent) + 1 : 0
    depthCache.set(s.spanId, d)
    return d
  }
  return list.map(s => ({ ...s, depth: depthOf(s) }))
}

const flat = computed(() => withDepth(spans.value))

// Denominator for bar widths: the farthest endMs across all spans. Gives
// the bars a stable 0..totalMs canvas even when the root's durationMs is
// shorter than a late child (shouldn't happen for sane traces, but keeps
// the UI robust).
const totalMs = computed(() => {
  let max = 0
  for (const s of flat.value) {
    const end = (s.startMs || 0) + (s.durationMs || 0)
    if (end > max) max = end
  }
  return Math.max(max, 1)
})

function barStyle(s) {
  const pct = (v) => (v / totalMs.value) * 100
  const dur = Math.max(s.durationMs || 0, 0)
  // Minimum width so a 0-ms span is still visible as a tick.
  const w = Math.max(pct(dur), 0.5)
  return {
    left: `${pct(s.startMs || 0)}%`,
    width: `${w}%`,
  }
}

function barClass(s) {
  if (s.error) return 'bar bar-err'
  if (s.status && s.status >= 400) return 'bar bar-err'
  if (s.kind === 'request.start') return 'bar bar-root'
  return 'bar'
}

async function load() {
  if (!props.traceId) return
  loading.value = true
  error.value = null
  selected.value = null
  try {
    const data = await fetchTrace(props.traceId)
    if (!data) {
      error.value = 'Trace not found (may have aged out of the ring buffer).'
      spans.value = []
    } else {
      spans.value = data.spans || []
    }
  } catch (e) {
    error.value = e.message || String(e)
    spans.value = []
  } finally {
    loading.value = false
  }
}

watch(() => [props.open, props.traceId], ([open]) => {
  if (open) load()
  else { spans.value = []; error.value = null; selected.value = null }
}, { immediate: true })

function onBackdrop(e) {
  if (e.target.classList.contains('backdrop')) emit('close')
}

function fmtDur(ms) {
  if (ms == null) return ''
  if (ms < 1) return '<1ms'
  if (ms < 1000) return `${ms}ms`
  return `${(ms / 1000).toFixed(2)}s`
}

function toggleSelect(spanId) {
  selected.value = selected.value === spanId ? null : spanId
}

// Gridlines along the time axis — 0%, 25%, 50%, 75%, 100%. Rendered once
// as a backdrop so spans float over them.
const gridlines = [0, 25, 50, 75, 100]

function attrEntries(attrs) {
  if (!attrs) return []
  return Object.entries(attrs).map(([k, v]) => ({
    k,
    v: typeof v === 'object' ? JSON.stringify(v) : String(v),
  }))
}
</script>

<template>
  <div v-if="open" class="backdrop" @click="onBackdrop">
    <div class="dialog" role="dialog" aria-modal="true">
      <header>
        <div class="title">
          <Activity :size="15" :stroke-width="2.2" />
          <span>Trace</span>
          <code>{{ traceId }}</code>
          <span class="counter">{{ spans.length }} span{{ spans.length === 1 ? '' : 's' }} · {{ fmtDur(totalMs) }}</span>
        </div>
        <button class="close" @click="emit('close')" aria-label="Close">
          <X :size="15" :stroke-width="2" />
        </button>
      </header>

      <div v-if="loading" class="banner">
        <Loader2 :size="13" :stroke-width="2" class="spin" /> loading
      </div>
      <div v-else-if="error" class="banner err">{{ error }}</div>

      <div v-if="!loading && !error && spans.length" class="body">
        <div class="axis">
          <span v-for="g in gridlines" :key="g" class="axis-tick" :style="{ left: g + '%' }">
            {{ Math.round((g / 100) * totalMs) }}ms
          </span>
        </div>
        <div class="rows">
          <template v-for="s in flat" :key="s.spanId">
            <div class="row" :class="{ selected: selected === s.spanId }" @click="toggleSelect(s.spanId)">
              <div class="label" :style="{ paddingLeft: (s.depth * 14 + 12) + 'px' }">
                <span class="name">{{ s.name || s.endpoint || '(unnamed)' }}</span>
                <span v-if="s.service && s.kind === 'request.start'" class="service">{{ s.service }}</span>
                <span v-if="s.remote" class="chip remote" title="Continued from an upstream traceparent">
                  <Globe :size="10" :stroke-width="2" /> remote
                </span>
                <span v-if="s.status" class="chip" :class="s.status < 400 ? 'ok' : 'fail'">
                  <component :is="s.status < 400 ? CheckCircle2 : XCircle" :size="10" :stroke-width="2" />
                  {{ s.status }}
                </span>
              </div>
              <div class="track">
                <div v-for="g in gridlines" :key="g" class="grid" :style="{ left: g + '%' }"></div>
                <div :class="barClass(s)" :style="barStyle(s)" :title="`${s.startMs}ms + ${fmtDur(s.durationMs)}`"></div>
              </div>
              <div class="dur">{{ fmtDur(s.durationMs) }}</div>
            </div>

            <div v-if="selected === s.spanId" class="detail">
              <div v-if="s.error" class="err-row">
                <XCircle :size="12" :stroke-width="2" /> {{ s.error }}
              </div>
              <div class="meta-grid">
                <div><span class="k">span</span><code>{{ s.spanId }}</code></div>
                <div v-if="s.parentId"><span class="k">parent</span><code>{{ s.parentId }}</code></div>
                <div><span class="k">start</span>{{ s.startMs }}ms</div>
                <div><span class="k">duration</span>{{ fmtDur(s.durationMs) }}</div>
                <div v-if="s.service"><span class="k">service</span>{{ s.service }}</div>
                <div v-if="s.endpoint"><span class="k">endpoint</span>{{ s.endpoint }}</div>
                <div v-if="s.transport"><span class="k">transport</span>{{ s.transport }}</div>
              </div>
              <div v-if="attrEntries(s.attrs).length" class="attrs">
                <div class="attrs-title">Attributes</div>
                <div class="attrs-grid">
                  <div v-for="a in attrEntries(s.attrs)" :key="a.k" class="attr">
                    <span class="k">{{ a.k }}</span>
                    <code>{{ a.v }}</code>
                  </div>
                </div>
              </div>
            </div>
          </template>
        </div>
      </div>

      <div v-else-if="!loading && !error && !spans.length" class="empty">
        No spans recorded.
      </div>
    </div>
  </div>
</template>

<style scoped>
.backdrop {
  position: fixed; inset: 0;
  background: rgba(17, 24, 39, 0.45);
  display: grid; place-items: center;
  z-index: 40;
}
.dialog {
  background: var(--bg-card);
  border-radius: 10px;
  box-shadow: var(--shadow-lg);
  width: min(980px, 96vw);
  height: min(720px, 90vh);
  display: flex; flex-direction: column;
  overflow: hidden;
  font-family: var(--font-sans);
}
header {
  display: flex; align-items: center; justify-content: space-between;
  padding: 12px 16px;
  border-bottom: 1px solid var(--border);
  background: var(--bg-subtle);
  flex-shrink: 0;
}
.title {
  display: inline-flex; align-items: center; gap: 8px;
  font-weight: 600; font-size: 13.5px; color: var(--text);
}
.title code {
  font-family: var(--font-mono);
  background: var(--bg-hover);
  padding: 1px 6px;
  border-radius: 4px;
  font-size: 11px;
}
.counter {
  font-size: 11px;
  color: var(--text-dim);
  font-variant-numeric: tabular-nums;
  font-weight: 500;
}
.close {
  background: transparent; border: 1px solid transparent;
  border-radius: 6px; padding: 4px;
  color: var(--text-dim);
}
.close:hover { background: var(--bg-hover); color: var(--text); }

.banner {
  display: flex; gap: 6px; align-items: center;
  padding: 10px 16px;
  font-size: 12px;
  color: var(--text-dim);
  border-bottom: 1px solid var(--border);
}
.banner.err { background: var(--error-soft); color: var(--error); }
.spin { animation: spin 1s linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }

.body {
  flex: 1;
  overflow-y: auto;
  padding: 0 0 12px 0;
}

.axis {
  position: sticky; top: 0;
  background: var(--bg);
  border-bottom: 1px solid var(--border);
  height: 26px;
  z-index: 2;
  margin-left: 280px;  /* matches .label width */
  margin-right: 60px;  /* matches .dur width */
  position: relative;
}
.axis-tick {
  position: absolute;
  top: 6px;
  transform: translateX(-50%);
  font-size: 10px;
  color: var(--text-dim);
  font-variant-numeric: tabular-nums;
  font-family: var(--font-mono);
}

.rows { display: flex; flex-direction: column; }
.row {
  display: grid;
  grid-template-columns: 280px 1fr 60px;
  align-items: center;
  min-height: 28px;
  cursor: pointer;
  border-bottom: 1px solid var(--border);
}
.row:hover { background: var(--bg-hover); }
.row.selected { background: var(--bg-active); }

.label {
  display: flex; align-items: center; gap: 6px;
  padding-right: 10px;
  font-size: 12px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  min-width: 0;
}
.name {
  font-family: var(--font-mono);
  font-weight: 500;
  color: var(--text);
  overflow: hidden;
  text-overflow: ellipsis;
}
.service {
  font-size: 10.5px;
  color: var(--text-dim);
  background: var(--bg-hover);
  padding: 1px 6px;
  border-radius: 8px;
  font-weight: 500;
}
.chip {
  display: inline-flex; align-items: center; gap: 3px;
  font-size: 10px;
  padding: 1px 6px;
  border-radius: 8px;
  font-weight: 600;
}
.chip.ok { background: var(--success-soft); color: var(--success); }
.chip.fail { background: var(--error-soft); color: var(--error); }
.chip.remote { background: var(--accent-soft); color: var(--accent); }

.track {
  position: relative;
  height: 18px;
  margin: 4px 8px;
  background: var(--bg-subtle);
  border-radius: 3px;
}
.grid {
  position: absolute;
  top: 0; bottom: 0;
  width: 1px;
  background: var(--border);
}
.bar {
  position: absolute;
  top: 2px; bottom: 2px;
  background: var(--accent);
  border-radius: 2px;
  min-width: 2px;
}
.bar-root { background: var(--text-muted); }
.bar-err { background: var(--error); }

.dur {
  text-align: right;
  padding-right: 12px;
  font-size: 11px;
  font-family: var(--font-mono);
  color: var(--text-dim);
  font-variant-numeric: tabular-nums;
}

.detail {
  padding: 10px 16px 14px 16px;
  background: var(--bg-subtle);
  border-bottom: 1px solid var(--border);
  font-size: 12px;
}
.err-row {
  display: inline-flex; align-items: center; gap: 5px;
  color: var(--error);
  font-family: var(--font-mono);
  font-size: 12px;
  margin-bottom: 8px;
}
.meta-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: 4px 18px;
  color: var(--text-muted);
}
.meta-grid .k {
  display: inline-block;
  min-width: 72px;
  color: var(--text-dim);
  font-size: 10.5px;
  text-transform: uppercase;
  letter-spacing: 0.05em;
  font-weight: 600;
  margin-right: 6px;
}
.meta-grid code {
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--text);
}
.attrs { margin-top: 10px; }
.attrs-title {
  font-size: 10.5px;
  text-transform: uppercase;
  letter-spacing: 0.05em;
  font-weight: 600;
  color: var(--text-muted);
  margin-bottom: 4px;
}
.attrs-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(240px, 1fr));
  gap: 4px 18px;
}
.attr .k {
  display: inline-block;
  min-width: 60px;
  color: var(--text-dim);
  font-size: 11px;
  margin-right: 6px;
}
.attr code {
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--text);
  background: var(--bg-hover);
  padding: 0 4px;
  border-radius: 3px;
}

.empty {
  flex: 1;
  display: grid; place-items: center;
  color: var(--text-dim);
  font-size: 13px;
}
</style>