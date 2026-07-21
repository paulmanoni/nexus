<script setup>
import { ref, computed, watch } from 'vue'
import { Info, Lock, ShieldCheck, Globe, AlertTriangle, Gauge, X, Play, Pause, RotateCcw } from 'lucide-vue-next'
import * as lucide from 'lucide-vue-next'
import CategoryIcon from './CategoryIcon.vue'

// Inspector — the persistent right-hand panel of the redesigned dashboard.
// Replaces the slide-over drawer as the always-on detail surface. Pure view:
// Architecture.vue resolves the selected canvas node into a rich `target`
// (modules get endpoints/auth/traces/crons/limits; resources/workers/crons/
// clients get their own detail) plus the global middleware chain, GraphQL
// cache, and auth summary for the no-selection overview — all live against
// the /__nexus/live WS snapshot, nothing polled. Endpoint clicks emit
// `open-op`; cron actions emit `cron`.
const props = defineProps({
  target: { type: Object, default: null },
  totals: { type: Object, default: () => ({ services: 0, eps: 0, rps: 0, errs: 0 }) },
  appName: { type: String, default: 'Nexus' },
  globalMiddleware: { type: Array, default: () => [] },
  graphqlCache: { type: Array, default: () => [] },
  authSummary: { type: Object, default: null },
})
const emit = defineEmits(['open-op', 'open-errors', 'cron', 'pick', 'close'])

const t = computed(() => props.target)
const isModule = computed(() => t.value && t.value.kind === 'module')
const hasEndpoints = computed(() => isModule.value && Array.isArray(t.value.endpoints))

// Active facet tab, reset to Endpoints whenever the selection changes.
const tab = ref('Endpoints')
watch(() => props.target && props.target.id, () => { tab.value = 'Endpoints' })

const tabs = computed(() => {
  if (!hasEndpoints.value) return []
  const out = ['Endpoints', 'Auth', 'Traces']
  if ((t.value.crons || []).length) out.push('Crons')
  if (t.value.limited) out.push('Limits')
  return out
})

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

// epIcon returns the lucide component for an endpoint's custom dashboard icon
// (set by nexus.WithIcon, e.g. an extension branding endpoints it registered
// through a custom decorator), or null to fall back to the text glyph.
function epIcon(e) {
  const tags = e && e.endpoint && e.endpoint.Tags
  const name = tags && tags['dashboard.icon']
  if (!name) return null
  const pascal = name.split('-').map(s => s.charAt(0).toUpperCase() + s.slice(1)).join('')
  return lucide[pascal] || null
}
// proxyOf returns the upstream URL when an endpoint is reverse-proxied
// (extension/proxy sets registry.ProxyTag), or '' for a native handler — drives
// the per-row PROXY pill so proxied routes are distinguishable at a glance.
function proxyOf(e) {
  const tags = e && e.endpoint && e.endpoint.Tags
  return (tags && tags['dashboard.proxy']) || ''
}
function authLabel(a) {
  if (!a) return { txt: 'public', cls: 'public', perm: false }
  if (a === 'required') return { txt: 'auth required', cls: '', perm: false }
  return { txt: a.replace('requires:', ''), cls: '', perm: true }
}
const eyebrow = computed(() => {
  if (!t.value) return ''
  return t.value.kind === 'module' ? 'Module' : (t.value.kindLabel || t.value.kind)
})

function fmtTime(s) { if (!s) return ''; try { return new Date(s).toLocaleTimeString() } catch { return '' } }
function fmtMs(ms) { return ms == null ? '' : ms + 'ms' }

// Worker / cron live state → status pill class.
function workerState(s) {
  if (s === 'failed') return 'err'
  if (s === 'running') return 'ok'
  if (s === 'stopped') return 'idle'
  return 'warn'
}
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

      <!-- ───────────── MODULE ───────────── -->
      <template v-if="t.kind === 'module'">
        <div class="insp-stats">
          <div class="stat"><div class="k">Endpoints</div><div class="v">{{ t.count != null ? t.count : '—' }}</div></div>
          <div class="stat"><div class="k">Throughput</div><div class="v">{{ t.rps }}<small>/s</small></div></div>
          <div class="stat"><div class="k">Error rate</div><div class="v" :class="{ warn: t.errPct > 0 && t.errPct < 2, err: t.errPct >= 2 }">{{ t.errPct }}<small>%</small></div></div>
        </div>

        <div class="insp-tabs">
          <button v-for="tb in tabs" :key="tb" class="insp-tab" :class="{ 'is-active': tab === tb }" @click="tab = tb">{{ tb }}</button>
        </div>
        <div class="insp-scroll">
          <template v-if="tab === 'Endpoints'">
            <div v-for="g in epGroups" :key="g.key">
              <div class="grp-head"><span class="tdot" :class="g.key">{{ g.label }}</span><span class="n">{{ g.items.length }}</span></div>
              <div v-for="(e, ei) in g.items" :key="ei" class="ep" @click="e.endpoint && emit('open-op', e.endpoint)">
                <span class="ep-glyph" :class="e.kind">
                  <component v-if="epIcon(e)" :is="epIcon(e)" :size="11" :stroke-width="2.25" />
                  <template v-else>{{ glyph(e.kind) }}</template>
                </span>
                <div class="ep-main">
                  <div class="ep-name"><span v-if="e.method" class="verb">{{ e.method }} </span>{{ e.name }}<span v-if="proxyOf(e)" class="ep-proxy" :title="'reverse-proxied → ' + proxyOf(e)">proxy</span></div>
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
            <div v-if="!authScopes.scopes.length" class="insp-pad"><div class="ov-hint"><Info :size="15" :stroke-width="2" />{{ authScopes.pub }} public · {{ authScopes.prot }} require a session. No fine-grained scopes.</div></div>
          </template>

          <template v-else-if="tab === 'Traces'">
            <div v-for="(tr, ti) in traces" :key="ti" class="ep">
              <span class="trace-st" :class="'s' + String(tr.status).charAt(0)">{{ tr.status }}</span>
              <div class="ep-main"><div class="ep-name"><span class="verb">{{ tr.method }} </span>{{ tr.op }}</div></div>
              <div class="ep-right"><div class="ep-p50">{{ tr.ms }}ms</div><div class="trace-ago">{{ tr.ago }} ago</div></div>
            </div>
            <div v-if="!traces.length" class="insp-pad"><div class="ov-hint"><Info :size="15" :stroke-width="2" />No requests captured for this module yet.</div></div>
          </template>

          <template v-else-if="tab === 'Crons'">
            <div v-for="(c, ci) in crons" :key="ci" class="ep">
              <span class="ep-glyph" :class="c.kind"><Gauge :size="11" :stroke-width="2" /></span>
              <div class="ep-main"><div class="ep-name">{{ c.name }}</div><span class="ep-auth"><Info :size="10" :stroke-width="2" />{{ c.schedule }}</span></div>
              <div class="ep-right"><div class="ep-p50">{{ c.last }}</div></div>
            </div>
          </template>

          <template v-else-if="tab === 'Limits' && limits">
            <div class="facet-sum">
              <div class="fs"><div class="fsv">{{ limits.count }}</div><div class="fsk">limited</div></div>
              <div class="fs"><div class="fsv">{{ limits.maxRpm }}</div><div class="fsk">max rpm</div></div>
              <div class="fs"><div class="fsv">{{ limits.overridden ? 'yes' : 'no' }}</div><div class="fsk">override</div></div>
            </div>
            <div class="grp-head"><span>Rate-limited endpoints</span><span class="n">{{ limits.count }}</span></div>
            <div v-for="(l, li) in limits.ops" :key="li" class="ep">
              <span class="ep-glyph cron"><Gauge :size="11" :stroke-width="2" /></span>
              <div class="ep-main">
                <div class="ep-name">{{ l.op }}</div>
                <span class="ep-auth"><Info :size="10" :stroke-width="2" />{{ l.rpm }}/min<template v-if="l.burst"> · burst {{ l.burst }}</template><template v-if="l.perIP"> · per-IP</template></span>
              </div>
              <div class="ep-right"><div class="ep-p50" :class="{ ov: l.overridden }">{{ l.overridden ? 'override' : 'declared' }}</div></div>
            </div>
          </template>
        </div>
      </template>

      <!-- ───────────── RESOURCE ───────────── -->
      <template v-else-if="t.kind === 'resource' && t.resource">
        <div class="insp-scroll">
          <div class="insp-pad">
            <span class="rpill" :class="t.resource.healthy ? 'ok' : 'err'"><span class="dot"></span>{{ t.resource.healthy ? 'healthy' : 'down' }}</span>
          </div>
          <template v-if="Object.keys(t.resource.details).length">
            <div class="grp-head"><span>Details</span></div>
            <div class="kvs">
              <div v-for="(v, k) in t.resource.details" :key="k" class="kv"><span class="kk">{{ k }}</span><span class="kvv">{{ v }}</span></div>
            </div>
          </template>
          <template v-if="t.resource.attachedTo.length">
            <div class="grp-head"><span>Used by</span><span class="n">{{ t.resource.attachedTo.length }}</span></div>
            <div class="chips-pad"><span v-for="s in t.resource.attachedTo" :key="s" class="dchip">{{ s }}</span></div>
          </template>
          <template v-if="t.resource.dependsOn.length">
            <div class="grp-head"><span>Depends on</span></div>
            <div class="chips-pad"><span v-for="s in t.resource.dependsOn" :key="s" class="dchip" @click="emit('pick', 'res:' + s)">{{ s }}</span></div>
          </template>
        </div>
      </template>

      <!-- ───────────── WORKER ───────────── -->
      <template v-else-if="t.kind === 'worker' && t.worker">
        <div class="insp-scroll">
          <div class="insp-pad">
            <span class="rpill" :class="workerState(t.worker.status)"><span class="dot"></span>{{ t.worker.status }}</span>
          </div>
          <div v-if="t.worker.lastError" class="insp-pad">
            <div class="ov-hint err-hint"><AlertTriangle :size="15" :stroke-width="2" />{{ t.worker.lastError }}</div>
          </div>
          <template v-if="t.worker.resourceDeps.length">
            <div class="grp-head"><span>Resources</span></div>
            <div class="chips-pad"><span v-for="r in t.worker.resourceDeps" :key="r" class="dchip" @click="emit('pick', 'res:' + r)">{{ r }}</span></div>
          </template>
          <template v-if="t.worker.serviceDeps.length">
            <div class="grp-head"><span>Calls services</span></div>
            <div class="chips-pad"><span v-for="s in t.worker.serviceDeps" :key="s" class="dchip">{{ s }}</span></div>
          </template>
        </div>
      </template>

      <!-- ───────────── CRON ───────────── -->
      <template v-else-if="t.kind === 'cron' && t.cron">
        <div class="insp-scroll">
          <div class="insp-pad cron-head">
            <span class="rpill" :class="t.cron.paused ? 'idle' : (t.cron.lastRun && !t.cron.lastRun.success ? 'err' : 'ok')">
              <span class="dot"></span>{{ t.cron.paused ? 'paused' : (t.cron.running ? 'running' : 'scheduled') }}
            </span>
            <span class="cron-sched">{{ t.cron.schedule }}</span>
          </div>
          <div class="cron-actions">
            <button @click="emit('cron', { name: t.label, action: 'trigger' })"><Play :size="13" :stroke-width="2" />Trigger</button>
            <button v-if="t.cron.paused" @click="emit('cron', { name: t.label, action: 'resume' })"><RotateCcw :size="13" :stroke-width="2" />Resume</button>
            <button v-else @click="emit('cron', { name: t.label, action: 'pause' })"><Pause :size="13" :stroke-width="2" />Pause</button>
          </div>
          <div class="kvs">
            <div v-if="t.cron.nextRun" class="kv"><span class="kk">next run</span><span class="kvv">{{ fmtTime(t.cron.nextRun) }}</span></div>
            <div v-if="t.cron.lastRun" class="kv"><span class="kk">last run</span><span class="kvv">{{ fmtTime(t.cron.lastRun.started) }} · {{ fmtMs(t.cron.lastRun.durationMs) }}</span></div>
          </div>
          <div v-if="t.cron.lastRun && t.cron.lastRun.error" class="insp-pad"><div class="ov-hint err-hint"><AlertTriangle :size="15" :stroke-width="2" />{{ t.cron.lastRun.error }}</div></div>
          <template v-if="t.cron.history.length">
            <div class="grp-head"><span>Recent runs</span><span class="n">{{ t.cron.history.length }}</span></div>
            <div v-for="(h, hi) in t.cron.history" :key="hi" class="ep">
              <span class="trace-st" :class="h.success ? 's2' : 's5'">{{ h.success ? 'OK' : 'ERR' }}</span>
              <div class="ep-main"><div class="ep-name">{{ fmtTime(h.started) }}<span v-if="h.manual" class="manual"> · manual</span></div></div>
              <div class="ep-right"><div class="ep-p50">{{ fmtMs(h.durationMs) }}</div></div>
            </div>
          </template>
        </div>
      </template>

      <!-- ───────────── CLIENTS / generic ───────────── -->
      <template v-else>
        <div class="insp-scroll">
          <div v-if="t.kind === 'clients'" class="insp-stats one">
            <div class="stat"><div class="k">Live throughput</div><div class="v">{{ t.rps }}<small>/s</small></div></div>
          </div>
          <div class="insp-pad"><div class="ov-hint"><Info :size="15" :stroke-width="2" />{{ t.desc || ('This is a ' + (t.kindLabel || t.kind) + '.') }}</div></div>
        </div>
      </template>
    </template>

    <!-- ───────────── OVERVIEW (no selection) ───────────── -->
    <div v-else class="insp-scroll">
      <div class="insp-overview">
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

      <template v-if="globalMiddleware.length">
        <div class="grp-head"><span>Global middleware</span><span class="n">{{ globalMiddleware.length }}</span></div>
        <div class="mw-chain">
          <template v-for="(mw, mi) in globalMiddleware" :key="mi">
            <span class="mw-chip">{{ mw }}</span>
            <span v-if="mi < globalMiddleware.length - 1" class="mw-arrow">→</span>
          </template>
        </div>
      </template>

      <template v-if="graphqlCache.length">
        <div class="grp-head"><span>GraphQL document cache</span><span class="n">{{ graphqlCache.length }}</span></div>
        <div v-for="(m, mi) in graphqlCache" :key="mi" class="ep">
          <span class="ep-glyph mutation">GQL</span>
          <div class="ep-main"><div class="ep-name">{{ m.path }}</div><span class="ep-auth"><Info :size="10" :stroke-width="2" />{{ m.size }}/{{ m.capacity }} cached · {{ m.hits }} hits</span></div>
          <div class="ep-right"><div class="ep-p50">{{ m.hitRate }}%</div></div>
        </div>
      </template>

      <template v-if="authSummary">
        <div class="grp-head"><span>Auth</span></div>
        <div class="insp-pad">
          <div class="ov-hint"><ShieldCheck :size="15" :stroke-width="2" />{{ authSummary.identities.length }} cached {{ authSummary.identities.length === 1 ? 'identity' : 'identities' }} · caching {{ authSummary.cachingEnabled ? 'on' : 'off' }}.</div>
        </div>
      </template>
    </div>
  </aside>
</template>

<style scoped>
.inspector {
  width: 380px; flex: none; background: var(--surface); border-left: 1px solid var(--line);
  display: flex; flex-direction: column; min-height: 0;
  box-shadow: -8px 0 30px rgba(0, 0, 0, .12); font-family: var(--font-sans);
}
:root[data-theme="light"] .inspector { box-shadow: -8px 0 24px rgba(16, 24, 40, .04); }

.insp-head { padding: 18px 18px 14px; border-bottom: 1px solid var(--line); }
.insp-eyebrow {
  font-size: 10.5px; font-weight: 650; letter-spacing: .1em; text-transform: uppercase;
  color: var(--ink-3); display: flex; align-items: center; justify-content: space-between;
}
.insp-close { border: none; background: none; color: var(--ink-3); cursor: pointer; padding: 3px; border-radius: 6px; display: grid; place-items: center; }
.insp-close:hover { background: var(--surface-3); color: var(--ink); }
.insp-title { display: flex; align-items: center; gap: 11px; margin-top: 12px; }
.insp-title-text { min-width: 0; }
.insp-name { font-family: var(--font-mono); font-weight: 600; font-size: 17px; color: var(--ink); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.insp-desc { font-size: 12.5px; color: var(--ink-2); margin-top: 2px; }

.insp-stats { display: grid; grid-template-columns: repeat(3, 1fr); gap: 1px; background: var(--line); border-bottom: 1px solid var(--line); }
.insp-stats.one { grid-template-columns: 1fr; }
.stat { background: var(--surface); padding: 12px 14px; }
.stat .k { font-size: 10px; font-weight: 600; letter-spacing: .06em; text-transform: uppercase; color: var(--ink-3); }
.stat .v { font-family: var(--font-mono); font-size: 17px; font-weight: 600; color: var(--ink); margin-top: 3px; }
.stat .v.warn { color: var(--warn); }
.stat .v.err { color: var(--err); }
.stat .v small { font-size: 11px; color: var(--ink-3); font-weight: 500; }

.insp-tabs { display: flex; gap: 2px; padding: 8px 12px; border-bottom: 1px solid var(--line); background: var(--surface); overflow-x: auto; }
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

.ep { display: flex; align-items: center; gap: 11px; padding: 9px 18px; cursor: pointer; border-left: 2px solid transparent; transition: background var(--speed) var(--ease); }
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
.ep-name { font-family: var(--font-mono); font-size: 12.5px; color: var(--ink); font-weight: 500; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.ep-name .verb { color: var(--ink-3); }
.ep-name .manual { color: var(--ink-3); }
.ep-name .ep-proxy {
  margin-left: 6px;
  font-family: var(--font-sans, inherit);
  font-size: 8.5px;
  font-weight: 700;
  letter-spacing: 0.05em;
  text-transform: uppercase;
  vertical-align: middle;
  padding: 1px 4px;
  border-radius: 3px;
  color: var(--warn, #f59e0b);
  background: color-mix(in srgb, var(--warn, #f59e0b) 14%, transparent);
  border: 1px solid color-mix(in srgb, var(--warn, #f59e0b) 30%, transparent);
}
.ep-auth { display: inline-flex; align-items: center; gap: 4px; margin-top: 3px; font-family: var(--font-mono); font-size: 10px; font-weight: 500; color: var(--authc); }
.ep-auth.public { color: var(--ink-3); }
.ep-right { text-align: right; flex: none; display: flex; flex-direction: column; align-items: flex-end; }
.ep-p50 { font-family: var(--font-mono); font-size: 11px; color: var(--ink-2); font-weight: 560; }
.ep-p50.ov { color: var(--warn); }
.ep-err { font-family: var(--font-mono); font-size: 10px; color: var(--err); font-weight: 600; display: inline-flex; align-items: center; gap: 3px; margin-top: 2px; background: none; border: none; padding: 0; cursor: pointer; }
.ep-err.zero { color: var(--ink-3); opacity: .55; cursor: default; }

.facet-sum { display: grid; grid-template-columns: repeat(3, 1fr); gap: 1px; background: var(--line); border-bottom: 1px solid var(--line); }
.facet-sum .fs { background: var(--surface); padding: 12px 14px; }
.fsv { font-family: var(--font-mono); font-size: 18px; font-weight: 600; color: var(--ink); }
.fsk { font-size: 10px; font-weight: 600; letter-spacing: .05em; text-transform: uppercase; color: var(--ink-3); margin-top: 2px; }

.trace-st { font-family: var(--font-mono); font-size: 10px; font-weight: 700; height: 17px; min-width: 30px; padding: 0 5px; border-radius: 5px; flex: none; display: inline-flex; align-items: center; justify-content: center; }
.trace-st.s2 { color: var(--rest); background: color-mix(in srgb, var(--rest) 15%, transparent); }
.trace-st.s4 { color: var(--warn); background: var(--warn-soft); }
.trace-st.s5 { color: var(--err); background: var(--err-soft); }
.trace-ago { font-family: var(--font-mono); font-size: 10px; color: var(--ink-3); margin-top: 2px; }

.insp-pad { padding: 14px 18px; }

/* status pill (resource / worker / cron) */
.rpill { display: inline-flex; align-items: center; gap: 6px; font-family: var(--font-mono); font-size: 11px; font-weight: 600; padding: 3px 10px; border-radius: var(--r-pill); }
.rpill .dot { width: 6px; height: 6px; border-radius: 50%; background: currentColor; }
.rpill.ok { color: var(--st-healthy); background: var(--st-healthy-soft); }
.rpill.err { color: var(--err); background: var(--err-soft); }
.rpill.warn { color: var(--warn); background: var(--warn-soft); }
.rpill.idle { color: var(--ink-3); background: var(--surface-3); }

/* key/value detail rows */
.kvs { padding: 4px 18px 8px; display: flex; flex-direction: column; gap: 4px; }
.kv { display: flex; gap: 10px; font-family: var(--font-mono); font-size: 11.5px; }
.kk { color: var(--ink-3); min-width: 84px; }
.kvv { color: var(--ink); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

.chips-pad { padding: 4px 18px 8px; display: flex; flex-wrap: wrap; gap: 6px; }
.dchip { font-family: var(--font-mono); font-size: 10.5px; color: var(--ink-2); background: var(--surface-2); border: 1px solid var(--line); border-radius: var(--r-pill); padding: 2px 9px; cursor: pointer; }
.dchip:hover { border-color: var(--accent-line); color: var(--ink); }

.err-hint { color: var(--err) !important; }
.err-hint :deep(svg) { color: var(--err) !important; }

/* cron */
.cron-head { display: flex; align-items: center; gap: 10px; padding-bottom: 8px; }
.cron-sched { font-family: var(--font-mono); font-size: 12px; color: var(--ink-2); }
.cron-actions { display: flex; gap: 8px; padding: 0 18px 10px; }
.cron-actions button { font-size: 12px; padding: 6px 11px; }

/* middleware chain */
.mw-chain { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; padding: 4px 18px 8px; }
.mw-chip { font-family: var(--font-mono); font-size: 10.5px; color: var(--ink); background: var(--surface-2); border: 1px solid var(--line); border-radius: 6px; padding: 3px 8px; }
.mw-arrow { color: var(--ink-3); font-size: 11px; }

.insp-overview { padding: 18px; }
.ov-title { font-size: 13px; font-weight: 600; color: var(--ink); margin-bottom: 4px; }
.ov-sub { font-size: 12px; color: var(--ink-2); line-height: 1.55; margin-bottom: 16px; }
.ov-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 9px; margin-bottom: 18px; }
.ov-card { background: var(--surface-2); border: 1px solid var(--line); border-radius: var(--r-sm); padding: 12px 13px; }
.ov-card .k { font-size: 10px; font-weight: 600; letter-spacing: .06em; text-transform: uppercase; color: var(--ink-3); }
.ov-card .v { font-family: var(--font-mono); font-size: 21px; font-weight: 600; color: var(--ink); margin-top: 4px; }
.ov-card .v small { font-size: 12px; color: var(--ink-3); }
.ov-hint { font-size: 11.5px; color: var(--ink-3); display: flex; align-items: center; gap: 7px; padding: 11px 12px; background: var(--surface-2); border: 1px solid var(--line); border-radius: var(--r-sm); }
.ov-hint svg { flex: none; color: var(--accent); }
</style>
