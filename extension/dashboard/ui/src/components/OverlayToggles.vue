<script setup>
import { computed, inject } from 'vue'
import { AlertTriangle, Gauge, ShieldCheck } from 'lucide-vue-next'

// OverlayToggles is the floating chip group that turns the canvas
// into a diagnostic surface. Each chip is a toggle: when active, op
// rows that DON'T match the overlay's predicate fade to ~35% so
// matching rows pop. Multiple chips can be active simultaneously —
// rows pass if any active overlay matches them (OR semantics).
//
// State lives in Architecture.vue under the `nexus.overlays` provide
// key as a Set of overlay ids: 'errors', 'limits', 'auth'.
const overlays = inject('nexus.overlays', { value: new Set() })
const setOverlay = inject('nexus.setOverlay', () => {})

const ITEMS = [
  { id: 'errors', label: 'Errors', icon: AlertTriangle, hue: '#ef4444' }, // --st-error
  { id: 'limits', label: 'Limits', icon: Gauge,         hue: '#ec4899' }, // --cat-cron
  { id: 'auth',   label: 'Auth',   icon: ShieldCheck,   hue: '#f59e0b' }, // --st-warn
]

function isActive(id) { return overlays.value.has(id) }
function toggle(id) {
  const next = new Set(overlays.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  setOverlay(next)
}
const anyActive = computed(() => overlays.value.size > 0)
</script>

<template>
  <div class="overlay-toggles" :class="{ active: anyActive }">
    <span class="label">Highlight</span>
    <button
      v-for="it in ITEMS"
      :key="it.id"
      class="chip"
      :class="{ active: isActive(it.id) }"
      :style="isActive(it.id) ? { '--hue': it.hue } : {}"
      :title="`Highlight ops with ${it.label.toLowerCase()}`"
      @click="toggle(it.id)"
    >
      <component :is="it.icon" :size="13" :stroke-width="2" />
      {{ it.label }}
    </button>
  </div>
</template>

<style scoped>
.overlay-toggles {
  position: absolute;
  left: 12px;
  bottom: 52px; /* clears the ActivityRail's collapsed header (40px + margin) */
  z-index: 12;
  display: flex;
  align-items: center;
  gap: 4px;
  padding: 4px 6px;
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  box-shadow: var(--shadow-sm);
  font-family: var(--font-sans);
  font-size: var(--fs-xs);
  transition: box-shadow 160ms;
}
.overlay-toggles.active { box-shadow: var(--shadow-md); }

.label {
  padding-left: 4px;
  padding-right: 4px;
  color: var(--text-dim);
  font-size: 10px;
  font-weight: 600;
  letter-spacing: 0.06em;
  text-transform: uppercase;
}

.chip {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 4px 9px;
  background: transparent;
  border: 1px solid transparent;
  color: var(--text-muted);
  font-size: var(--fs-xs);
  font-weight: 500;
  border-radius: var(--radius-sm);
  cursor: pointer;
  transition: background 120ms, color 120ms, border-color 120ms;
}
.chip:hover { background: var(--bg-hover); color: var(--text); }
.chip.active {
  color: var(--hue);
  background: color-mix(in srgb, var(--hue) 12%, transparent);
  border-color: color-mix(in srgb, var(--hue) 35%, transparent);
}
</style>