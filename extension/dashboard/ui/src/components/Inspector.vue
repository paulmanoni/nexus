<script setup>
import { ref, computed, watch } from 'vue'
import { Info, Lock, ShieldCheck, Globe, AlertTriangle, Gauge, X } from 'lucide-vue-next'
import CategoryIcon from './CategoryIcon.vue'

// Inspector — the persistent right-hand panel of the redesigned dashboard.
// Replaces the slide-over drawer as the always-on detail surface. It's a
// pure view: Architecture.vue resolves the selected canvas node into a rich
// `target` (endpoints + crons + limits + traces + auth tallies, all live
// against the latest /__nexus/live snapshot) and the Inspector just renders
// the facet tabs. Clicking an endpoint row emits `open-op` so the parent can
// open the deep-dive drawer (tester / waterfall / rate-limit editor).
const props = defineProps({
  target: { type: Object, default: null },
  totals: { type: Object, default: () => ({ services: 0, eps: 0, rps: 0, errs: 0 }) },
  appName: { type: String, default: 'Nexus' },
})
const emit = defineEmits(['open-op', 'open-errors', 'close'])

// Active facet tab, reset to Endpoints whenever the selection changes.
const tab = ref('Endpoints')
watch(() => props.target && props.target.id, () => { tab.value = 'Endpoints' })

const t = computed(() => props.target)
const hasEndpoints = computed(() => !!(t.value && Array.isArray(t.value.endpoints)))

// Which facet tabs apply to the current target. Crons/Limits are
// conditional on the node actually having scheduled jobs / a rate-limit
// guard so we never show an empty tab.
const tabs = computed(() => {
  if (!hasEndpoints.value) return []
  const out = ['Endpoints', 'Auth', 'Traces']
  if ((t.value.crons || []).length) out.push('Crons')
  if (t.value.limited) out.push('Limits')
  return out
})

// Endpoint groups — REST / Queries / Mutations / Realtime & jobs. Mirrors
// the redesign's transport bucketing.
const BUCKETS = [
  { key: 'rest', label: 'REST', kinds: ['rest'] },
  { key: 'query', label: 'Queries', kinds: ['query'] },
  { key: 'mutation', label: 'Mutations', kinds: ['mutation'] },
  { key: 'ws', label: 'Realtime & jobs', kinds: ['ws', 'worker', 'cron'] },
]
const epGroups = computed(() => {
  if (!hasEndpoints.value) return []
  return BUCKETS
    .map(b => ({ ...b, items: t.value.endpoints.filter(e => b.kinds.includes(e.kind)) }))
    .filter(b => b.items.length)
})

// Auth tally — protected vs public, plus a sorted scope list (which
// permission gates how many endpoints).
const authScopes = computed(() => {
  if (!hasEndpoints.value) return { prot: 0, pub: 0, scopes: [] }
  let prot = 0, pub = 0
  const m = {}
  for (const e of t.value.endpoints) {
    if (!e.auth) { pub++; continue }
    prot++
    if (e.auth.indexOf('requires:') === 0) {
      const p = e.auth.slice(9)
      m[p] = (m[p] || 0) + 1
    }
  }
  const scopes = Object.keys(m).map(perm => ({ perm, count: m[perm] })).sort((a, b) => b.count - a.count)
  return { prot, pub, scopes }
})

const traces = computed(() => (t.value && t.value.traces) || [])
const crons = computed(() => (t.value && t.value.crons) || [])
const limits = computed(() => (t.value && t.value.limits) || null)

const GLYPH = { rest: 'REST', query: 'Q', mutation: 'M', ws: 'WS', worker: 'WK', cron: 'CR' }
function glyph(kind) { return GLYPH[kind] || '?' }

function authLabel(a) {
  if (!a) return { txt: 'public', cls: 'public', perm: false }
  if (a === 'required') return { txt: 'auth required', cls: '', perm: false }
  return { txt: a.replace('requires:', ''), cls: '', perm: true }
}

const eyebrow = computed(() => {
  if (!t.value) return ''
  if (t.value.kind === 'module') return 'Module'
  return t.value.kind
})
</script>

<template>
  <aside class="inspector">
    <template v-if="t">
      <div class="insp-head">
        <div class="insp-eyebrow">
          <span>{{ eyebrow }}</span>
          <button class="insp-close" @click="emit('close')"><X :size="15" :stroke-width="2" /></button>
        </div>
        <div class="insp-title">
          <CategoryIcon :type="t.icon" :size="34" />
          <div class="insp-title-text">
            <div class="insp-name">{{ t.label }}</div>
            <div class="insp-desc" v-if="t.desc">{{ t.desc }}</div>
          </div>
        </div>
      </div>

      <div class="insp-stats">
        <div class="stat"><div class="k">Endpoints</div><div class="v">{{ t.count != null ? t.count : '—' }}</div></div>
        <div class="stat"><div class="k">Throughput</div><div class="v">{{ t.rps }}<small>/s</small></div></div>
        <div class="stat"><div class="k">Error rate</div><div class="v" :class="{ warn: t.errPct > 0 && t.errPct < 2, err: t.errPct >= 2 }">{{ t.errPct }}<small>%</small></div></div>
      </div>

      <template v-if="hasEndpoints">
        <div class="insp-tabs">
          <button v-for="tb in tabs" :key="tb" class="insp-tab" :class="{ 'is-active': tab === tb }" @click="tab = tb">{{ tb }}</button>
        </div>
        <div class="insp-scroll">
          <!-- Endpoints -->
          <template v-if="tab === 'Endpoints'">
            <div v-for="g in epGroups" :key="g.key">
              <div class="grp-head">
                <span class="tdot" :class="g.key">{{ g.label }}</span>
                <span class="n">{{ g.items.length }}</span>
              </div>
              <div v-for="(e, ei) in g.items" :key="ei" class="ep" @click="e.endpoint && emit('open-op', e.endpoint)">
                <span class="ep-glyph" :class="e.kind">{{ glyph(e.kind) }}</span>
                <div class="ep-main">
                  <div class="ep-name"><span v-if="e.method" class="verb">{{ e.method }} </span>{{ e.name }}</div>
                  <span class="ep-auth" :class="authLabel(e.auth).cls">
                    <Lock v-if="authLabel(e.auth).perm" :size="10" :stroke-width="2" />
                    <ShieldCheck v-else-if="e.auth" :size="10" :stroke-width="2" />
                    <Globe v-else :size="10" :stroke-width="2" />
                    {{ authLabel(e.auth).txt }}
                  </span>
                </div>
                <div class="ep-right">
                  <div class="ep-p50">{{ e.p50 != null ? e.p50 + 'ms' : (e.reqs || 0) + ' req' }}</div>
                  <button v-if="e.errors" class="ep-err" @click.stop="emit('open-errors', e)"><AlertTriangle :size="10" :stroke-width="2" />{{ e.errors }}</button>
                  <div v-else class="ep-err zero">0</div>
                </div>
              </div>
            </div>
          </template>

          <!-- Auth -->
          <template v-else-if="tab === 'Auth'">
            <div class="facet-sum">
              <div class="fs"><div class="fsv">{{ authScopes.prot }}</div><div class="fsk">protected</div></div>
              <div class="fs"><div class="fsv">{{ authScopes.pub }}</div><div class="fsk">public</div></div>
              <div class="fs"><div class="fsv">{{ authScopes.scopes.length }}</div><div class="fsk">scopes</div></div>
            </div>
            <div class="grp-head"><span>Required permissions</span><span class="n">{{ authScopes.scopes.length }}</span></div>
            <div v-for="sc in authScopes.scopes" :key="sc.perm" class="ep">
              <span class="ep-glyph auth"><Lock :size="11" :stroke-width="2" /></span>
              <div class="ep-main"><div class="ep-name">{{ sc.perm }}</div></div>
              <div class="ep-right"><div class="ep-p50">{{ sc.count }} ep</div></div>
            </div>
            <div v-if="!authScopes.scopes.length" class="insp-pad">
              <div class="ov-hint"><Info :size="15" :stroke-width="2" />{{ authScopes.pub }} public · {{ authScopes.prot }} require a session. No fine-grained scopes.</div>
            </div>
          </template>

          <!-- Traces -->
          <template v-else-if="tab === 'Traces'">
            <div v-for="(tr, ti) in traces" :key="ti" class="ep">
              <span class="trace-st" :class="'s' + String(tr.status).charAt(0)">{{ tr.status }}</span>
              <div class="ep-main"><div class="ep-name"><span class="verb">{{ tr.method }} </span>{{ tr.op }}</div></div>
              <div class="ep-right"><div class="ep-p50">{{ tr.ms }}ms</div><div class="trace-ago">{{ tr.ago }} ago</div></div>
            </div>
            <div v-if="!traces.length" class="insp-pad">
              <div class="ov-hint"><Info :size="15" :stroke-width="2" />No requests captured for this module yet.</div>
            </div>
          </template>

          <!-- Crons -->
          <template v-else-if="tab === 'Crons'">
            <div v-for="(c, ci) in crons" :key="ci" class="ep">
              <span class="ep-glyph" :class="c.kind"><Gauge :size="11" :stroke-width="2" /></span>
              <div class="ep-main"><div class="ep-name">{{ c.name }}</div><span class="ep-auth"><Info :size="10" :stroke-width="2" />{{ c.schedule }}</span></div>
              <div class="ep-right"><div class="ep-p50">{{ c.last }}</div></div>
            </div>
          </template>

          <!-- Limits -->
          <template v-else-if="tab === 'Limits' && limits">
            <div class="facet-sum">
              <div class="fs"><div class="fsv">{{ limits.rpm }}</div><div class="fsk">rpm</div></div>
              <div class="fs"><div class="fsv">{{ limits.burst }}</div><div class="fsk">burst</div></div>
              <div class="fs"><div class="fsv">{{ limits.pct }}<small>%</small></div><div class="fsk">used</div></div>
            </div>
            <div class="insp-pad">
              <div class="limit-bar"><i :style="{ width: limits.pct + '%' }"></i></div>
              <div class="ov-hint"><Gauge :size="15" :stroke-width="2" />Token-bucket — {{ limits.used }}/{{ limits.rpm }} req this minute, burst {{ limits.burst }}.</div>
            </div>
          </template>
        </div>
      </template>

      <div v-else class="insp-scroll">
        <div class="insp-overview"><div class="ov-hint"><Info :size="15" :stroke-width="2" />This is a {{ t.kindLabel || t.kind }} resource. {{ t.desc }}.</div></div>
      </div>
    </template>

    <!-- Empty selection: topology overview -->
    <div v-else class="insp-overview">
      <div class="ov-title">{{ appName }} topology</div>
      <div class="ov-sub">Auto-generated from the Nexus registry — every module, endpoint, and resource the framework wired up at boot.</div>
      <div class="ov-grid">
        <div class="ov-card"><div class="k">Services</div><div class="v">{{ totals.services }}</div></div>
        <div class="ov-card"><div class="k">Endpoints</div><div class="v">{{ totals.eps }}</div></div>
        <div class="ov-card"><div class="k">Live</div><div class="v">{{ totals.rps }}<small>/s</small></div></div>
        <div class="ov-card"><div class="k">Errors</div><div class="v">{{ totals.errs }}</div></div>
      </div>
      <div class="ov-hint"><Info :size="15" :stroke-width="2" />Select any node to inspect its endpoints, auth scopes, and live latency.</div>
    </div>
  </aside>
</template>

<style scoped>
.inspector {
  width: 380px;
  flex: none;
  background: var(--surface);
  border-left: 1px solid var(--line);
  display: flex;
  flex-direction: column;
  min-height: 0;
  box-shadow: -8px 0 30px rgba(0, 0, 0, .12);
  font-family: var(--font-sans);
}
:root[data-theme="light"] .inspector { box-shadow: -8px 0 24px rgba(16, 24, 40, .04); }

.insp-head { padding: 18px 18px 14px; border-bottom: 1px solid var(--line); }
.insp-eyebrow {
  font-size: 10.5px; font-weight: 650; letter-spacing: .1em; text-transform: uppercase;
  color: var(--ink-3); display: flex; align-items: center; justify-content: space-between;
}
.insp-close {
  border: none; background: none; color: var(--ink-3); cursor: pointer; padding: 3px; border-radius: 6px;
  display: grid; place-items: center;
}
.insp-close:hover { background: var(--surface-3); color: var(--ink); }
.insp-title { display: flex; align-items: center; gap: 11px; margin-top: 12px; }
.insp-title-text { min-width: 0; }
.insp-name { font-family: var(--font-mono); font-weight: 600; font-size: 17px; color: var(--ink); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.insp-desc { font-size: 12.5px; color: var(--ink-2); margin-top: 2px; }

.insp-stats { display: grid; grid-template-columns: repeat(3, 1fr); gap: 1px; background: var(--line); border-bottom: 1px solid var(--line); }
.stat { background: var(--surface); padding: 12px 14px; }
.stat .k { font-size: 10px; font-weight: 600; letter-spacing: .06em; text-transform: uppercase; color: var(--ink-3); }
.stat .v { font-family: var(--font-mono); font-size: 17px; font-weight: 600; color: var(--ink); margin-top: 3px; }
.stat .v.warn { color: var(--warn); }
.stat .v.err { color: var(--err); }
.stat .v small { font-size: 11px; color: var(--ink-3); font-weight: 500; }

.insp-tabs {
  display: flex; gap: 2px; padding: 8px 12px; border-bottom: 1px solid var(--line);
  background: var(--surface); overflow-x: auto;
}
.insp-tab {
  font-family: inherit; font-size: 12px; font-weight: 520; color: var(--ink-3);
  border: none; background: none; padding: 6px 11px; border-radius: 7px; cursor: pointer;
  white-space: nowrap; transition: all var(--speed) var(--ease);
}
.insp-tab:hover { color: var(--ink); background: var(--surface-3); }
.insp-tab.is-active { color: var(--ink); background: var(--surface-3); font-weight: 580; }

.insp-scroll { flex: 1; overflow-y: auto; padding: 6px 0 20px; min-height: 0; }
.insp-scroll::-webkit-scrollbar { width: 9px; }
.insp-scroll::-webkit-scrollbar-thumb { background: var(--line-strong); border-radius: 9px; border: 3px solid var(--surface); }

.grp-head {
  display: flex; align-items: center; gap: 8px; padding: 16px 18px 7px;
  font-size: 10.5px; font-weight: 650; letter-spacing: .08em; text-transform: uppercase; color: var(--ink-3);
}
.grp-head .n { margin-left: auto; font-family: var(--font-mono); color: var(--ink-3); letter-spacing: 0; }

.tdot {
  font-family: var(--font-mono); font-size: 9.5px; font-weight: 700; color: #fff;
  height: 16px; min-width: 16px; padding: 0 6px; border-radius: 4px;
  display: inline-flex; align-items: center; justify-content: center; letter-spacing: .02em;
}
.tdot.rest { background: var(--rest); }
.tdot.query { background: var(--query); }
.tdot.mutation { background: var(--mutation); }
.tdot.ws { background: var(--ws); }

.ep {
  display: flex; align-items: center; gap: 11px; padding: 9px 18px; cursor: pointer;
  border-left: 2px solid transparent; transition: background var(--speed) var(--ease);
}
.ep:hover { background: var(--surface-2); border-left-color: var(--accent); }
.ep-glyph {
  font-family: var(--font-mono); font-size: 9.5px; font-weight: 700; color: #fff;
  height: 17px; min-width: 17px; padding: 0 5px; border-radius: 5px;
  display: inline-flex; align-items: center; justify-content: center; flex: none;
}
.ep-glyph.rest { background: var(--rest); }
.ep-glyph.query { background: var(--query); }
.ep-glyph.mutation { background: var(--mutation); }
.ep-glyph.ws { background: var(--ws); }
.ep-glyph.worker { background: var(--worker); }
.ep-glyph.cron { background: var(--cron); }
.ep-glyph.auth { background: var(--authc); }
.ep-glyph svg { width: 11px; height: 11px; }

.ep-main { min-width: 0; flex: 1; }
.ep-name {
  font-family: var(--font-mono); font-size: 12.5px; color: var(--ink); font-weight: 500;
  white-space: nowrap; overflow: hidden; text-overflow: ellipsis;
}
.ep-name .verb { color: var(--ink-3); }
.ep-auth {
  display: inline-flex; align-items: center; gap: 4px; margin-top: 3px;
  font-family: var(--font-mono); font-size: 10px; font-weight: 500; color: var(--authc);
}
.ep-auth.public { color: var(--ink-3); }
.ep-right { text-align: right; flex: none; display: flex; flex-direction: column; align-items: flex-end; }
.ep-p50 { font-family: var(--font-mono); font-size: 11px; color: var(--ink-2); font-weight: 560; }
.ep-err {
  font-family: var(--font-mono); font-size: 10px; color: var(--err); font-weight: 600;
  display: inline-flex; align-items: center; gap: 3px; margin-top: 2px;
  background: none; border: none; padding: 0; cursor: pointer;
}
.ep-err.zero { color: var(--ink-3); opacity: .55; cursor: default; }

.facet-sum { display: grid; grid-template-columns: repeat(3, 1fr); gap: 1px; background: var(--line); border-bottom: 1px solid var(--line); }
.facet-sum .fs { background: var(--surface); padding: 12px 14px; }
.fsv { font-family: var(--font-mono); font-size: 18px; font-weight: 600; color: var(--ink); }
.fsv small { font-size: 11px; color: var(--ink-3); }
.fsk { font-size: 10px; font-weight: 600; letter-spacing: .05em; text-transform: uppercase; color: var(--ink-3); margin-top: 2px; }

.trace-st {
  font-family: var(--font-mono); font-size: 10px; font-weight: 700;
  height: 17px; min-width: 30px; padding: 0 5px; border-radius: 5px; flex: none;
  display: inline-flex; align-items: center; justify-content: center;
}
.trace-st.s2 { color: var(--rest); background: color-mix(in srgb, var(--rest) 15%, transparent); }
.trace-st.s4 { color: var(--warn); background: var(--warn-soft); }
.trace-st.s5 { color: var(--err); background: var(--err-soft); }
.trace-ago { font-family: var(--font-mono); font-size: 10px; color: var(--ink-3); margin-top: 2px; }

.insp-pad { padding: 14px 18px; }
.limit-bar { height: 8px; border-radius: var(--r-pill); background: var(--surface-3); border: 1px solid var(--line); overflow: hidden; margin-bottom: 12px; }
.limit-bar i { display: block; height: 100%; background: linear-gradient(90deg, var(--accent-2), var(--accent)); border-radius: var(--r-pill); transition: width var(--speed) var(--ease); }

.insp-overview { padding: 18px; }
.ov-title { font-size: 13px; font-weight: 600; color: var(--ink); margin-bottom: 4px; }
.ov-sub { font-size: 12px; color: var(--ink-2); line-height: 1.55; margin-bottom: 16px; }
.ov-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 9px; margin-bottom: 18px; }
.ov-card { background: var(--surface-2); border: 1px solid var(--line); border-radius: var(--r-sm); padding: 12px 13px; }
.ov-card .k { font-size: 10px; font-weight: 600; letter-spacing: .06em; text-transform: uppercase; color: var(--ink-3); }
.ov-card .v { font-family: var(--font-mono); font-size: 21px; font-weight: 600; color: var(--ink); margin-top: 4px; }
.ov-card .v small { font-size: 12px; color: var(--ink-3); }
.ov-hint {
  font-size: 11.5px; color: var(--ink-3); display: flex; align-items: center; gap: 7px;
  padding: 11px 12px; background: var(--surface-2); border: 1px solid var(--line); border-radius: var(--r-sm);
}
.ov-hint svg { flex: none; color: var(--accent); }
</style>
