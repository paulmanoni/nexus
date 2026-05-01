<script setup>
import { ref, computed, watch } from 'vue'
import { X, AlertTriangle, Search, Loader2 } from 'lucide-vue-next'
import { fetchErrorEvents } from '../lib/api.js'
import { formatAbsolute as fmt } from '../lib/time.js'
import StackTrace from './StackTrace.vue'

// ErrorDialog shows the recent-error ring for one endpoint, lazily
// fetched on open so the /stats hot path stays lean. The server-side
// ring is capped at metrics.RecentErrorsCap (currently 1000); we
// render the filtered list with native scroll so each row can carry
// a click-to-expand stack disclosure (added when panic stacks were
// captured server-side). Virtual scrolling was dropped because fixed
// row heights are incompatible with the variable-height stack panel.
//
// Props take service+op names (not a pre-captured list) so we can
// refetch on open without Architecture having to hold the ring in
// its state.
const props = defineProps({
  open: Boolean,
  service: String,
  op: String,
})
const emit = defineEmits(['close'])

const events = ref([])     // full ring from the server, newest first
const loading = ref(false)
const error = ref(null)
const filter = ref('')

async function load() {
  if (!props.service || !props.op) return
  loading.value = true
  error.value = null
  try {
    const data = await fetchErrorEvents(props.service, props.op)
    events.value = data.events || []
  } catch (e) {
    error.value = e.message || String(e)
    events.value = []
  } finally {
    loading.value = false
  }
}

watch(() => [props.open, props.service, props.op], ([open]) => {
  if (open) {
    filter.value = ''
    load()
  } else {
    events.value = []
  }
}, { immediate: true })

const filtered = computed(() => {
  const f = filter.value.trim().toLowerCase()
  if (!f) return events.value
  return events.value.filter(e =>
    (e.message || '').toLowerCase().includes(f) ||
    (e.ip || '').toLowerCase().includes(f)
  )
})

const total = computed(() => filtered.value.length)

function onBackdrop(e) { if (e.target.classList.contains('backdrop')) emit('close') }
const title = computed(() => `${props.service || ''}.${props.op || ''}`)
</script>

<template>
  <div v-if="open" class="backdrop" @click="onBackdrop">
    <div class="dialog" role="dialog" aria-modal="true">
      <header>
        <div class="title">
          <AlertTriangle :size="15" :stroke-width="2.2" />
          <span>Errors on <code>{{ title }}</code></span>
          <span class="counter">{{ total }}{{ filter ? ` / ${events.length}` : '' }}</span>
        </div>
        <button class="close" @click="emit('close')" aria-label="Close">
          <X :size="15" :stroke-width="2" />
        </button>
      </header>
      <div class="toolbar">
        <div class="search">
          <Search :size="13" :stroke-width="2" class="search-icon" />
          <input v-model="filter" placeholder="Filter by IP or message…" />
        </div>
        <span v-if="loading" class="loading">
          <Loader2 :size="12" :stroke-width="2" class="spin" /> loading
        </span>
      </div>
      <div v-if="error" class="error-banner">Could not load errors: {{ error }}</div>
      <div class="body">
        <div class="table-head">
          <span class="col ts">When</span>
          <span class="col ip">IP</span>
          <span class="col msg">Message</span>
        </div>
        <div v-for="(e, i) in filtered" :key="i" class="row">
          <div class="row-main">
            <span class="col ts mono">{{ fmt(e.timestamp) }}</span>
            <span class="col ip mono">{{ e.ip || '—' }}</span>
            <span class="col msg">{{ e.message }}</span>
          </div>
          <StackTrace :stack="e.stack || ''" />
        </div>
        <div v-if="!loading && !total && !error" class="empty">
          {{ filter ? 'No events match the filter.' : 'No errors recorded yet.' }}
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.backdrop {
  position: fixed;
  inset: 0;
  background: rgba(17, 24, 39, 0.45);
  display: grid;
  place-items: center;
  z-index: 40;
}
.dialog {
  background: var(--bg-card);
  border-radius: 10px;
  box-shadow: var(--shadow-lg);
  width: min(820px, 94vw);
  height: min(640px, 86vh);
  display: flex;
  flex-direction: column;
  overflow: hidden;
  font-family: var(--font-sans);
}
header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 12px 16px;
  border-bottom: 1px solid var(--border);
  background: var(--bg-subtle);
  flex-shrink: 0;
}
.title {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  font-weight: 600;
  font-size: 13.5px;
  color: var(--text);
}
.title code {
  font-family: var(--font-mono);
  background: var(--bg-hover);
  padding: 1px 6px;
  border-radius: 4px;
  font-size: 12px;
}
.counter {
  font-size: 10.5px;
  color: var(--text-dim);
  font-variant-numeric: tabular-nums;
  margin-left: 4px;
}
.close {
  background: transparent;
  border: 1px solid transparent;
  border-radius: 6px;
  padding: 4px;
  cursor: pointer;
  color: var(--text-dim);
}
.close:hover { background: var(--bg-hover); color: var(--text); }

.toolbar {
  display: flex;
  gap: 10px;
  align-items: center;
  padding: 10px 16px;
  border-bottom: 1px solid var(--border);
  flex-shrink: 0;
}
.search { position: relative; flex: 1; }
.search-icon {
  position: absolute; left: 10px; top: 50%;
  transform: translateY(-50%); color: var(--text-dim); pointer-events: none;
}
.search input {
  width: 100%;
  padding: 5px 10px 5px 30px;
  background: var(--bg);
  font-size: 12px;
}
.loading {
  display: inline-flex; align-items: center; gap: 5px;
  color: var(--text-dim); font-size: 11px;
}
.spin { animation: spin 1s linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }

.error-banner {
  padding: 8px 16px;
  background: var(--st-error-soft);
  color: var(--st-error);
  font-size: 12px;
  border-bottom: 1px solid var(--border);
}

.body {
  flex: 1;
  overflow-y: auto;
  position: relative;
}
.table-head {
  display: grid;
  grid-template-columns: 170px 130px 1fr;
  padding: 6px 16px;
  font-size: 10.5px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--text-muted);
  border-bottom: 1px solid var(--border);
  background: var(--bg);
  font-weight: 600;
  position: sticky;
  top: 0;
  z-index: 1;
}
.row {
  padding: 8px 16px;
  border-bottom: 1px solid var(--border);
  font-size: 12px;
}
.row:hover { background: var(--bg-hover); }
.row-main {
  display: grid;
  grid-template-columns: 170px 130px 1fr;
  align-items: center;
}
.col { min-width: 0; }
.mono { font-family: var(--font-mono); }
.ts { color: var(--text-muted); white-space: nowrap; }
.ip { color: var(--text); }
.msg {
  color: var(--st-error);
  font-family: var(--font-mono);
  font-size: 11.5px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.empty {
  position: absolute;
  inset: 40px 20px;
  display: grid;
  place-items: center;
  color: var(--text-dim);
  font-size: 13px;
}
</style>