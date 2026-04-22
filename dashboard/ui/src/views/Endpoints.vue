<script setup>
import { ref, onMounted, computed } from 'vue'
import { Search, ChevronRight, Plug, Shield, Layers } from 'lucide-vue-next'
import { fetchEndpoints, fetchMiddlewares } from '../lib/api.js'
import RestTester from '../components/RestTester.vue'
import GraphQLTester from '../components/GraphQLTester.vue'
import WsTester from '../components/WsTester.vue'

const endpoints = ref([])
const selected = ref(null)
const filter = ref('')
const middlewares = ref({})  // name -> { kind, description }

async function load() {
  const [ep, mw] = await Promise.all([fetchEndpoints(), fetchMiddlewares()])
  endpoints.value = ep.endpoints || []
  const map = {}
  for (const m of mw.middlewares || []) map[m.name] = m
  middlewares.value = map
}
onMounted(load)

const grouped = computed(() => {
  const f = filter.value.toLowerCase()
  const list = f
    ? endpoints.value.filter(e => (e.Name + e.Service + e.Path).toLowerCase().includes(f))
    : endpoints.value
  const byService = {}
  for (const e of list) (byService[e.Service] ||= []).push(e)
  return byService
})

function tagLabel(e) {
  if (e.Transport === 'rest') return e.Method
  if (e.Transport === 'graphql') return e.Method
  return 'WS'
}

function mwKind(name) {
  const m = middlewares.value[name]
  if (!m) return 'unknown'
  return m.kind === 'builtin' ? 'builtin' : 'custom'
}
function mwDesc(name) {
  const m = middlewares.value[name]
  return m?.description || ''
}
</script>

<template>
  <div class="endpoints">
    <aside>
      <div class="search">
        <Search :size="14" :stroke-width="2" class="search-icon" />
        <input v-model="filter" placeholder="Search endpoints" />
      </div>
      <div class="list">
        <div v-for="(list, svc) in grouped" :key="svc" class="service-group">
          <div class="svc-name">{{ svc }}</div>
          <button
            v-for="e in list"
            :key="e.Service + ':' + e.Name"
            class="endpoint"
            :class="[
              e.Transport,
              {
                active: selected && selected.Name === e.Name && selected.Service === e.Service,
                deprecated: e.Deprecated
              }
            ]"
            @click="selected = e"
          >
            <span class="tag">{{ tagLabel(e) }}</span>
            <span class="ep-name">{{ e.Path || e.Name }}</span>
            <ChevronRight :size="14" class="chev" />
          </button>
        </div>
        <div v-if="!Object.keys(grouped).length" class="empty-list">No endpoints match.</div>
      </div>
    </aside>
    <section class="detail">
      <div v-if="!selected" class="empty">
        <div class="empty-icon"><Plug :size="28" :stroke-width="1.5" /></div>
        <div class="empty-text">Select an endpoint to test it.</div>
      </div>
      <div v-else class="detail-body">
        <header class="detail-head">
          <span class="tag" :class="selected.Transport">{{ tagLabel(selected) }}</span>
          <h2>{{ selected.Name }}</h2>
          <span v-if="selected.Deprecated" class="deprecated-badge" :title="selected.DeprecationReason">
            deprecated
          </span>
          <div v-if="selected.Description" class="desc">{{ selected.Description }}</div>
          <div v-if="selected.Deprecated && selected.DeprecationReason" class="deprecation-note">
            {{ selected.DeprecationReason }}
          </div>
        </header>

        <div v-if="selected.Middleware && selected.Middleware.length" class="mw-row">
          <span class="mw-label"><Shield :size="12" :stroke-width="2" /> Middleware</span>
          <div class="mw-chain">
            <span
              v-for="m in selected.Middleware"
              :key="m"
              :class="['mw-chip', mwKind(m)]"
              :title="mwDesc(m) || (mwKind(m) === 'unknown' ? 'Not registered in any middleware' : '')"
            >
              <Layers v-if="mwKind(m) === 'builtin'" :size="10" :stroke-width="2.5" />
              {{ m }}
            </span>
          </div>
        </div>

        <RestTester v-if="selected.Transport === 'rest'" :endpoint="selected" :key="selected.Service + selected.Name" />
        <GraphQLTester v-else-if="selected.Transport === 'graphql'" :endpoint="selected" :key="selected.Service + selected.Name" />
        <WsTester v-else-if="selected.Transport === 'websocket'" :endpoint="selected" :key="selected.Service + selected.Name" />
      </div>
    </section>
  </div>
</template>

<style scoped>
.endpoints { display: grid; grid-template-columns: 320px 1fr; height: 100%; background: var(--bg); }
aside { border-right: 1px solid var(--border); display: flex; flex-direction: column; overflow: hidden; background: var(--bg); }
.search { position: relative; padding: 12px; border-bottom: 1px solid var(--border); }
.search-icon { position: absolute; left: 24px; top: 50%; transform: translateY(-50%); color: var(--text-dim); pointer-events: none; }
.search input { padding-left: 34px; background: var(--bg-subtle); }
.list { flex: 1; overflow-y: auto; padding: 8px 8px 16px; }
.service-group { margin-top: 12px; }
.service-group:first-child { margin-top: 4px; }
.svc-name { padding: 4px 10px 6px; font-size: 10.5px; text-transform: uppercase; color: var(--text-dim); letter-spacing: 0.08em; font-weight: 600; }
.endpoint { display: flex; align-items: center; gap: 8px; width: 100%; text-align: left; padding: 7px 10px; border: 1px solid transparent; background: transparent; border-radius: var(--radius); font-family: var(--font-mono); font-size: 12px; margin-bottom: 1px; color: var(--text); font-weight: 500; }
.endpoint:hover { background: var(--bg-hover); }
.endpoint.active { background: var(--bg-active); border-color: transparent; color: var(--accent); }
.ep-name { flex: 1; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.chev { color: var(--text-dim); opacity: 0; transition: opacity 120ms; }
.endpoint:hover .chev, .endpoint.active .chev { opacity: 1; }
.tag { font-size: 10px; padding: 2px 6px; border-radius: 3px; font-weight: 700; flex-shrink: 0; letter-spacing: 0.02em; }
.endpoint.rest .tag, .detail-head .tag.rest { background: var(--rest-soft); color: var(--rest); }
.endpoint.graphql .tag, .detail-head .tag.graphql { background: var(--graphql-soft); color: var(--graphql); }
.endpoint.websocket .tag, .detail-head .tag.websocket { background: var(--ws-soft); color: var(--ws); }
.empty-list { color: var(--text-dim); padding: 20px; text-align: center; font-size: 12px; }

.detail { overflow-y: auto; padding: 28px 32px; background: var(--bg-subtle); }
.detail-body { max-width: 920px; margin: 0 auto; }
.empty { height: 100%; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 12px; color: var(--text-dim); }
.empty-icon { color: var(--text-dim); opacity: 0.5; }
.empty-text { font-size: 13px; }

.detail-head { margin-bottom: 16px; display: flex; align-items: center; gap: 12px; flex-wrap: wrap; padding-bottom: 16px; border-bottom: 1px solid var(--border); }
.detail-head h2 { margin: 0; font-size: 16px; font-weight: 600; font-family: var(--font-mono); }
.detail-head .desc { width: 100%; color: var(--text-dim); font-size: 13px; }
.deprecated-badge {
  font-size: 10px;
  padding: 2px 8px;
  border-radius: 10px;
  font-weight: 700;
  letter-spacing: 0.04em;
  text-transform: uppercase;
  background: #fef3c7;
  color: #92400e;
  border: 1px solid #fde68a;
}
.deprecation-note {
  width: 100%;
  color: #92400e;
  font-size: 12px;
  background: #fffbeb;
  border: 1px solid #fde68a;
  border-radius: var(--radius-sm);
  padding: 6px 10px;
  margin-top: 4px;
}
.endpoint.deprecated .ep-name {
  text-decoration: line-through;
  text-decoration-color: var(--text-dim);
  color: var(--text-dim);
}

.mw-row {
  display: flex;
  align-items: flex-start;
  gap: 14px;
  margin-bottom: 24px;
  padding-bottom: 16px;
  border-bottom: 1px solid var(--border);
}
.mw-label {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 10.5px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  font-weight: 600;
  color: var(--text-muted);
  padding-top: 3px;
  flex-shrink: 0;
  width: 110px;
}
.mw-chain { display: flex; flex-wrap: wrap; gap: 6px; }
.mw-chip {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-family: var(--font-mono);
  font-size: 11.5px;
  padding: 3px 9px;
  border-radius: 12px;
  font-weight: 600;
  border: 1px solid transparent;
}
.mw-chip.builtin { background: #eef2ff; color: #4338ca; border-color: #c7d2fe; }
.mw-chip.custom  { background: var(--bg-hover); color: var(--text); border-color: var(--border); }
.mw-chip.unknown { background: #fef3c7; color: #b45309; border-color: #fde68a; }
</style>
