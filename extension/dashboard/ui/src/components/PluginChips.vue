<script setup>
import { ref, onMounted } from 'vue'
import { Puzzle } from 'lucide-vue-next'
import { fetchPlugins } from '../lib/api.js'

const plugins = ref([])
const open = ref(false)

onMounted(async () => {
  plugins.value = await fetchPlugins()
})
</script>

<template>
  <div
    class="plugin-chips"
    @mouseenter="open = true"
    @mouseleave="open = false"
    v-if="plugins.length"
  >
    <button class="trigger" type="button">
      <Puzzle :size="12" :stroke-width="2.25" />
      <span class="count">{{ plugins.length }}</span>
      <span class="label">plugins</span>
    </button>
    <div class="popover" v-if="open">
      <div class="title">Registered plugins</div>
      <ul>
        <li v-for="p in plugins" :key="p.name">
          <span class="name">{{ p.name }}</span>
          <span class="version" v-if="p.version">v{{ p.version }}</span>
          <span class="badges">
            <span class="badge" v-if="p.hasDashboard" title="contributes dashboard routes">dash</span>
            <span class="badge" v-if="p.hasClient" title="contributes client SDK">sdk</span>
            <span class="badge" v-if="p.namespace" :title="`SDK namespace: nx.${p.namespace}`">nx.{{ p.namespace }}</span>
          </span>
        </li>
      </ul>
    </div>
  </div>
</template>

<style scoped>
.plugin-chips {
  position: relative;
  display: inline-flex;
  align-items: center;
}
.trigger {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: var(--fs-sm, 12px);
  color: var(--text-muted, #888);
  background: transparent;
  border: 1px solid var(--border, rgba(255, 255, 255, 0.08));
  border-radius: var(--radius-sm, 4px);
  padding: 3px 8px;
  cursor: default;
  font-family: inherit;
}
.trigger:hover {
  color: var(--text, #ddd);
  border-color: var(--border-hover, rgba(255, 255, 255, 0.15));
}
.count {
  font-weight: 600;
  font-variant-numeric: tabular-nums;
  color: var(--text, #ddd);
}
.label {
  letter-spacing: -0.005em;
}
.popover {
  position: absolute;
  top: calc(100% + 6px);
  right: 0;
  min-width: 240px;
  background: var(--bg-elevated, #1a1a1a);
  border: 1px solid var(--border, rgba(255, 255, 255, 0.1));
  border-radius: var(--radius-md, 6px);
  box-shadow: 0 8px 24px rgba(0, 0, 0, 0.4);
  padding: 8px 10px;
  z-index: 100;
  font-size: var(--fs-sm, 12px);
}
.title {
  font-size: 10px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--text-muted, #888);
  padding-bottom: 6px;
  border-bottom: 1px solid var(--border, rgba(255, 255, 255, 0.06));
  margin-bottom: 6px;
}
ul {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 4px;
}
li {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 3px 0;
}
.name {
  font-weight: 500;
  color: var(--text, #ddd);
}
.version {
  font-size: 10px;
  color: var(--text-muted, #888);
  font-variant-numeric: tabular-nums;
}
.badges {
  margin-left: auto;
  display: inline-flex;
  gap: 4px;
}
.badge {
  font-size: 9px;
  text-transform: uppercase;
  letter-spacing: 0.04em;
  background: var(--bg-subtle, rgba(255, 255, 255, 0.06));
  color: var(--text-muted, #aaa);
  padding: 1px 5px;
  border-radius: 3px;
}
</style>