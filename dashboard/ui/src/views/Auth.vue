<script setup>
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { ShieldCheck, ShieldOff, Users, Trash2, RefreshCw, AlertCircle } from 'lucide-vue-next'
import { fetchAuth, invalidateAuth, subscribeEvents } from '../lib/api.js'

// state.snapshot holds the latest { identities, cachingEnabled } payload.
// state.kind is one of:
//   'loading'        – first fetch pending
//   'configured'     – auth.Module is wired; snapshot is valid
//   'not-configured' – /__nexus/auth returned 404; no auth.Module in the app
//   'error'          – fetch failed for other reasons; displays the message
const state = ref({ kind: 'loading', snapshot: null, error: '' })

// Recent 401/403 events, newest-first, capped. We populate from the same
// /__nexus/events WS that Traces uses — filtered to kind=auth.reject so
// operators can see denied requests alongside the cached identity table.
const rejects = ref([])
const REJECT_CAP = 50

let pollTimer = null
let traceSub = null

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
  // Poll every 5s so new sessions / expired entries stay reflected.
  pollTimer = setInterval(refresh, 5000)
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
    if (rejects.value.length > REJECT_CAP) {
      rejects.value.length = REJECT_CAP
    }
  }, null, 0)
})

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
  if (traceSub) traceSub.close()
})

const identities = computed(() => state.value.snapshot?.identities || [])
const cachingEnabled = computed(() => !!state.value.snapshot?.cachingEnabled)

// Format a Go/JSON time string as "in 12m" / "3s ago" relative to now,
// so the table stays scannable without a separate absolute column.
function relative(iso) {
  if (!iso) return ''
  const t = new Date(iso).getTime()
  if (!t) return iso
  const delta = Math.round((t - Date.now()) / 1000)
  if (delta > 0) return `in ${formatSpan(delta)}`
  return `${formatSpan(-delta)} ago`
}

function formatSpan(seconds) {
  if (seconds < 60) return `${seconds}s`
  const m = Math.round(seconds / 60)
  if (m < 60) return `${m}m`
  const h = Math.round(m / 60)
  if (h < 24) return `${h}h`
  return `${Math.round(h / 24)}d`
}

async function forceLogout(id) {
  if (!id) return
  if (!confirm(`Invalidate all cached sessions for identity "${id}"?`)) return
  try {
    const res = await invalidateAuth({ id })
    // Refresh after a beat so the snapshot reflects the drop. No need
    // to wait for the poll tick.
    await refresh()
    // Minor UX touch: confirm in the reject ring so admins see the
    // action they just took alongside live rejections.
    rejects.value.unshift({
      at: new Date().toISOString(),
      service: '',
      endpoint: '',
      reason: 'admin-invalidate',
      identity: id,
      error: `dropped ${res.dropped} session(s)`,
      status: 200,
    })
  } catch (err) {
    alert(`Invalidate failed: ${err.message || err}`)
  }
}

async function invalidateAll() {
  if (!confirm('Invalidate EVERY cached identity? Every active user will re-authenticate on their next request.')) return
  // No server endpoint for "all" today — loop over current snapshot
  // ids. It's best-effort: new sessions may slip in between fetches,
  // but admins see the drop count and can re-run if needed.
  let dropped = 0
  for (const row of identities.value) {
    if (!row.Identity?.ID) continue
    try {
      const res = await invalidateAuth({ id: row.Identity.ID })
      dropped += res.dropped || 0
    } catch { /* keep going */ }
  }
  await refresh()
  alert(`Invalidated ${dropped} session(s).`)
}
</script>

<template>
  <div class="auth-view">
    <header class="hdr">
      <h1>
        <ShieldCheck :size="18" :stroke-width="2" />
        Auth
      </h1>
      <div class="actions">
        <span v-if="state.kind === 'configured'" class="cache-pill" :class="{ off: !cachingEnabled }">
          <span class="dot" :class="{ on: cachingEnabled }"></span>
          cache {{ cachingEnabled ? 'enabled' : 'disabled' }}
        </span>
        <button class="btn" @click="refresh" title="Refresh now">
          <RefreshCw :size="14" :stroke-width="2" />
        </button>
        <button
          v-if="state.kind === 'configured' && identities.length > 0"
          class="btn danger"
          @click="invalidateAll"
        >
          <Trash2 :size="14" :stroke-width="2" />
          Invalidate all
        </button>
      </div>
    </header>

    <div v-if="state.kind === 'loading'" class="empty">Loading…</div>
    <div v-else-if="state.kind === 'not-configured'" class="empty">
      <ShieldOff :size="28" :stroke-width="1.5" />
      <p>
        <strong>auth.Module is not wired in this app.</strong><br>
        Add <code>auth.Module(auth.Config{Resolve: ...})</code> to your
        <code>nexus.Run</code> options to unlock per-identity cache
        inspection and live reject monitoring.
      </p>
    </div>
    <div v-else-if="state.kind === 'error'" class="empty error">
      <AlertCircle :size="22" :stroke-width="2" />
      <p>Failed to load auth state: {{ state.error }}</p>
    </div>

    <div v-else class="body">
      <!-- Identity table -->
      <section class="panel">
        <div class="panel-hdr">
          <Users :size="14" :stroke-width="2" />
          <span>Cached identities</span>
          <span class="count">{{ identities.length }}</span>
        </div>
        <div v-if="!cachingEnabled" class="hint">
          Cache is disabled in <code>Config.Cache</code> — identities are
          re-resolved on every request. Enable with
          <code>auth.CacheFor(ttl)</code> to populate this list.
        </div>
        <div v-else-if="identities.length === 0" class="hint">
          No cached identities. The list fills in as requests come in with
          valid tokens; entries expire per <code>Cache.TTL</code>.
        </div>
        <table v-else>
          <thead>
            <tr>
              <th>Token</th>
              <th>Identity</th>
              <th>Roles</th>
              <th>Scopes</th>
              <th>Expires</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(row, i) in identities" :key="i">
              <td class="mono dim">{{ row.TokenPrefix }}</td>
              <td class="mono">{{ row.Identity?.ID || '—' }}</td>
              <td>
                <span v-for="r in row.Identity?.Roles || []" :key="r" class="chip">{{ r }}</span>
              </td>
              <td>
                <span v-for="s in row.Identity?.Scopes || []" :key="s" class="chip scope">{{ s }}</span>
              </td>
              <td class="dim">{{ relative(row.ExpiresAt) }}</td>
              <td>
                <button
                  class="btn sm"
                  :disabled="!row.Identity?.ID"
                  :title="row.Identity?.ID ? 'Invalidate every cached session for this identity' : 'No identity ID; cannot sweep'"
                  @click="forceLogout(row.Identity?.ID)"
                >
                  <Trash2 :size="12" :stroke-width="2" />
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      </section>

      <!-- Recent rejects -->
      <section class="panel">
        <div class="panel-hdr">
          <ShieldOff :size="14" :stroke-width="2" />
          <span>Recent rejections</span>
          <span class="count">{{ rejects.length }}</span>
        </div>
        <div v-if="rejects.length === 0" class="hint">
          No rejections yet. 401 / 403 responses stream here via
          <code>auth.reject</code> trace events.
        </div>
        <table v-else>
          <thead>
            <tr>
              <th>When</th>
              <th>Reason</th>
              <th>Service</th>
              <th>Op</th>
              <th>Identity</th>
              <th>Error</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(r, i) in rejects" :key="i" :class="{ admin: r.reason === 'admin-invalidate' }">
              <td class="dim">{{ relative(r.at) }}</td>
              <td><span class="chip" :class="r.reason">{{ r.reason }}</span></td>
              <td class="mono">{{ r.service || '—' }}</td>
              <td class="mono">{{ r.endpoint || '—' }}</td>
              <td class="mono">{{ r.identity || '—' }}</td>
              <td class="dim">{{ r.error || '—' }}</td>
            </tr>
          </tbody>
        </table>
      </section>
    </div>
  </div>
</template>

<style scoped>
.auth-view {
  padding: 20px 24px;
  overflow: auto;
  height: 100%;
  background: var(--bg-subtle);
  font-family: var(--font-sans);
}
.hdr {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 18px;
}
.hdr h1 {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  font-size: 17px;
  margin: 0;
  color: var(--text);
}
.actions { display: inline-flex; gap: 8px; align-items: center; }
.cache-pill {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-family: var(--font-mono);
  font-size: 10.5px;
  background: var(--bg-active);
  color: var(--accent);
  padding: 3px 8px;
  border-radius: 10px;
}
.cache-pill.off { background: #fee2e2; color: #b91c1c; }
.cache-pill .dot {
  width: 6px; height: 6px; border-radius: 50%;
  background: var(--text-dim);
}
.cache-pill .dot.on { background: var(--success); }
.btn {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  border: 1px solid var(--border);
  background: var(--bg-card);
  color: var(--text);
  padding: 5px 10px;
  font-size: 12px;
  border-radius: 6px;
  cursor: pointer;
}
.btn:hover { background: var(--bg-hover); }
.btn:disabled { opacity: 0.35; cursor: not-allowed; }
.btn.sm { padding: 3px 6px; }
.btn.danger { color: var(--error); border-color: var(--error-soft); }
.btn.danger:hover { background: #fee2e2; }

.empty {
  display: grid;
  place-items: center;
  min-height: 200px;
  text-align: center;
  color: var(--text-muted);
  gap: 10px;
}
.empty.error { color: var(--error); }
.empty code {
  background: var(--bg-active);
  padding: 1px 5px;
  border-radius: 4px;
  font-size: 11.5px;
}

.body { display: flex; flex-direction: column; gap: 16px; }
.panel {
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: 8px;
  overflow: hidden;
}
.panel-hdr {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 14px;
  font-weight: 600;
  font-size: 12.5px;
  color: var(--text);
  border-bottom: 1px solid var(--border);
  background: var(--bg-subtle);
}
.panel-hdr .count {
  margin-left: auto;
  font-family: var(--font-mono);
  color: var(--text-muted);
  font-weight: 500;
}
.hint {
  padding: 14px;
  color: var(--text-muted);
  font-size: 12.5px;
  line-height: 1.5;
}
.hint code {
  background: var(--bg-active);
  padding: 1px 5px;
  border-radius: 4px;
  font-size: 11.5px;
  font-family: var(--font-mono);
}

table { width: 100%; border-collapse: collapse; }
th, td {
  text-align: left;
  padding: 8px 12px;
  font-size: 12px;
  border-bottom: 1px solid var(--border);
}
th { color: var(--text-muted); font-weight: 500; font-size: 11px; text-transform: uppercase; letter-spacing: 0.04em; }
tbody tr:last-child td { border-bottom: 0; }
tbody tr:hover { background: var(--bg-hover); }
.mono { font-family: var(--font-mono); font-size: 11px; }
.dim { color: var(--text-muted); }

.chip {
  display: inline-block;
  font-family: var(--font-mono);
  font-size: 10px;
  padding: 1px 6px;
  border-radius: 8px;
  background: var(--bg-active);
  color: var(--accent);
  margin-right: 4px;
}
.chip.scope { background: #ede9fe; color: #6b21a8; }
.chip.unauthenticated { background: #fef3c7; color: #b45309; }
.chip.forbidden { background: #fee2e2; color: #b91c1c; }
.chip.admin-invalidate { background: #dbeafe; color: #1e40af; }

tr.admin { background: #f0f9ff; }
</style>