<script setup>
import { ref, onMounted, onBeforeUnmount } from 'vue'
import { Puzzle } from 'lucide-vue-next'
import { fetchPlugins } from '../lib/api.js'

const plugins = ref([])
const open = ref(false)
const root = ref(null)

onMounted(async () => {
  plugins.value = await fetchPlugins()
  // Click-outside + Escape: both close the popover. Bound at mount
  // and torn down at unmount so we never leak listeners on a tab
  // switch. Listening on document.click (not the root) catches
  // clicks on iframes/portals/dashboards inside the same window.
  document.addEventListener('click', onDocClick)
  document.addEventListener('keydown', onKey)
})
onBeforeUnmount(() => {
  document.removeEventListener('click', onDocClick)
  document.removeEventListener('keydown', onKey)
})

function toggle(e) {
  // Stop the document.click handler from immediately re-closing
  // the popover that this very click just opened — the document
  // listener runs after this handler in the capture/bubble cycle.
  e.stopPropagation()
  open.value = !open.value
}
function onDocClick(e) {
  if (!open.value) return
  if (root.value && root.value.contains(e.target)) return
  open.value = false
}
function onKey(e) {
  if (e.key === 'Escape' && open.value) open.value = false
}
</script>

<template>
  <div ref="root" class="plugin-chips" v-if="plugins.length">
    <button class="trigger" type="button" :aria-expanded="open" @click="toggle">
      <Puzzle :size="12" :stroke-width="2.25" />
      <span class="count">{{ plugins.length }}</span>
      <span class="label">plugins</span>
    </button>
    <div class="popover" v-if="open" role="menu">
      <div class="title">Registered plugins</div>
      <ul>
        <li v-for="p in plugins" :key="p.name">
          <span class="name">{{ p.name }}</span>
          <span class="version" v-if="p.version">v{{ p.version }}</span>
          <span class="badges">
            <span class="badge" v-if="p.hasDashboard" title="contributes dashboard routes">dash</span>
            <span class="badge" v-if="p.hasClient" title="contributes client SDK">sdk</span>
            <span class="badge" v-if="p.hasGenerate" title="contributes codegen driver">gen</span>
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
  font-size: 12px;
  color: var(--text-muted);
  background: transparent;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm, 4px);
  padding: 3px 8px;
  cursor: pointer;
  font-family: inherit;
}
.trigger:hover,
.trigger[aria-expanded="true"] {
  color: var(--text);
  border-color: var(--border-strong);
  background: var(--bg-hover);
}
.count {
  font-weight: 600;
  font-variant-numeric: tabular-nums;
  color: var(--text);
}
.label {
  letter-spacing: -0.005em;
}
.popover {
  position: absolute;
  /* No visual gap — the popover sits flush against the trigger so
     a click→read motion doesn't fight a hover-bridge. Borders take
     care of visual separation. */
  top: calc(100% + 4px);
  right: 0;
  min-width: 240px;
  /* Use the defined surface tokens so the popover follows the
     active theme (white on light, dark on dark) instead of falling
     back to a hardcoded near-black via an undefined --bg-elevated. */
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: var(--radius, 6px);
  box-shadow: 0 8px 24px rgba(0, 0, 0, 0.08), 0 2px 6px rgba(0, 0, 0, 0.04);
  padding: 8px 10px;
  z-index: 100;
  font-size: 12px;
}
.title {
  font-size: 10px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--text-muted);
  padding-bottom: 6px;
  border-bottom: 1px solid var(--border);
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
  color: var(--text);
}
.version {
  font-size: 10px;
  color: var(--text-dim);
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
  background: var(--bg-subtle);
  color: var(--text-muted);
  padding: 1px 5px;
  border-radius: 3px;
  border: 1px solid var(--border);
}
</style>
