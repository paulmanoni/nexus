<script setup>
import { computed, ref } from 'vue'
import { Play, Pause, RotateCcw } from 'lucide-vue-next'
import { triggerCron, setCronPaused } from '../../lib/api.js'

// CronDetail is the drawer content for a clicked Cron node. Surfaces
// schedule, paused/running state, last run outcome, recent history.
// Trigger / pause actions are placeholders for now — the existing
// Crons tab already has the wiring; we'll fold them in here next.
const props = defineProps({
  cron: { type: Object, required: true },
})

const c = computed(() => props.cron)
const lastErr = computed(() => c.value.lastRun?.error || '')
const lastRunOK = computed(() => c.value.lastRun?.success === true)

const pillState = computed(() => {
  if (c.value.paused) return 'inactive'
  if (lastErr.value) return 'error'
  if (c.value.running) return 'throttled'
  if (c.value.lastRun) return lastRunOK.value ? 'healthy' : 'error'
  return 'healthy'
})
const pillLabel = computed(() => {
  if (c.value.paused) return 'paused'
  if (c.value.running) return 'running'
  if (lastErr.value) return 'failed'
  if (lastRunOK.value) return 'ok'
  return 'idle'
})

function fmt(t) {
  if (!t) return ''
  try { return new Date(t).toLocaleString() } catch { return '' }
}

const history = computed(() => Array.isArray(c.value.history) ? c.value.history.slice(0, 10) : [])

// ─── Actions ───────────────────────────────────────────────────────
// triggerCron / setCronPaused already exist in api.js — they POST to
// /__nexus/crons/:name/{trigger|pause|resume}. The drawer doesn't need
// to refresh state itself: /__nexus/live snapshots every ~2s, so the
// running/paused/lastRun fields update naturally on the next frame.
// We just gate the buttons with per-action loading flags so a clicker
// can't fire 5 of them while the request is inflight.
const triggering = ref(false)
const togglingPause = ref(false)
const actionError = ref('')

async function doTrigger() {
  if (triggering.value) return
  actionError.value = ''
  triggering.value = true
  try {
    await triggerCron(c.value.name)
  } catch (err) {
    actionError.value = err.message || String(err)
  } finally {
    triggering.value = false
  }
}
async function doTogglePause() {
  if (togglingPause.value) return
  actionError.value = ''
  togglingPause.value = true
  try {
    await setCronPaused(c.value.name, !c.value.paused)
  } catch (err) {
    actionError.value = err.message || String(err)
  } finally {
    togglingPause.value = false
  }
}
</script>

<template>
  <div class="cron-detail">
    <!-- Status + schedule headline. -->
    <section class="section">
      <h3>Status</h3>
      <div class="row">
        <span class="status" :class="pillState">
          <span class="dot" />
          {{ pillLabel }}
        </span>
        <code class="schedule">{{ c.schedule || '—' }}</code>
      </div>
      <p v-if="c.description" class="desc">{{ c.description }}</p>
      <dl class="meta">
        <template v-if="c.service">
          <dt>Service</dt>
          <dd>{{ c.service }}</dd>
        </template>
        <template v-if="c.nextRun">
          <dt>Next run</dt>
          <dd>{{ fmt(c.nextRun) }}</dd>
        </template>
        <template v-if="c.lastRun?.started">
          <dt>Last run</dt>
          <dd>{{ fmt(c.lastRun.started) }} <span v-if="c.lastRun.durationMs">· {{ c.lastRun.durationMs }} ms</span></dd>
        </template>
      </dl>
    </section>

    <!-- Last error panel — only when the most-recent run failed. -->
    <section v-if="lastErr" class="section">
      <h3>Last error</h3>
      <div class="last-error"><code>{{ lastErr }}</code></div>
    </section>

    <!-- Recent history — newest first, success/failure pip per row. -->
    <section v-if="history.length" class="section">
      <h3>Recent runs</h3>
      <div class="history">
        <div v-for="(r, i) in history" :key="i" class="history-row" :class="{ err: !r.success }">
          <span class="when">{{ fmt(r.started) }}</span>
          <span class="rstatus" :class="r.success ? 'ok' : 'fail'">{{ r.success ? 'ok' : 'fail' }}</span>
          <span class="rlat" v-if="r.durationMs">{{ r.durationMs }} ms</span>
          <span v-if="r.manual" class="manual">manual</span>
          <span v-if="r.error" class="rerr" :title="r.error">{{ r.error }}</span>
        </div>
      </div>
    </section>

    <!-- Actions — fire the existing /__nexus/crons/:name endpoints.
         No optimistic state mutation; the WS snapshot picks up the
         change within 2s. Buttons gate themselves while the request
         is inflight. -->
    <section class="section">
      <h3>Actions</h3>
      <div class="action-row">
        <button
          class="action primary"
          :disabled="triggering || c.running"
          @click="doTrigger"
        >
          <Play :size="14" :stroke-width="2" />
          {{ triggering ? 'Triggering…' : 'Trigger now' }}
        </button>
        <button
          class="action"
          :disabled="togglingPause"
          @click="doTogglePause"
        >
          <component :is="c.paused ? RotateCcw : Pause" :size="14" :stroke-width="2" />
          {{ togglingPause ? '…' : (c.paused ? 'Resume' : 'Pause') }}
        </button>
      </div>
      <div v-if="actionError" class="action-error">{{ actionError }}</div>
    </section>
  </div>
</template>

<style scoped>
.cron-detail {
  padding: var(--space-4) var(--space-5) var(--space-5);
  display: flex;
  flex-direction: column;
  gap: var(--space-5);
}
.section h3 {
  margin: 0 0 var(--space-2);
  font-size: var(--fs-md);
  font-weight: 600;
  color: var(--text);
}

.row {
  display: flex;
  align-items: center;
  gap: var(--space-3);
}
.status {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: var(--fs-xs);
  font-weight: 500;
  padding: 3px 10px;
  border-radius: 999px;
  text-transform: capitalize;
}
.status .dot { width: 6px; height: 6px; border-radius: 50%; background: currentColor; }
.status.healthy   { background: var(--st-healthy-soft);   color: var(--st-healthy); }
.status.throttled { background: var(--st-throttled-soft); color: var(--st-throttled); }
.status.error     { background: var(--st-error-soft);     color: var(--st-error); }
.status.inactive  { background: var(--st-inactive-soft);  color: var(--st-inactive); }
.schedule {
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
  color: var(--text);
  background: var(--bg-hover);
  padding: 2px 8px;
  border-radius: var(--radius-sm);
}
.desc {
  margin: var(--space-2) 0 0;
  font-size: var(--fs-sm);
  color: var(--text-muted);
}

.meta {
  margin: var(--space-3) 0 0;
  display: grid;
  grid-template-columns: max-content 1fr;
  gap: 4px var(--space-3);
}
.meta dt {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-dim);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.meta dd {
  margin: 0;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text);
}

.last-error {
  padding: var(--space-3);
  background: var(--st-error-soft);
  border: 1px solid color-mix(in srgb, var(--st-error) 30%, transparent);
  border-radius: var(--radius-md);
}
.last-error code {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text);
  white-space: pre-wrap;
  word-break: break-word;
}

.history {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.history-row {
  display: grid;
  grid-template-columns: 88px 50px 64px max-content 1fr;
  gap: var(--space-2);
  align-items: center;
  padding: 5px var(--space-2);
  background: var(--bg-subtle);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  font-variant-numeric: tabular-nums;
}
.history-row.err { border-color: color-mix(in srgb, var(--st-error) 25%, var(--border)); }
.when { color: var(--text-dim); }
.rstatus { font-weight: 600; }
.rstatus.ok   { color: var(--st-healthy); }
.rstatus.fail { color: var(--st-error); }
.rlat { color: var(--text-muted); }
.manual {
  font-size: 9px;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--cat-service);
  background: color-mix(in srgb, var(--cat-service) 12%, transparent);
  padding: 1px 5px;
  border-radius: var(--radius-sm);
}
.rerr {
  color: var(--st-error);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.placeholder {
  padding: var(--space-3);
  background: var(--bg-subtle);
  border: 1px dashed var(--border-strong);
  border-radius: var(--radius-md);
  font-size: var(--fs-sm);
  color: var(--text-muted);
  text-align: center;
}

/* Action row — primary "Trigger now" + secondary pause/resume.
   Disabled state shows opacity/cursor so the user reads the gate. */
.action-row {
  display: flex;
  gap: var(--space-2);
  flex-wrap: wrap;
}
.action {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 7px 14px;
  font-size: var(--fs-sm);
  font-weight: 500;
}
.action.primary {
  background: var(--accent);
  color: white;
  border-color: var(--accent);
}
.action.primary:hover:not(:disabled) {
  background: var(--accent-hover);
}
.action:disabled { opacity: 0.55; cursor: not-allowed; }
.action-error {
  margin-top: var(--space-2);
  padding: 8px var(--space-3);
  background: var(--st-error-soft);
  color: var(--st-error);
  border: 1px solid color-mix(in srgb, var(--st-error) 30%, transparent);
  border-radius: var(--radius-sm);
  font-size: var(--fs-xs);
  font-family: var(--font-mono);
}
</style>