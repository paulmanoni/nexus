<script setup>
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import { Save, RotateCcw, Gauge } from 'lucide-vue-next'
import OpTester from './OpTester.vue'
import StackTrace from '../StackTrace.vue'
import { subscribeEvents, configureRateLimit, resetRateLimit } from '../../lib/api.js'

// OpDetail is the drawer content for a selected endpoint. Reads a
// fully-resolved endpoint object (including .Stats) and renders:
//   - Stats summary  (req count, error count, last error)
//   - Dependencies   (owner / calls / reads / middleware)
//   - GraphQL args   (when applicable)
//   - Tester slot    (placeholder; unified OpTester ships next)
//
// The endpoint is passed from Architecture.vue as a *live* computed so
// stats update with each /__nexus/live snapshot — the drawer doesn't
// snapshot at click time and go stale.
const props = defineProps({
  op: { type: Object, required: true },
})

const e = computed(() => props.op)
const transport = computed(() => e.value.Transport || 'rest')
const stats = computed(() => e.value.Stats || null)

const middlewareNames = computed(() => {
  const all = Array.isArray(e.value.Middleware) ? e.value.Middleware : []
  return all.filter(m => m !== 'metrics')
})

const args = computed(() => Array.isArray(e.value.Args) ? e.value.Args : [])

const lastErrorAt = computed(() => {
  const t = stats.value?.lastErrAt
  if (!t) return ''
  try {
    return new Date(t).toLocaleString()
  } catch { return '' }
})

// ─── Recent activity feed ──────────────────────────────────────────
// Subscribes to /__nexus/events and keeps the last RECENT_CAP request
// completions for THIS op. Drawer can switch from op to op without
// remounting (props change in place); the watch on opKey resets the
// buffer so we never show events that belong to a different endpoint.
const RECENT_CAP = 10
const recent = ref([])
let sub = null

const opKey = computed(() => `${e.value.Service || ''}.${e.value.Name || ''}`)

function pushEvent(ev) {
  const entry = {
    id: ev.id || (Date.now() + ':' + Math.random()),
    timestamp: ev.timestamp || new Date().toISOString(),
    status: typeof ev.status === 'number' ? ev.status : 0,
    duration: typeof ev.durationMs === 'number' ? ev.durationMs : 0,
    error: ev.error || '',
    stack: ev.stack || '',
  }
  const next = [entry, ...recent.value]
  if (next.length > RECENT_CAP) next.length = RECENT_CAP
  recent.value = next
}

watch(opKey, () => { recent.value = [] })

onMounted(() => {
  sub = subscribeEvents((ev) => {
    if (ev.kind !== 'request.op') return
    if (!ev.service || !ev.endpoint) return
    if (`${ev.service}.${ev.endpoint}` !== opKey.value) return
    pushEvent(ev)
  }, null, 0)
})
onUnmounted(() => { if (sub) sub.close() })

function formatTime(iso) {
  try { return new Date(iso).toLocaleTimeString() } catch { return '' }
}

// ─── Rate limit ────────────────────────────────────────────────────
// Live data path:
//   - e.RateLimit         — declared baseline from registry (always
//                           present when the source declared a limit)
//   - e.RateLimitRecord   — store snapshot when the operator has an
//                           override AND/OR a declared limit exists;
//                           gives us .declared / .effective / .overridden
// The form below edits the EFFECTIVE limit; Save POSTs the override,
// Reset DELETEs back to the declared baseline.
const rlRecord  = computed(() => e.value.RateLimitRecord || null)
const rlDeclared = computed(() =>
  rlRecord.value?.declared || e.value.RateLimit || null,
)
const rlEffective = computed(() =>
  rlRecord.value?.effective || e.value.RateLimit || null,
)
const rlOverridden = computed(() => !!rlRecord.value?.overridden)
// hasRateLimit covers both "declared in source" and "operator
// configured an override on an undeclared op" — render the editable
// form whenever either is true.
const hasRateLimit = computed(() => !!(rlDeclared.value || rlEffective.value))

// Draft state for the override form. Reset whenever the open op
// changes so a leftover draft from another endpoint doesn't bleed in.
const rlDraft = ref({ rpm: 0, burst: 0, perIP: false })
function snapshotDraft() {
  const eff = rlEffective.value || rlDeclared.value || { rpm: 0, burst: 0, perIP: false }
  rlDraft.value = {
    rpm: eff.rpm || 0,
    burst: eff.burst || 0,
    perIP: !!eff.perIP,
  }
}
watch(opKey, snapshotDraft, { immediate: true })
// Re-snapshot when the live record updates from outside (someone else
// changed the limit) AND the user isn't mid-edit.
watch(rlEffective, () => {
  if (!rlSaving.value && !rlDirty.value) snapshotDraft()
})

const rlDirty = computed(() => {
  const eff = rlEffective.value || rlDeclared.value || { rpm: 0, burst: 0, perIP: false }
  return rlDraft.value.rpm !== (eff.rpm || 0)
      || rlDraft.value.burst !== (eff.burst || 0)
      || rlDraft.value.perIP !== !!eff.perIP
})

const rlSaving = ref(false)
const rlError = ref('')
async function rlSave() {
  rlError.value = ''
  rlSaving.value = true
  try {
    await configureRateLimit(e.value.Service, e.value.Name, {
      rpm: Number(rlDraft.value.rpm) || 0,
      burst: Number(rlDraft.value.burst) || 0,
      perIP: !!rlDraft.value.perIP,
    })
  } catch (err) {
    rlError.value = err.message || String(err)
  } finally {
    rlSaving.value = false
  }
}
async function rlReset() {
  rlError.value = ''
  rlSaving.value = true
  try {
    await resetRateLimit(e.value.Service, e.value.Name)
  } catch (err) {
    rlError.value = err.message || String(err)
  } finally {
    rlSaving.value = false
  }
}
</script>

<template>
  <div class="op-detail">
    <!-- Stats summary — always present so the user has a heartbeat read
         without flipping tabs. Errors get a red value when non-zero. -->
    <section class="section">
      <h3>Stats</h3>
      <div class="stat-grid">
        <div class="stat-cell">
          <div class="stat-label">Requests</div>
          <div class="stat-value">{{ stats?.count ?? 0 }}</div>
        </div>
        <div class="stat-cell">
          <div class="stat-label">Errors</div>
          <div class="stat-value" :class="{ err: (stats?.errors || 0) > 0 }">
            {{ stats?.errors ?? 0 }}
          </div>
        </div>
      </div>
      <div v-if="stats?.lastError" class="last-error">
        <div class="le-label">Last error{{ lastErrorAt ? ' · ' + lastErrorAt : '' }}</div>
        <code>{{ stats.lastError }}</code>
        <StackTrace :stack="stats.lastErrStack || ''" />
      </div>
    </section>

    <!-- Dependencies — same data as the chip row on the canvas, but
         broken out by kind so it reads as a contract: who owns this op,
         who it calls, what it touches, what guards it. -->
    <section class="section">
      <h3>Dependencies</h3>
      <div v-if="!e.ServiceAutoRouted && e.Service" class="dep-row">
        <span class="dep-kind">Owner</span>
        <code class="tag tag-svc">{{ e.Service }}</code>
      </div>
      <div v-if="e.ServiceDeps?.length" class="dep-row">
        <span class="dep-kind">Calls</span>
        <code v-for="s in e.ServiceDeps" :key="s" class="tag tag-svc">{{ s }}</code>
      </div>
      <div v-if="e.Resources?.length" class="dep-row">
        <span class="dep-kind">Reads</span>
        <code v-for="r in e.Resources" :key="r" class="tag tag-res">{{ r }}</code>
      </div>
      <div v-if="middlewareNames.length" class="dep-row">
        <span class="dep-kind">Middleware</span>
        <code v-for="m in middlewareNames" :key="m" class="tag tag-mw">{{ m }}</code>
      </div>
      <div
        v-if="e.ServiceAutoRouted && !e.ServiceDeps?.length && !e.Resources?.length && !middlewareNames.length"
        class="empty"
      >
        No declared dependencies. The handler is auto-routed and takes no service or resource deps.
      </div>
    </section>

    <!-- GraphQL args — surfaces the schema the dashboard already knows
         about, so a user can read the op's contract without leaving
         the canvas. REST/WS skip this. -->
    <section v-if="args.length" class="section">
      <h3>Arguments</h3>
      <div v-for="a in args" :key="a.Name" class="arg-row">
        <code class="arg-name">{{ a.Name }}<span v-if="a.Required" class="req">!</span></code>
        <span class="arg-type">{{ a.Type }}</span>
        <p v-if="a.Description" class="arg-desc">{{ a.Description }}</p>
      </div>
    </section>

    <!-- Rate limit — declared baseline, current effective, override
         form. Visible whenever the op has a declared limit OR an
         operator-set override; otherwise hidden so untoggled ops
         don't accumulate empty sections. -->
    <section v-if="hasRateLimit" class="section">
      <h3>
        Rate limit
        <span v-if="rlOverridden" class="overridden-badge" title="Live override differs from the declared baseline">override</span>
      </h3>
      <div class="rl-summary">
        <div class="rl-cell">
          <div class="rl-label">Declared</div>
          <div v-if="rlDeclared && rlDeclared.rpm" class="rl-value">
            {{ rlDeclared.rpm }} <span class="rl-unit">rpm</span>
            <span v-if="rlDeclared.burst" class="rl-aux">· burst {{ rlDeclared.burst }}</span>
            <span v-if="rlDeclared.perIP" class="rl-aux">· per-IP</span>
          </div>
          <div v-else class="rl-empty">— none</div>
        </div>
        <div class="rl-cell">
          <div class="rl-label">Effective</div>
          <div v-if="rlEffective && rlEffective.rpm" class="rl-value" :class="{ overridden: rlOverridden }">
            {{ rlEffective.rpm }} <span class="rl-unit">rpm</span>
            <span v-if="rlEffective.burst" class="rl-aux">· burst {{ rlEffective.burst }}</span>
            <span v-if="rlEffective.perIP" class="rl-aux">· per-IP</span>
          </div>
          <div v-else class="rl-empty">— disabled</div>
        </div>
      </div>

      <div class="rl-form">
        <label>
          <span>RPM</span>
          <input type="number" min="0" v-model.number="rlDraft.rpm" />
        </label>
        <label>
          <span>Burst</span>
          <input type="number" min="0" v-model.number="rlDraft.burst" />
        </label>
        <label class="checkbox">
          <input type="checkbox" v-model="rlDraft.perIP" />
          <span>Per-IP</span>
        </label>
      </div>
      <div class="rl-actions">
        <button class="action primary" :disabled="!rlDirty || rlSaving" @click="rlSave">
          <Save :size="13" :stroke-width="2" />
          {{ rlSaving ? 'Saving…' : 'Save override' }}
        </button>
        <button class="action ghost" :disabled="!rlOverridden || rlSaving" @click="rlReset">
          <RotateCcw :size="13" :stroke-width="2" />
          Reset to declared
        </button>
      </div>
      <div v-if="rlError" class="rl-error">{{ rlError }}</div>
    </section>

    <!-- Live tester — picks REST / GraphQL / WS form by op.Transport,
         auto-populates inputs from the op metadata, runs requests
         directly from the drawer. Replaces the legacy per-transport
         tester components. -->
    <section class="section">
      <h3>Test endpoint</h3>
      <OpTester :op="e" />
    </section>

    <!-- Recent activity — live feed of the last completions for this
         op, populated from the /__nexus/events stream. Empty state
         nudges the user to fire a request from the tester above so the
         section reads "ready to populate" instead of "broken." -->
    <section class="section">
      <h3>Recent activity</h3>
      <div v-if="recent.length" class="recent">
        <div v-for="r in recent" :key="r.id" class="recent-row" :class="{ err: r.status >= 400 || r.error }">
          <div class="recent-main">
            <span class="when">{{ formatTime(r.timestamp) }}</span>
            <span
              class="rstatus"
              :class="(r.status && r.status < 400) ? 'ok' : (r.status >= 400 ? 'fail' : 'idle')"
            >
              {{ r.status || '—' }}
            </span>
            <span class="rlat" v-if="r.duration">{{ r.duration }} ms</span>
            <span v-if="r.error" class="rerr" :title="r.error">{{ r.error }}</span>
          </div>
          <StackTrace :stack="r.stack" />
        </div>
      </div>
      <div v-else class="placeholder">
        No requests yet. Fire one from the tester above and it'll appear here.
      </div>
    </section>
  </div>
</template>

<style scoped>
.op-detail {
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

/* Stats grid — two equal columns with subtle borders. Mirrors the
   AWS CloudWatch summary blocks. */
.stat-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--space-3);
}
.stat-cell {
  padding: var(--space-3);
  background: var(--bg-subtle);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
}
.stat-label {
  font-size: var(--fs-xs);
  color: var(--text-dim);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.stat-value {
  margin-top: 4px;
  font-family: var(--font-mono);
  font-size: var(--fs-xl);
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}
.stat-value.err { color: var(--st-error); }

.last-error {
  margin-top: var(--space-3);
  padding: var(--space-3);
  background: var(--st-error-soft);
  border: 1px solid color-mix(in srgb, var(--st-error) 30%, transparent);
  border-radius: var(--radius-md);
}
.le-label {
  font-size: var(--fs-xs);
  color: var(--st-error);
  text-transform: uppercase;
  letter-spacing: 0.04em;
  margin-bottom: 4px;
}
.last-error code {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text);
  white-space: pre-wrap;
  word-break: break-word;
}

/* Dep rows — left label + inline tags. Tag colours mirror the canvas
   chip system so the drawer reads as a continuation, not a different
   visual language. */
.dep-row {
  display: flex;
  align-items: baseline;
  flex-wrap: wrap;
  gap: var(--space-2);
  padding: var(--space-2) 0;
  border-bottom: 1px solid var(--border);
}
.dep-row:last-child { border-bottom: none; }
.dep-kind {
  font-size: var(--fs-xs);
  color: var(--text-dim);
  text-transform: uppercase;
  letter-spacing: 0.04em;
  min-width: 84px;
  flex-shrink: 0;
}
.tag {
  display: inline-flex;
  align-items: center;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  padding: 2px 8px;
  border-radius: 999px;
  background: var(--bg-hover);
  color: var(--text);
  border: 1px solid transparent;
}
.tag-svc {
  background: color-mix(in srgb, var(--cat-service) 10%, transparent);
  color: var(--cat-service);
  border-color: color-mix(in srgb, var(--cat-service) 25%, transparent);
}
.tag-res {
  background: color-mix(in srgb, var(--cat-database) 10%, transparent);
  color: var(--cat-database);
  border-color: color-mix(in srgb, var(--cat-database) 25%, transparent);
}
.tag-mw {
  background: var(--st-warn-soft);
  color: #92400e;
  border-color: color-mix(in srgb, var(--st-warn) 35%, transparent);
}

.empty {
  padding: var(--space-3);
  background: var(--bg-subtle);
  border: 1px dashed var(--border-strong);
  border-radius: var(--radius-md);
  font-size: var(--fs-sm);
  color: var(--text-muted);
}

/* GraphQL args. Name in mono, type lighter, description below. */
.arg-row {
  padding: var(--space-2) 0;
  border-bottom: 1px solid var(--border);
}
.arg-row:last-child { border-bottom: none; }
.arg-name {
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--text);
}
.arg-name .req { color: var(--st-error); }
.arg-type {
  margin-left: var(--space-2);
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-muted);
}
.arg-desc {
  margin: 4px 0 0;
  font-size: var(--fs-sm);
  color: var(--text-muted);
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
.placeholder strong {
  font-family: var(--font-mono);
  text-transform: uppercase;
  color: var(--text);
}

/* Recent activity — compact rows, mono numerics, ring of last 10
   request completions piped from /__nexus/events. Failed rows pick up
   a red status pill so the eye lands on them first. */
.recent {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.recent-row {
  display: flex;
  flex-direction: column;
  padding: 5px var(--space-2);
  background: var(--bg-subtle);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  font-variant-numeric: tabular-nums;
}
.recent-main {
  display: grid;
  grid-template-columns: 80px 56px 64px 1fr;
  gap: var(--space-2);
  align-items: center;
}
.recent-row.err { border-color: color-mix(in srgb, var(--st-error) 25%, var(--border)); }
.when { color: var(--text-dim); }
.rstatus {
  display: inline-flex;
  justify-content: center;
  padding: 1px 0;
  border-radius: var(--radius-sm);
  font-weight: 600;
}
.rstatus.ok    { color: var(--st-healthy); }
.rstatus.fail  { color: var(--st-error); }
.rstatus.idle  { color: var(--text-dim); }
.rlat { color: var(--text-muted); }
.rerr {
  color: var(--st-error);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

/* Rate-limit section — declared/effective summary, override form,
   action buttons. Same look as the cron Actions: primary indigo,
   ghost reset, error panel. */
.section h3 .overridden-badge {
  margin-left: var(--space-2);
  font-family: var(--font-mono);
  font-size: 9.5px;
  font-weight: 600;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  padding: 2px 7px;
  border-radius: 999px;
  background: color-mix(in srgb, var(--cat-cron) 12%, transparent);
  color: var(--cat-cron);
  border: 1px solid color-mix(in srgb, var(--cat-cron) 35%, transparent);
}
.rl-summary {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--space-3);
}
.rl-cell {
  padding: var(--space-3);
  background: var(--bg-subtle);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
}
.rl-label {
  font-size: var(--fs-xs);
  color: var(--text-dim);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.rl-value {
  margin-top: 4px;
  font-family: var(--font-mono);
  font-size: var(--fs-md);
  font-weight: 600;
  color: var(--text);
  font-variant-numeric: tabular-nums;
}
.rl-value.overridden { color: var(--cat-cron); }
.rl-unit {
  font-weight: 500;
  color: var(--text-muted);
}
.rl-aux {
  font-weight: 500;
  font-size: var(--fs-xs);
  color: var(--text-muted);
  margin-left: 4px;
}
.rl-empty {
  margin-top: 4px;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-dim);
}

.rl-form {
  margin-top: var(--space-3);
  display: grid;
  grid-template-columns: 1fr 1fr auto;
  gap: var(--space-3);
  align-items: end;
}
.rl-form label {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: var(--fs-xs);
  text-transform: uppercase;
  letter-spacing: 0.04em;
  color: var(--text-dim);
  font-weight: 600;
}
.rl-form label.checkbox {
  flex-direction: row;
  align-items: center;
  gap: 6px;
  text-transform: none;
  letter-spacing: 0;
  font-weight: 500;
  color: var(--text);
  padding-bottom: 8px;
}
.rl-form label.checkbox input { width: auto; }
.rl-form input[type="number"] {
  font-family: var(--font-mono);
}

.rl-actions {
  margin-top: var(--space-3);
  display: flex;
  gap: var(--space-2);
}
.rl-actions .action {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 6px 12px;
  font-size: var(--fs-sm);
}
.rl-actions .action.primary {
  background: var(--accent);
  color: white;
  border-color: var(--accent);
}
.rl-actions .action.primary:hover:not(:disabled) {
  background: var(--accent-hover);
}
.rl-actions .action.ghost {
  background: transparent;
  border: 1px solid var(--border);
  color: var(--text-muted);
}
.rl-actions .action:disabled { opacity: 0.55; cursor: not-allowed; }
.rl-error {
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