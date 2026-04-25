<script setup>
import { ref, onMounted, computed } from 'vue'
import { Search, Gauge, Save, RotateCcw, AlertCircle } from 'lucide-vue-next'
import { fetchEndpoints, fetchRateLimits, configureRateLimit, resetRateLimit } from '../lib/api.js'
import { usePoll } from '../lib/usePoll.js'

// Rate limits view joins registry endpoints with the live store snapshot.
// - Endpoints give us the declared baseline + the set of keys known at boot.
// - Store snapshot (/ratelimits) carries the EFFECTIVE limit after any
//   operator override, plus the overridden flag.
// A row is editable via inline inputs; Save POSTs, Reset DELETEs.

const endpoints = ref([])
const records = ref([])  // []Record from store snapshot
const filter = ref('')
const saving = ref(null) // key currently saving

async function load() {
  const [ep, rl] = await Promise.all([fetchEndpoints(), fetchRateLimits()])
  endpoints.value = ep.endpoints || []
  records.value = rl.limits || []
}

onMounted(load)
// Poll so other operators' overrides show up live.
usePoll(load, 3000)

// Drafts live in a map keyed by "<service>.<op>". While a row is being
// edited we hold the in-progress values here so rerenders from polling
// don't clobber typing.
const drafts = ref({})

function keyFor(e) { return `${e.Service}.${e.Name}` }

function recordFor(e) {
  const k = keyFor(e)
  return records.value.find(r => r.key === k) || null
}

function effective(e) {
  const r = recordFor(e)
  if (r) return r.effective
  return e.RateLimit || { rpm: 0, burst: 0, perIP: false }
}

function declared(e) {
  const r = recordFor(e)
  if (r) return r.declared
  return e.RateLimit || { rpm: 0, burst: 0, perIP: false }
}

function isOverridden(e) {
  const r = recordFor(e)
  return !!(r && r.overridden)
}

function draftFor(e) {
  const k = keyFor(e)
  if (!drafts.value[k]) {
    const eff = effective(e)
    drafts.value[k] = {
      rpm: eff.rpm || 0,
      burst: eff.burst || 0,
      perIP: !!eff.perIP,
    }
  }
  return drafts.value[k]
}

function isDirty(e) {
  const d = draftFor(e)
  const eff = effective(e)
  return d.rpm !== (eff.rpm || 0)
      || d.burst !== (eff.burst || 0)
      || d.perIP !== !!eff.perIP
}

async function onSave(e) {
  const k = keyFor(e)
  const d = drafts.value[k]
  saving.value = k
  try {
    await configureRateLimit(e.Service, e.Name, {
      rpm: Number(d.rpm) || 0,
      burst: Number(d.burst) || 0,
      perIP: !!d.perIP,
    })
    delete drafts.value[k]
    await load()
  } catch (err) {
    console.error(err)
  }
  saving.value = null
}

async function onReset(e) {
  const k = keyFor(e)
  saving.value = k
  try {
    await resetRateLimit(e.Service, e.Name)
    delete drafts.value[k]
    await load()
  } catch (err) {
    console.error(err)
  }
  saving.value = null
}

const filtered = computed(() => {
  const f = filter.value.toLowerCase()
  // Sort: rate-limited ops (have a declared limit or an override record) first,
  // then alphabetical. Readers care about what's actively throttled.
  const list = endpoints.value.filter(e => {
    if (!f) return true
    return (e.Service + ' ' + e.Name).toLowerCase().includes(f)
  })
  return list.sort((a, b) => {
    const ra = recordFor(a) || a.RateLimit
    const rb = recordFor(b) || b.RateLimit
    if (!!ra !== !!rb) return ra ? -1 : 1
    if (a.Service !== b.Service) return a.Service.localeCompare(b.Service)
    return a.Name.localeCompare(b.Name)
  })
})
</script>

<template>
  <div class="ratelimits">
    <header>
      <span class="title">
        <Gauge :size="16" :stroke-width="2" /> Rate limits
      </span>
      <div class="search">
        <Search :size="14" :stroke-width="2" class="search-icon" />
        <input v-model="filter" placeholder="Search endpoints" />
      </div>
      <span class="counter">{{ filtered.length }} / {{ endpoints.length }}</span>
    </header>
    <div class="list">
      <table class="table">
        <thead>
          <tr>
            <th>Endpoint</th>
            <th>Declared</th>
            <th>Effective (editable)</th>
            <th style="text-align: right"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="e in filtered" :key="e.Service + '.' + e.Name" :class="{ overridden: isOverridden(e), disabled: !declared(e).rpm && !isOverridden(e) }">
            <td>
              <div class="ep"><span class="service">{{ e.Service }}</span>.<span class="op">{{ e.Name }}</span></div>
              <div class="transport">{{ e.Transport }}<span v-if="e.Method"> · {{ e.Method }}</span></div>
            </td>
            <td class="mono">
              <template v-if="declared(e).rpm">
                {{ declared(e).rpm }} rpm<span v-if="declared(e).burst"> · burst {{ declared(e).burst }}</span><span v-if="declared(e).perIP"> · per-IP</span>
              </template>
              <span v-else class="dim">— (no declared limit)</span>
            </td>
            <td>
              <div class="inputs">
                <label>RPM
                  <input type="number" min="0" v-model.number="draftFor(e).rpm" />
                </label>
                <label>Burst
                  <input type="number" min="0" v-model.number="draftFor(e).burst" />
                </label>
                <label class="check">
                  <input type="checkbox" v-model="draftFor(e).perIP" />
                  per-IP
                </label>
                <span v-if="isOverridden(e)" class="badge">
                  <AlertCircle :size="11" /> overridden
                </span>
              </div>
            </td>
            <td class="actions">
              <button class="btn primary" :disabled="!isDirty(e) || saving === keyFor(e)" @click="onSave(e)">
                <Save :size="13" /> Save
              </button>
              <button class="btn" :disabled="!isOverridden(e) || saving === keyFor(e)" @click="onReset(e)">
                <RotateCcw :size="13" /> Reset
              </button>
            </td>
          </tr>
          <tr v-if="!filtered.length">
            <td colspan="4" class="empty">No endpoints.</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>

<style scoped>
.ratelimits { display: flex; flex-direction: column; height: 100%; background: var(--bg); }
header {
  display: flex;
  align-items: center;
  gap: 14px;
  padding: 12px 16px;
  border-bottom: 1px solid var(--border);
  background: var(--bg);
  flex-shrink: 0;
}
.title {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  font-weight: 600;
  font-size: 13.5px;
}
.search { position: relative; flex: 1; max-width: 420px; }
.search-icon {
  position: absolute; left: 10px; top: 50%;
  transform: translateY(-50%); color: var(--text-dim); pointer-events: none;
}
.search input { padding-left: 32px; background: var(--bg-subtle); width: 100%; }
.counter { color: var(--text-dim); font-size: 12px; font-variant-numeric: tabular-nums; }

.list { flex: 1; overflow-y: auto; padding: 16px; }
.table { width: 100%; border-collapse: collapse; background: var(--bg); border: 1px solid var(--border); border-radius: var(--radius); overflow: hidden; }
.table th {
  text-align: left;
  padding: 8px 14px;
  font-size: 10.5px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--text-muted);
  border-bottom: 1px solid var(--border);
  background: var(--bg-subtle);
  font-weight: 600;
}
.table td { padding: 10px 14px; border-bottom: 1px solid var(--border); vertical-align: top; font-size: 12px; }
.table tr:last-child td { border-bottom: none; }
.table tr.overridden { background: #fffbeb; }
.table tr.disabled td { opacity: 0.6; }

.ep { font-family: var(--font-mono); font-weight: 500; }
.ep .service { color: var(--text-dim); }
.ep .op { color: var(--text); }
.transport { font-size: 10.5px; color: var(--text-dim); margin-top: 2px; text-transform: uppercase; letter-spacing: 0.04em; }

.mono { font-family: var(--font-mono); font-size: 11.5px; color: var(--text); }
.dim { color: var(--text-dim); }

.inputs { display: flex; gap: 10px; align-items: center; flex-wrap: wrap; }
.inputs label { display: inline-flex; align-items: center; gap: 6px; font-size: 11px; color: var(--text-muted); }
.inputs label.check { cursor: pointer; }
.inputs input[type="number"] { width: 72px; padding: 4px 6px; font-family: var(--font-mono); font-size: 12px; }
.badge {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  font-size: 10px;
  padding: 1px 8px;
  border-radius: 10px;
  background: #fef3c7;
  color: #92400e;
  border: 1px solid #fde68a;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.04em;
}

.actions { text-align: right; white-space: nowrap; }
.btn {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 5px 10px;
  border-radius: var(--radius);
  border: 1px solid var(--border);
  background: var(--bg);
  font-weight: 500;
  cursor: pointer;
  color: var(--text);
  font-size: 11.5px;
  margin-left: 6px;
}
.btn:hover:not(:disabled) { background: var(--bg-hover); }
.btn:disabled { opacity: 0.4; cursor: not-allowed; }
.btn.primary { background: var(--accent); border-color: var(--accent); color: #fff; }
.btn.primary:hover:not(:disabled) { background: var(--accent-hover); }

.empty { text-align: center; color: var(--text-dim); padding: 40px 20px; }
</style>
