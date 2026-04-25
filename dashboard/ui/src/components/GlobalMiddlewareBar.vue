<script setup>
import { ref, onMounted, computed } from 'vue'
import { Shield, Activity } from 'lucide-vue-next'
import { fetchMiddlewares } from '../lib/api.js'
import { usePoll } from '../lib/usePoll.js'

// GlobalMiddlewareBar shows the app-wide middleware chain at the top of
// the Architecture tab — left-to-right in the order a request traverses
// them on the engine root. Chips carry the middleware's Info
// (kind=builtin/custom + description) for a tooltip; metrics overlay
// (when available) can decorate them later with per-middleware counters.
const entries = ref([])  // [{ name, info }]  (null info when unknown)

async function load() {
  try {
    const data = await fetchMiddlewares()
    const byName = {}
    for (const m of data.middlewares || []) byName[m.name] = m
    const order = data.global || []
    entries.value = order.map(name => ({ name, info: byName[name] || null }))
  } catch (err) {
    console.warn('middlewares fetch failed', err)
    entries.value = []
  }
}

onMounted(load)
// Middleware set changes only at boot + rare dynamic adds — slow poll
// is plenty.
usePoll(load, 10_000)

const empty = computed(() => entries.value.length === 0)
</script>

<template>
  <div v-if="!empty" class="bar">
    <span class="label">
      <Activity :size="11" :stroke-width="2.2" /> Global middleware
    </span>
    <div class="chain">
      <template v-for="(e, i) in entries" :key="e.name">
        <span v-if="i > 0" class="arrow">→</span>
        <span
          class="chip"
          :class="e.info?.kind || 'unknown'"
          :title="e.info?.description || 'custom middleware'"
        >
          <Shield :size="10" :stroke-width="2.2" />
          {{ e.name }}
        </span>
      </template>
    </div>
  </div>
</template>

<style scoped>
.bar {
  position: absolute;
  top: 12px;
  left: 50%;
  transform: translateX(-50%);
  z-index: 10;
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 6px 14px;
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: 999px;
  box-shadow: var(--shadow-sm);
  font-family: var(--font-sans);
  pointer-events: auto;
}
.label {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 10.5px;
  font-weight: 600;
  color: var(--text-muted);
  text-transform: uppercase;
  letter-spacing: 0.06em;
}
.chain { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
.arrow { color: var(--text-dim); font-size: 11px; }
.chip {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-family: var(--font-mono);
  font-size: 11px;
  padding: 2px 8px 2px 7px;
  border-radius: 10px;
  font-weight: 500;
}
.chip.builtin { background: #eef2ff; color: #4338ca; border: 1px solid #c7d2fe; }
.chip.custom  { background: var(--bg-hover); color: var(--text); border: 1px solid var(--border); }
.chip.unknown { background: var(--bg-hover); color: var(--text-muted); border: 1px solid var(--border); }
</style>
