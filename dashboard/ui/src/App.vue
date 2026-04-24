<script setup>
import { ref, onMounted, watch } from 'vue'
import { Network, Plug, Activity, Box, Clock, Gauge, ShieldCheck } from 'lucide-vue-next'
import Architecture from './views/Architecture.vue'
import Endpoints from './views/Endpoints.vue'
import Traces from './views/Traces.vue'
import Crons from './views/Crons.vue'
import RateLimits from './views/RateLimits.vue'
import Auth from './views/Auth.vue'
import { fetchConfig } from './lib/api.js'

const tabs = [
  { id: 'architecture', label: 'Architecture', icon: Network },
  { id: 'endpoints', label: 'Endpoints', icon: Plug },
  { id: 'crons', label: 'Crons', icon: Clock },
  { id: 'ratelimits', label: 'Rate limits', icon: Gauge },
  { id: 'auth', label: 'Auth', icon: ShieldCheck },
  { id: 'traces', label: 'Traces', icon: Activity }
]

// Persist the selected tab via the URL ?tab= query param. Shareable,
// bookmarkable, works with browser back/forward without the weight of
// a full router. Defensive validation falls back to the first tab
// when the URL carries an unknown id.
function readTabFromURL() {
  try {
    const id = new URL(window.location.href).searchParams.get('tab')
    if (id && tabs.some(t => t.id === id)) return id
  } catch { /* SSR or weird URL */ }
  return tabs[0].id
}
function writeTabToURL(id) {
  try {
    const url = new URL(window.location.href)
    if (url.searchParams.get('tab') === id) return
    url.searchParams.set('tab', id)
    window.history.replaceState(null, '', url)
  } catch { /* no-op */ }
}
const tab = ref(readTabFromURL())
watch(tab, writeTabToURL)
// Sync in when user navigates with the browser back/forward buttons.
if (typeof window !== 'undefined') {
  window.addEventListener('popstate', () => {
    const id = readTabFromURL()
    if (id !== tab.value) tab.value = id
  })
}

const brand = ref('Nexus')

onMounted(async () => {
  // Make sure the URL always reflects the active tab, even on a fresh
  // visit with no ?tab= param — copy/paste the URL and the receiver
  // lands on the same tab.
  writeTabToURL(tab.value)
  const cfg = await fetchConfig()
  if (cfg && cfg.Name) {
    brand.value = cfg.Name
    document.title = cfg.Name
  }
})
</script>

<template>
  <div class="shell">
    <header>
      <div class="brand">
        <div class="logo"><Box :size="16" :stroke-width="2.5" /></div>
        <span>{{ brand }}</span>
      </div>
      <nav>
        <button
          v-for="t in tabs"
          :key="t.id"
          :class="['tab', { active: tab === t.id }]"
          @click="tab = t.id"
        >
          <component :is="t.icon" :size="15" :stroke-width="2" />
          <span>{{ t.label }}</span>
        </button>
      </nav>
    </header>
    <main>
      <Architecture v-show="tab === 'architecture'" />
      <Endpoints v-show="tab === 'endpoints'" />
      <Crons v-show="tab === 'crons'" />
      <RateLimits v-show="tab === 'ratelimits'" />
      <Auth v-show="tab === 'auth'" />
      <Traces v-show="tab === 'traces'" />
    </main>
  </div>
</template>

<style scoped>
.shell {
  display: flex;
  flex-direction: column;
  height: 100%;
  background: var(--bg);
}
header {
  display: flex;
  align-items: center;
  gap: 24px;
  padding: 0 20px;
  height: 52px;
  border-bottom: 1px solid var(--border);
  background: var(--bg);
  flex-shrink: 0;
}
.brand {
  display: flex;
  align-items: center;
  gap: 10px;
  font-weight: 600;
  font-size: 14px;
  color: var(--text);
  letter-spacing: -0.01em;
}
.logo {
  width: 26px;
  height: 26px;
  background: var(--accent);
  color: white;
  display: grid;
  place-items: center;
  border-radius: 6px;
}
nav {
  display: flex;
  gap: 2px;
  margin-left: 8px;
}
.tab {
  border: 1px solid transparent;
  background: transparent;
  color: var(--text-muted);
  padding: 6px 10px;
  font-weight: 500;
}
.tab:hover { background: var(--bg-hover); border-color: transparent; color: var(--text); }
.tab.active {
  background: var(--bg-active);
  color: var(--accent);
  border-color: transparent;
}
main {
  flex: 1;
  overflow: hidden;
  position: relative;
  background: var(--bg-subtle);
}
main > * { height: 100%; }
</style>
