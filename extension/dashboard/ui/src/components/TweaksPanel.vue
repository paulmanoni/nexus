<script setup>
import { Sliders, X, Globe, User, Box, LayoutGrid } from 'lucide-vue-next'

// TweaksPanel — the redesign's appearance/topology control surface. Pure
// view + emits; Architecture.vue owns the state and applies the accent /
// density / layout changes (persisting them to localStorage).
defineProps({
  theme: { type: String, default: 'dark' },
  accent: { type: String, default: '' },
  density: { type: String, default: 'regular' },
  mode: { type: String, default: 'layered' },
  laneLabels: { type: Boolean, default: true },
  live: { type: Boolean, default: true },
  clientIcon: { type: String, default: 'globe' },
})
const emit = defineEmits(['set-theme', 'set-accent', 'set-density', 'set-mode', 'set-lane', 'set-live', 'set-client', 'close'])

const ACCENTS = ['#2fe0c6', '#5b8cff', '#7c5cff', '#c6f24e', '#ff8a4c', '#f25fb0']
const CLIENT_ICONS = [
  { id: 'globe', icon: Globe },
  { id: 'user', icon: User },
  { id: 'box', icon: Box },
  { id: 'layout', icon: LayoutGrid },
]
const DENSITIES = ['compact', 'regular', 'comfy']
</script>

<template>
  <div class="tweaks">
    <div class="tw-head">
      <Sliders :size="15" :stroke-width="2" />
      <span>Tweaks</span>
      <span class="tw-spacer"></span>
      <button class="tw-close" @click="emit('close')"><X :size="15" :stroke-width="2" /></button>
    </div>
    <div class="tw-body">
      <div class="tw-sec">Appearance</div>
      <div class="tw-row">
        <label>Theme</label>
        <div class="tw-seg">
          <button :class="{ on: theme === 'light' }" @click="emit('set-theme', 'light')">Light</button>
          <button :class="{ on: theme === 'dark' }" @click="emit('set-theme', 'dark')">Dark</button>
        </div>
      </div>
      <div class="tw-row">
        <label>Accent</label>
        <div class="tw-sw-row">
          <button v-for="c in ACCENTS" :key="c" class="tw-sw" :class="{ on: accent === c }" :style="{ background: c }" @click="emit('set-accent', c)"></button>
        </div>
      </div>
      <div class="tw-row">
        <label>Density</label>
        <div class="tw-seg">
          <button v-for="d in DENSITIES" :key="d" :class="{ on: density === d }" @click="emit('set-density', d)">{{ d }}</button>
        </div>
      </div>

      <div class="tw-sec">Topology</div>
      <div class="tw-row">
        <label>Layout</label>
        <div class="tw-seg">
          <button :class="{ on: mode === 'layered' }" @click="emit('set-mode', 'layered')">Layered</button>
          <button :class="{ on: mode === 'flow' }" @click="emit('set-mode', 'flow')">Flow</button>
        </div>
      </div>
      <div class="tw-row">
        <label>Lane labels</label>
        <button class="tw-tg" :class="{ on: laneLabels }" @click="emit('set-lane', !laneLabels)"><i></i></button>
      </div>
      <div class="tw-row">
        <label>Live traffic</label>
        <button class="tw-tg" :class="{ on: live }" @click="emit('set-live', !live)"><i></i></button>
      </div>

      <div class="tw-sec">Client node</div>
      <div class="tw-row">
        <label>Presentation</label>
        <div class="tw-icons">
          <button v-for="ic in CLIENT_ICONS" :key="ic.id" class="tw-ic" :class="{ on: clientIcon === ic.id }" @click="emit('set-client', ic.id)">
            <component :is="ic.icon" :size="16" :stroke-width="2" />
          </button>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.tweaks {
  position: fixed; top: 64px; right: 16px; width: 300px; max-height: calc(100vh - 88px);
  overflow-y: auto; z-index: 60; background: var(--surface);
  border: 1px solid var(--line); border-radius: 16px; box-shadow: var(--shadow-pop);
  animation: popin 180ms var(--ease); font-family: var(--font-sans);
}
@keyframes popin { from { opacity: 0; transform: translateY(-6px); } to { opacity: 1; transform: none; } }
.tw-head {
  display: flex; align-items: center; gap: 8px; padding: 14px 16px;
  border-bottom: 1px solid var(--line); font-weight: 600; font-size: 13.5px; color: var(--ink);
  position: sticky; top: 0; background: var(--surface); border-radius: 16px 16px 0 0; z-index: 1;
}
.tw-head > svg { color: var(--accent); }
.tw-spacer { flex: 1; }
.tw-close { border: none; background: none; color: var(--ink-3); cursor: pointer; padding: 3px; border-radius: 6px; display: grid; place-items: center; }
.tw-close:hover { background: var(--surface-3); color: var(--ink); }
.tw-body { padding: 4px 16px 18px; }
.tw-sec { font-size: 10.5px; font-weight: 650; letter-spacing: .08em; text-transform: uppercase; color: var(--ink-3); margin: 16px 0 8px; }
.tw-row { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 6px 0; }
.tw-row > label { font-size: 12.5px; color: var(--ink-2); white-space: nowrap; }
.tw-seg { display: inline-flex; background: var(--surface-3); border: 1px solid var(--line); border-radius: 9px; padding: 2px; gap: 2px; }
.tw-seg button {
  font-family: inherit; font-size: 11.5px; text-transform: capitalize; color: var(--ink-2);
  border: none; background: none; padding: 5px 10px; border-radius: 7px; cursor: pointer;
  transition: all var(--speed) var(--ease);
}
.tw-seg button.on { background: var(--surface); color: var(--ink); box-shadow: var(--shadow-card); }
.tw-sw-row { display: flex; gap: 7px; }
.tw-sw { width: 22px; height: 22px; border-radius: 50%; border: none; cursor: pointer; box-shadow: inset 0 0 0 1px rgba(0, 0, 0, .12); transition: transform var(--speed) var(--ease); padding: 0; }
.tw-sw:hover { transform: scale(1.12); }
.tw-sw.on { outline: 2px solid var(--ink); outline-offset: 2px; }
.tw-tg { width: 40px; height: 23px; border-radius: var(--r-pill); background: var(--surface-3); border: 1px solid var(--line); position: relative; cursor: pointer; transition: background var(--speed) var(--ease); flex: none; padding: 0; }
.tw-tg i { position: absolute; top: 2px; left: 2px; width: 17px; height: 17px; border-radius: 50%; background: var(--ink-3); transition: all var(--speed) var(--ease); }
.tw-tg.on { background: var(--accent); border-color: transparent; }
.tw-tg.on i { left: 19px; background: #fff; }
.tw-icons { display: flex; gap: 6px; }
.tw-ic { width: 30px; height: 30px; border-radius: 8px; border: 1px solid var(--line); background: var(--surface-3); color: var(--ink-2); display: grid; place-items: center; cursor: pointer; transition: all var(--speed) var(--ease); padding: 0; }
.tw-ic.on { border-color: var(--accent-line); color: var(--accent); background: var(--accent-soft); }
</style>
