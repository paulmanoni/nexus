<script setup>
import { ref, computed, onMounted, onUnmounted, watch } from 'vue'
import { ShieldCheck, ShieldOff, Trash2, RefreshCw, AlertCircle } from 'lucide-vue-next'
import { fetchAuth, invalidateAuth, subscribeEvents } from '../../lib/api.js'
import { formatRelative } from '../../lib/time.js'

// AuthDetail is the drawer content for the global Auth surface — the
// only drawer kind without an ownership relationship to a canvas node.
// It packs both panels of the legacy Auth tab into the drawer's narrow
// 480px column:
//   - Cached identities                   (each = compact card)
//   - Recent 401/403 rejections (live)    (chronological list)
// Plus header actions: refresh + invalidate-all.
//
// Wraps the same /__nexus/auth REST + /__nexus/events WS the legacy
// view used. Lifecycle: subscribe on mount, unsubscribe on unmount,
// poll every 5s so cache TTL expirations show up while the drawer is
// open without needing a manual refresh.

// state shapes:
//   loading        — first fetch pending
//   configured     — auth.Module is wired; snapshot is valid
//   not-configured — /__nexus/auth returned 404
//   error          — transport / server failure
const state = ref({ kind: 'loading', snapshot: null, error: '' })
const rejects = ref([])
const REJECT_CAP = 50

let traceSub = null
let pollTimer = null

async function refresh() {
  try {
    const data = await fetchAuth()
    if (data === null) {
      state.value = { kind: 'not-configured', snapshot: null, error: '' }
      return
    }
    state.value = { kind: 'configured', snapshot: data, error: '' }
  } catch (err) {
    state.value = { kind: 'error', snapshot: null, error: err.message || String(err) }
  }
}

onMounted(() => {
  refresh()
  traceSub = subscribeEvents(ev => {
    if (ev.kind !== 'auth.reject') return
    rejects.value.unshift({
      at: ev.timestamp || new Date().toISOString(),
      service: ev.service || '',
      endpoint: ev.endpoint || '',
      reason: (ev.meta && ev.meta.reason) || 'unknown',
      identity: (ev.meta && ev.meta.identity) || '',
      error: ev.error || '',
      status: ev.status || 0,
    })
    if (rejects.value.length > REJECT_CAP) rejects.value.length = REJECT_CAP
  }, null, 0)
  // Light polling so cache expiries surface without a manual click.
  pollTimer = setInterval(refresh, 5000)
})
onUnmounted(() => {
  if (traceSub) traceSub.close()
  if (pollTimer) clearInterval(pollTimer)
})

const identities = computed(() => state.value.snapshot?.identities || [])
const cachingEnabled = computed(() => !!state.value.snapshot?.cachingEnabled)

const invalidating = ref('')
async function forceLogout(id) {
  if (!id || invalidating.value) return
  invalidating.value = id
  try {
    const res = await invalidateAuth({ id })
    await refresh()
    rejects.value.unshift({
      at: new Date().toISOString(),
      service: '', endpoint: '',
      reason: 'admin-invalidate',
      identity: id,
      error: `dropped ${res.dropped} session(s)`,
      status: 200,
    })
  } catch (err) {
    state.value.error = err.message || String(err)
  } finally {
    invalidating.value = ''
  }
}

const invalidatingAll = ref(false)
async function invalidateAllSessions() {
  if (invalidatingAll.value) return
  // No server "invalidate all" endpoint — sweep the visible snapshot.
  // Best-effort; sessions arriving between fetches survive but the
  // operator can re-run.
  invalidatingAll.value = true
  let dropped = 0
  for (const row of identities.value) {
    if (!row.Identity?.ID) continue
    try {
      const res = await invalidateAuth({ id: row.Identity.ID })
      dropped += res.dropped || 0
    } catch { /* keep going */ }
  }
  await refresh()
  rejects.value.unshift({
    at: new Date().toISOString(),
    service: '', endpoint: '',
    reason: 'admin-invalidate',
    identity: '<all>',
    error: `dropped ${dropped} session(s)`,
    status: 200,
  })
  invalidatingAll.value = false
}
</script>

<template>
  <div class="auth-detail">
    <!-- Header strip: cache pill + refresh + invalidate-all -->
    <section class="section">
      <div class="hdr-row">
        <span
          v-if="state.kind === 'configured'"
          class="cache-pill"
          :class="{ off: !cachingEnabled }"
        >
          <span class="dot" :class="{ on: cachingEnabled }" />
          cache {{ cachingEnabled ? 'enabled' : 'disabled' }}
        </span>
        <button class="action ghost" @click="refresh" title="Refresh now">
          <RefreshCw :size="13" :stroke-width="2" />
          Refresh
        </button>
        <button
          v-if="state.kind === 'configured' && identities.length > 0"
          class="action danger"
          :disabled="invalidatingAll"
          @click="invalidateAllSessions"
        >
          <Trash2 :size="13" :stroke-width="2" />
          {{ invalidatingAll ? 'Invalidating…' : 'Invalidate all' }}
        </button>
      </div>
    </section>

    <!-- Empty / not-configured / error states -->
    <section v-if="state.kind === 'loading'" class="section">
      <div class="placeholder">Loading…</div>
    </section>
    <section v-else-if="state.kind === 'not-configured'" class="section">
      <div class="placeholder big">
        <ShieldOff :size="22" :stroke-width="1.6" />
        <p>
          <strong>auth.Module is not wired in this app.</strong><br>
          Add <code>auth.Module(auth.Config{Resolve: …})</code> to your
          <code>nexus.Run</code> options to unlock per-identity cache
          inspection and live reject monitoring.
        </p>
      </div>
    </section>
    <section v-else-if="state.kind === 'error'" class="section">
      <div class="error-panel">
        <AlertCircle :size="14" :stroke-width="2" />
        <span>Failed to load auth state: {{ state.error }}</span>
      </div>
    </section>

    <template v-else>
      <!-- Cached identities -->
      <section class="section">
        <h3>
          Cached identities
          <span class="count">{{ identities.length }}</span>
        </h3>
        <div v-if="!cachingEnabled" class="placeholder">
          Cache is disabled — identities are re-resolved on every
          request. Enable with <code>auth.CacheFor(ttl)</code> to
          populate this list.
        </div>
        <div v-else-if="identities.length === 0" class="placeholder">
          No cached identities. Entries appear as authenticated
          requests come in; they expire per <code>Cache.TTL</code>.
        </div>
        <div v-else class="identities">
          <div v-for="(row, i) in identities" :key="i" class="identity">
            <div class="identity-head">
              <code class="token">{{ row.TokenPrefix || '—' }}</code>
              <span class="expires">{{ formatRelative(row.ExpiresAt) }}</span>
              <button
                class="kill"
                :disabled="!row.Identity?.ID || invalidating === row.Identity?.ID"
                :title="row.Identity?.ID ? 'Invalidate every cached session for this identity' : 'No identity ID; cannot sweep'"
                @click="forceLogout(row.Identity?.ID)"
              >
                <Trash2 :size="12" :stroke-width="2" />
              </button>
            </div>
            <div class="identity-id">{{ row.Identity?.ID || '—' }}</div>
            <div v-if="(row.Identity?.Roles || []).length || (row.Identity?.Scopes || []).length" class="identity-tags">
              <code v-for="r in row.Identity?.Roles || []" :key="'r:' + r" class="tag tag-role">{{ r }}</code>
              <code v-for="s in row.Identity?.Scopes || []" :key="'s:' + s" class="tag tag-scope">{{ s }}</code>
            </div>
          </div>
        </div>
      </section>

      <!-- Recent rejections — fed by the auth.reject WS event stream. -->
      <section class="section">
        <h3>
          Recent rejections
          <span class="count">{{ rejects.length }}</span>
        </h3>
        <div v-if="rejects.length === 0" class="placeholder">
          No rejections yet. 401 / 403 responses stream in as
          <code>auth.reject</code> events arrive.
        </div>
        <div v-else class="rejects">
          <div v-for="(r, i) in rejects" :key="i" class="reject" :class="{ admin: r.reason === 'admin-invalidate' }">
            <div class="reject-head">
              <span class="when">{{ formatRelative(r.at) }}</span>
              <span class="reason" :class="r.reason">{{ r.reason }}</span>
              <span v-if="r.status" class="status-code">{{ r.status }}</span>
            </div>
            <div v-if="r.service || r.endpoint" class="reject-target">
              <code>{{ r.service || '—' }}</code>
              <span class="dot-sep">·</span>
              <code>{{ r.endpoint || '—' }}</code>
            </div>
            <div v-if="r.identity" class="reject-identity">
              <span class="dim">identity</span>
              <code>{{ r.identity }}</code>
            </div>
            <div v-if="r.error" class="reject-error">{{ r.error }}</div>
          </div>
        </div>
      </section>
    </template>
  </div>
</template>

<style scoped>
.auth-detail {
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
  display: flex;
  align-items: center;
  gap: var(--space-2);
}
.section h3 .count {
  margin-left: auto;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-muted);
  font-weight: 500;
}

/* Header action strip */
.hdr-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex-wrap: wrap;
}
.cache-pill {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  background: var(--st-healthy-soft);
  color: var(--st-healthy);
  padding: 3px 9px;
  border-radius: 999px;
}
.cache-pill.off { background: var(--st-error-soft); color: var(--st-error); }
.cache-pill .dot {
  width: 6px; height: 6px; border-radius: 50%;
  background: var(--text-dim);
}
.cache-pill .dot.on { background: currentColor; }

.action {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 5px 10px;
  font-size: var(--fs-xs);
  font-weight: 500;
}
.action.ghost {
  background: transparent;
  border: 1px solid var(--border);
  color: var(--text-muted);
}
.action.ghost:hover { background: var(--bg-hover); color: var(--text); }
.action.danger {
  background: var(--st-error-soft);
  color: var(--st-error);
  border: 1px solid color-mix(in srgb, var(--st-error) 30%, transparent);
}
.action.danger:hover:not(:disabled) {
  background: color-mix(in srgb, var(--st-error) 18%, transparent);
}
.action:disabled { opacity: 0.55; cursor: not-allowed; }

.placeholder {
  padding: var(--space-3);
  background: var(--bg-subtle);
  border: 1px dashed var(--border-strong);
  border-radius: var(--radius-md);
  font-size: var(--fs-sm);
  color: var(--text-muted);
  line-height: 1.5;
}
.placeholder.big {
  display: flex;
  flex-direction: column;
  align-items: center;
  text-align: center;
  gap: var(--space-2);
  padding: var(--space-5) var(--space-4);
}
.placeholder code {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  background: var(--bg-hover);
  padding: 1px 6px;
  border-radius: var(--radius-sm);
  color: var(--text);
}

.error-panel {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: var(--space-3);
  background: var(--st-error-soft);
  color: var(--st-error);
  border: 1px solid color-mix(in srgb, var(--st-error) 30%, transparent);
  border-radius: var(--radius-md);
  font-size: var(--fs-sm);
}

/* Identity cards — one per cached session, compact */
.identities {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.identity {
  background: var(--bg-subtle);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  padding: var(--space-3);
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.identity-head {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}
.identity-head .token {
  flex: 1;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-dim);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.identity-head .expires {
  font-size: var(--fs-xs);
  color: var(--text-muted);
}
.kill {
  background: transparent;
  border: 1px solid transparent;
  padding: 4px 6px;
  color: var(--text-muted);
  cursor: pointer;
  border-radius: var(--radius-sm);
}
.kill:hover:not(:disabled) {
  color: var(--st-error);
  background: var(--st-error-soft);
}
.kill:disabled { opacity: 0.35; cursor: not-allowed; }

.identity-id {
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--text);
}
.identity-tags {
  margin-top: 2px;
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}
.tag {
  display: inline-flex;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  padding: 1px 7px;
  border-radius: 999px;
  border: 1px solid transparent;
}
.tag-role {
  background: color-mix(in srgb, var(--cat-service) 10%, transparent);
  color: var(--cat-service);
  border-color: color-mix(in srgb, var(--cat-service) 25%, transparent);
}
.tag-scope {
  background: color-mix(in srgb, var(--cat-queue) 10%, transparent);
  color: var(--cat-queue);
  border-color: color-mix(in srgb, var(--cat-queue) 25%, transparent);
}

/* Reject rows — chronological */
.rejects {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}
.reject {
  background: var(--bg-subtle);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  padding: var(--space-2) var(--space-3);
  display: flex;
  flex-direction: column;
  gap: 3px;
}
.reject.admin { background: var(--accent-soft); border-color: color-mix(in srgb, var(--accent) 22%, transparent); }
.reject-head {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}
.when { font-size: var(--fs-xs); color: var(--text-dim); }
.reason {
  font-family: var(--font-mono);
  font-size: 9.5px;
  font-weight: 600;
  letter-spacing: 0.04em;
  text-transform: uppercase;
  padding: 1px 7px;
  border-radius: 999px;
  background: var(--bg-hover);
  color: var(--text-muted);
  border: 1px solid transparent;
}
.reason.unauthenticated { background: var(--st-warn-soft);   color: #92400e; }
.reason.forbidden       { background: var(--st-error-soft);  color: var(--st-error); }
.reason.admin-invalidate{ background: var(--accent-soft);    color: var(--accent); }
.status-code {
  margin-left: auto;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-muted);
  font-variant-numeric: tabular-nums;
}
.reject-target {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-muted);
  display: flex;
  gap: 4px;
}
.reject-target code { color: var(--text); }
.dot-sep { color: var(--text-dim); }
.reject-identity {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-muted);
}
.reject-identity code { color: var(--text); margin-left: 4px; }
.reject-identity .dim { color: var(--text-dim); text-transform: uppercase; letter-spacing: 0.04em; font-size: 9.5px; }
.reject-error {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--st-error);
}
</style>