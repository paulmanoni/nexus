<script setup>
import { inject, ref, computed, onMounted, onBeforeUnmount } from 'vue'
import { ArrowLeftRight } from 'lucide-vue-next'

// The "Proxied" surface for the single-view shell: a header chip that opens a
// panel showing the strangler-fig migration burndown. Data is injected from
// Architecture as `nexus.proxiedSummary` — snapshot.extra.proxied, contributed
// by extension/proxy via RegisterSnapshotExtra("proxied"). The default { value:
// null } means the proxy extension isn't wired, so the chip stays hidden.
const proxied = inject('nexus.proxiedSummary', { value: null })

const open = ref(false)
const root = ref(null)

const pct = computed(() => {
  const p = proxied.value
  return p && p.total ? Math.round((p.migrated / p.total) * 100) : 0
})

onMounted(() => {
  document.addEventListener('click', onDocClick)
  document.addEventListener('keydown', onKey)
})
onBeforeUnmount(() => {
  document.removeEventListener('click', onDocClick)
  document.removeEventListener('keydown', onKey)
})

function toggle(e) {
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
  <div ref="root" class="proxied-chips" v-if="proxied.value">
    <button class="trigger" type="button" :aria-expanded="open" @click="toggle" title="Proxied routes (strangler-fig migration)">
      <ArrowLeftRight :size="12" :stroke-width="2.25" />
      <span class="count">{{ proxied.value.proxied }}</span>
      <span class="label">proxied</span>
    </button>
    <div class="popover" v-if="open" role="menu">
      <div class="title">Proxied → {{ proxied.value.upstream }}</div>
      <div class="summary">
        <span class="mig">{{ proxied.value.migrated }}</span> of {{ proxied.value.total }} migrated
        <div class="bar"><div class="bar-fill" :style="{ width: pct + '%' }"></div></div>
      </div>
      <ul v-if="proxied.value.routes.length">
        <li v-for="r in proxied.value.routes" :key="r.method + ' ' + r.path">
          <span class="rmethod">{{ r.method }}</span>
          <span class="rpath" :title="r.path">{{ r.path }}</span>
          <span class="status" :class="r.status">{{ r.status }}</span>
        </li>
      </ul>
      <div class="empty" v-else>No enumerated routes.</div>
    </div>
  </div>
</template>

<style scoped>
.proxied-chips {
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
  top: calc(100% + 4px);
  right: 0;
  min-width: 280px;
  max-width: 380px;
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
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.summary {
  font-size: 12px;
  color: var(--text-muted);
  margin-bottom: 8px;
}
.summary .mig {
  font-weight: 600;
  color: var(--text);
  font-variant-numeric: tabular-nums;
}
.bar {
  height: 5px;
  border-radius: 3px;
  background: var(--bg-subtle);
  margin-top: 5px;
  overflow: hidden;
}
.bar-fill {
  height: 100%;
  background: var(--ok, #22c55e);
  transition: width 0.3s ease;
}
ul {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 3px;
  max-height: 320px;
  overflow-y: auto;
}
li {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 2px 0;
}
.rmethod {
  font-size: 9px;
  font-weight: 700;
  letter-spacing: 0.03em;
  color: var(--text-muted);
  min-width: 34px;
  font-variant-numeric: tabular-nums;
}
.rpath {
  flex: 1;
  color: var(--text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--font-mono, ui-monospace, monospace);
  font-size: 11px;
}
.status {
  margin-left: auto;
  font-size: 9px;
  text-transform: uppercase;
  letter-spacing: 0.04em;
  padding: 1px 5px;
  border-radius: 3px;
  border: 1px solid var(--border);
  flex: none;
}
.status.proxied {
  background: color-mix(in srgb, var(--warn, #f59e0b) 14%, transparent);
  color: var(--warn, #f59e0b);
  border-color: color-mix(in srgb, var(--warn, #f59e0b) 30%, transparent);
}
.status.migrated {
  background: color-mix(in srgb, var(--ok, #22c55e) 14%, transparent);
  color: var(--ok, #22c55e);
  border-color: color-mix(in srgb, var(--ok, #22c55e) 30%, transparent);
}
.empty {
  color: var(--text-dim);
  font-size: 11px;
  padding: 2px 0;
}
</style>
