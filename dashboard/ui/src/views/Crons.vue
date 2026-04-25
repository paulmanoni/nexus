<script setup>
import { ref, onMounted, computed } from 'vue'
import { Search, Clock, Play, Pause, Zap, CheckCircle2, XCircle, History } from 'lucide-vue-next'
import { fetchCrons, triggerCron, setCronPaused } from '../lib/api.js'
import { usePoll } from '../lib/usePoll.js'

const crons = ref([])
const selectedName = ref(null)
const filter = ref('')
const busy = ref(false)

async function load() {
  try {
    const data = await fetchCrons()
    crons.value = data.crons || []
    if (!selectedName.value && crons.value.length) selectedName.value = crons.value[0].name
  } catch (e) {
    console.error(e)
  }
}

onMounted(load)
usePoll(load, 2000)

const filtered = computed(() => {
  const f = filter.value.toLowerCase()
  if (!f) return crons.value
  return crons.value.filter(c => (c.name + (c.service || '') + c.schedule).toLowerCase().includes(f))
})

const selected = computed(() => crons.value.find(c => c.name === selectedName.value) || null)

function fmtAbs(t) {
  if (!t) return '—'
  try { return new Date(t).toLocaleString([], { hour12: false }) } catch { return String(t) }
}

function fmtRel(t) {
  if (!t) return ''
  const d = new Date(t).getTime() - Date.now()
  const abs = Math.abs(d)
  const s = Math.round(abs / 1000)
  const label = s < 60 ? `${s}s` : s < 3600 ? `${Math.round(s/60)}m` : s < 86400 ? `${Math.round(s/3600)}h` : `${Math.round(s/86400)}d`
  return d >= 0 ? `in ${label}` : `${label} ago`
}

async function onTrigger(name) {
  if (busy.value) return
  busy.value = true
  try { await triggerCron(name); await load() } catch (e) { console.error(e) }
  busy.value = false
}

async function onToggle(c) {
  if (busy.value) return
  busy.value = true
  try { await setCronPaused(c.name, !c.paused); await load() } catch (e) { console.error(e) }
  busy.value = false
}
</script>

<template>
  <div class="crons">
    <aside>
      <div class="search">
        <Search :size="14" :stroke-width="2" class="search-icon" />
        <input v-model="filter" placeholder="Search cron jobs" />
      </div>
      <div class="list">
        <button
          v-for="c in filtered"
          :key="c.name"
          class="cron"
          :class="{ active: selectedName === c.name, paused: c.paused, running: c.running }"
          @click="selectedName = c.name"
        >
          <span class="dot" :class="{ ok: !c.paused, paused: c.paused }"></span>
          <span class="name">{{ c.name }}</span>
          <span class="sched">{{ c.schedule }}</span>
        </button>
        <div v-if="!filtered.length" class="empty-list">
          No cron jobs{{ filter ? ' match.' : ' registered.' }}
        </div>
      </div>
    </aside>

    <section class="detail">
      <div v-if="!selected" class="empty">
        <div class="empty-icon"><Clock :size="28" :stroke-width="1.5" /></div>
        <div class="empty-text">Select a cron job to see its schedule and history.</div>
      </div>
      <div v-else class="detail-body">
        <header class="detail-head">
          <Clock :size="18" :stroke-width="2" class="head-icon" />
          <h2>{{ selected.name }}</h2>
          <span v-if="selected.paused" class="badge paused">paused</span>
          <span v-else-if="selected.running" class="badge running">running</span>
          <div v-if="selected.description" class="desc">{{ selected.description }}</div>
        </header>

        <div class="actions">
          <button class="btn primary" :disabled="busy || selected.running" @click="onTrigger(selected.name)">
            <Zap :size="14" :stroke-width="2" /> Trigger now
          </button>
          <button class="btn" :disabled="busy" @click="onToggle(selected)">
            <component :is="selected.paused ? Play : Pause" :size="14" :stroke-width="2" />
            {{ selected.paused ? 'Resume' : 'Pause' }}
          </button>
        </div>

        <div class="meta">
          <div class="kv">
            <span class="k">Schedule</span>
            <span class="v mono">{{ selected.schedule }}</span>
          </div>
          <div v-if="selected.service" class="kv">
            <span class="k">Service</span>
            <span class="v">{{ selected.service }}</span>
          </div>
          <div class="kv">
            <span class="k">Next run</span>
            <span class="v mono" v-if="selected.nextRun">
              {{ fmtAbs(selected.nextRun) }} <span class="rel">({{ fmtRel(selected.nextRun) }})</span>
            </span>
            <span class="v dim" v-else>{{ selected.paused ? 'paused' : '—' }}</span>
          </div>
          <div class="kv">
            <span class="k">Last run</span>
            <span class="v mono" v-if="selected.lastRun">
              {{ fmtAbs(selected.lastRun.started) }} <span class="rel">({{ fmtRel(selected.lastRun.started) }})</span>
            </span>
            <span class="v dim" v-else>never</span>
          </div>
        </div>

        <div class="history-block">
          <div class="section-title">
            <History :size="14" :stroke-width="2" /> History
          </div>
          <div v-if="!selected.history || !selected.history.length" class="empty-inline">
            No runs recorded yet.
          </div>
          <table v-else class="history">
            <thead>
              <tr>
                <th>Started</th>
                <th>Duration</th>
                <th>Result</th>
                <th>Kind</th>
                <th>Error</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="(r, i) in selected.history" :key="i" :class="{ fail: !r.success }">
                <td class="mono">{{ fmtAbs(r.started) }}</td>
                <td class="mono">{{ r.durationMs }}ms</td>
                <td>
                  <span class="status" :class="r.success ? 'ok' : 'fail'">
                    <component :is="r.success ? CheckCircle2 : XCircle" :size="11" :stroke-width="2" />
                    {{ r.success ? 'ok' : 'fail' }}
                  </span>
                </td>
                <td class="dim">{{ r.manual ? 'manual' : 'scheduled' }}</td>
                <td class="err-cell">{{ r.error || '' }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </section>
  </div>
</template>

<style scoped>
.crons { display: grid; grid-template-columns: 320px 1fr; height: 100%; background: var(--bg); }
aside { border-right: 1px solid var(--border); display: flex; flex-direction: column; overflow: hidden; background: var(--bg); }
.search { position: relative; padding: 12px; border-bottom: 1px solid var(--border); }
.search-icon { position: absolute; left: 24px; top: 50%; transform: translateY(-50%); color: var(--text-dim); pointer-events: none; }
.search input { padding-left: 34px; background: var(--bg-subtle); }
.list { flex: 1; overflow-y: auto; padding: 8px 8px 16px; }

.cron {
  display: grid;
  grid-template-columns: 12px 1fr auto;
  gap: 8px;
  align-items: center;
  width: 100%;
  text-align: left;
  padding: 7px 10px;
  border: 1px solid transparent;
  background: transparent;
  border-radius: var(--radius);
  font-size: 12px;
  margin-bottom: 1px;
  color: var(--text);
  font-weight: 500;
}
.cron:hover { background: var(--bg-hover); }
.cron.active { background: var(--bg-active); color: var(--accent); }
.cron.paused .name { color: var(--text-dim); }
.cron .dot { width: 8px; height: 8px; border-radius: 50%; background: var(--text-dim); }
.cron .dot.ok { background: var(--success); box-shadow: 0 0 0 3px var(--success-soft); }
.cron .dot.paused { background: var(--text-dim); }
.cron .name { font-family: var(--font-mono); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.cron .sched { font-family: var(--font-mono); font-size: 10.5px; color: var(--text-dim); }
.cron.running { background: var(--accent-soft); }

.empty-list { color: var(--text-dim); padding: 20px; text-align: center; font-size: 12px; }

.detail { overflow-y: auto; padding: 28px 32px; background: var(--bg-subtle); }
.detail-body { max-width: 920px; margin: 0 auto; }
.empty { height: 100%; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 12px; color: var(--text-dim); }
.empty-icon { color: var(--text-dim); opacity: 0.5; }
.empty-text { font-size: 13px; }

.detail-head { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; padding-bottom: 16px; border-bottom: 1px solid var(--border); margin-bottom: 16px; }
.head-icon { color: var(--accent); }
.detail-head h2 { margin: 0; font-size: 16px; font-weight: 600; font-family: var(--font-mono); }
.detail-head .desc { width: 100%; color: var(--text-dim); font-size: 13px; }

.badge { font-size: 10px; padding: 2px 8px; border-radius: 10px; font-weight: 700; letter-spacing: 0.04em; text-transform: uppercase; }
.badge.paused { background: var(--bg-hover); color: var(--text-muted); border: 1px solid var(--border); }
.badge.running { background: var(--accent-soft); color: var(--accent); border: 1px solid #c7d2fe; }

.actions { display: flex; gap: 8px; margin-bottom: 20px; }
.btn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 6px 12px;
  border-radius: var(--radius);
  border: 1px solid var(--border);
  background: var(--bg);
  font-weight: 500;
  cursor: pointer;
  color: var(--text);
}
.btn:hover:not(:disabled) { background: var(--bg-hover); }
.btn:disabled { opacity: 0.5; cursor: not-allowed; }
.btn.primary { background: var(--accent); border-color: var(--accent); color: #fff; }
.btn.primary:hover:not(:disabled) { background: var(--accent-hover); }

.meta {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 10px 24px;
  padding: 14px 16px;
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  margin-bottom: 20px;
}
.kv { display: flex; flex-direction: column; gap: 4px; min-width: 0; }
.kv .k {
  font-size: 10.5px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  font-weight: 600;
  color: var(--text-muted);
}
.kv .v { font-size: 13px; color: var(--text); word-break: break-word; }
.kv .v.dim { color: var(--text-dim); }
.mono { font-family: var(--font-mono); }
.rel { color: var(--text-dim); font-size: 11px; }

.section-title {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 10.5px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  font-weight: 600;
  color: var(--text-muted);
  margin-bottom: 10px;
}
.history { width: 100%; border-collapse: collapse; background: var(--bg); border: 1px solid var(--border); border-radius: var(--radius); overflow: hidden; }
.history th {
  text-align: left;
  padding: 8px 12px;
  font-size: 10.5px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--text-muted);
  border-bottom: 1px solid var(--border);
  background: var(--bg-subtle);
  font-weight: 600;
}
.history td { padding: 8px 12px; border-bottom: 1px solid var(--border); font-size: 12px; }
.history tr:last-child td { border-bottom: none; }
.history tr.fail { background: var(--bg-error); }
.status { display: inline-flex; align-items: center; gap: 4px; padding: 2px 8px; border-radius: 10px; font-weight: 600; font-size: 11px; }
.status.ok { background: var(--success-soft); color: var(--success); }
.status.fail { background: var(--error-soft); color: var(--error); }
.err-cell { color: var(--error); font-family: var(--font-mono); font-size: 11.5px; }
.dim { color: var(--text-dim); }
.empty-inline { color: var(--text-dim); padding: 16px; background: var(--bg); border: 1px dashed var(--border); border-radius: var(--radius); text-align: center; font-size: 12px; }
</style>