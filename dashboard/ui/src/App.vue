<script setup>
import { ref, onMounted } from 'vue'
import { Network, Plug, Activity, Box } from 'lucide-vue-next'
import Architecture from './views/Architecture.vue'
import Endpoints from './views/Endpoints.vue'
import Traces from './views/Traces.vue'
import { fetchConfig } from './lib/api.js'

const tab = ref('architecture')
const brand = ref('Nexus')

const tabs = [
  { id: 'architecture', label: 'Architecture', icon: Network },
  { id: 'endpoints', label: 'Endpoints', icon: Plug },
  { id: 'traces', label: 'Traces', icon: Activity }
]

onMounted(async () => {
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
