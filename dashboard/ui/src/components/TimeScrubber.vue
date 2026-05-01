<script setup>
import { ref, computed, inject, onMounted, onUnmounted } from 'vue'
import { Clock, Play } from 'lucide-vue-next'

// TimeScrubber lets the user step backwards through the WS snapshot
// history captured client-side. Each /__nexus/live frame is pushed
// into Architecture.vue's snapshotHistory ring; when the scrubber
// pins an index, the canvas renders THAT frame instead of the latest,
// so stats / errors / cache health are visualised at the moment that
// frame was captured.
//
// First-cut UX: a compact pill in the canvas utility strip. "LIVE"
// when streaming; the historical timestamp when paused. Click opens
// a popover with a slider over history.length steps and a Now button.
// History is session-scoped + capped (~60s at 2s cadence). Add a
// server-side per-bucket layer later for hour-long windows.
const history = inject('nexus.scrubHistory', { value: [] })
const scrubIndex = inject('nexus.scrubIndex', { value: null })
const setScrubIndex = inject('nexus.setScrubIndex', () => {})

const open = ref(false)
const popoverEl = ref(null)
const triggerEl = ref(null)

const isLive = computed(() => scrubIndex.value === null)
const lastIdx = computed(() => Math.max(history.value.length - 1, 0))

// Slider model: 0 = oldest, lastIdx = newest. Treats "live" as the
// rightmost notch; moving off it pins to that index.
const sliderValue = computed({
  get() {
    return scrubIndex.value === null ? lastIdx.value : scrubIndex.value
  },
  set(v) {
    const i = Number(v)
    if (i >= lastIdx.value) setScrubIndex(null) // resume live
    else setScrubIndex(i)
  },
})

const currentEntry = computed(() => {
  if (scrubIndex.value === null) {
    return history.value[lastIdx.value] || null
  }
  return history.value[scrubIndex.value] || null
})
const currentLabel = computed(() => {
  if (isLive.value) return 'LIVE'
  const e = currentEntry.value
  if (!e) return 'LIVE'
  try { return new Date(e.ts).toLocaleTimeString() } catch { return 'LIVE' }
})
const offsetLabel = computed(() => {
  if (isLive.value || !currentEntry.value) return ''
  const ms = Date.now() - currentEntry.value.ts
  const s = Math.round(ms / 1000)
  if (s < 60) return `${s}s ago`
  const m = Math.floor(s / 60)
  if (m < 60) {
    const rem = s - m * 60
    return rem ? `${m}m ${rem}s ago` : `${m}m ago`
  }
  const h = Math.floor(m / 60)
  const remM = m - h * 60
  return remM ? `${h}h ${remM}m ago` : `${h}h ago`
})

// windowLabel describes the total scrubbable span — useful so the
// user knows how far back the slider can reach without doing the
// frames × cadence arithmetic themselves.
const windowLabel = computed(() => {
  if (history.value.length < 2) return ''
  const first = history.value[0]
  const last  = history.value[history.value.length - 1]
  if (!first || !last) return ''
  const s = Math.round((last.ts - first.ts) / 1000)
  if (s < 60) return `${s}s window`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m window`
  const h = Math.floor(m / 60)
  const remM = m - h * 60
  return remM ? `${h}h ${remM}m window` : `${h}h window`
})

function resumeLive() {
  setScrubIndex(null)
}

function onOutside(ev) {
  if (!open.value) return
  if (popoverEl.value?.contains(ev.target)) return
  if (triggerEl.value?.contains(ev.target)) return
  open.value = false
}
onMounted(() => document.addEventListener('mousedown', onOutside))
onUnmounted(() => document.removeEventListener('mousedown', onOutside))
</script>

<template>
  <div class="scrubber">
    <button
      ref="triggerEl"
      class="trigger"
      :class="{ live: isLive, paused: !isLive }"
      :title="isLive ? 'Live — click to scrub through recent snapshots' : 'Scrubbing past snapshot — click for slider'"
      @click="open = !open"
    >
      <Clock :size="13" :stroke-width="2" />
      <span class="label">{{ currentLabel }}</span>
      <span v-if="!isLive" class="offset">{{ offsetLabel }}</span>
    </button>

    <div v-if="open" ref="popoverEl" class="popover">
      <div class="pop-head">
        <span class="pop-title">
          <Clock :size="12" :stroke-width="2" />
          Time
        </span>
        <span class="pop-info">
          {{ windowLabel || `${history.length} frame${history.length === 1 ? '' : 's'} captured` }}
        </span>
      </div>
      <div class="slider-row">
        <input
          type="range"
          min="0"
          :max="lastIdx"
          step="1"
          v-model.number="sliderValue"
          :disabled="history.length < 2"
        />
      </div>
      <div class="pop-actions">
        <span class="time">{{ currentLabel }}</span>
        <span v-if="!isLive" class="time dim">· {{ offsetLabel }}</span>
        <span class="spacer" />
        <button class="now" :disabled="isLive" @click="resumeLive">
          <Play :size="11" :stroke-width="2" />
          Now
        </button>
      </div>
      <div v-if="history.length < 2" class="hint">
        Capturing snapshots… stay on the page for a few seconds and a
        scrubbable history will populate.
      </div>
    </div>
  </div>
</template>

<style scoped>
.scrubber { position: relative; }

.trigger {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 6px 12px;
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  color: var(--text-muted);
  font-size: var(--fs-sm);
  font-weight: 500;
  cursor: pointer;
  box-shadow: var(--shadow-sm);
  font-family: var(--font-sans);
}
.trigger.live .label   { color: var(--st-healthy); font-family: var(--font-mono); font-size: var(--fs-xs); letter-spacing: 0.06em; }
.trigger.paused        { border-color: color-mix(in srgb, var(--cat-cron) 35%, transparent); }
.trigger.paused .label { color: var(--cat-cron); font-family: var(--font-mono); font-variant-numeric: tabular-nums; }
.trigger.paused svg    { color: var(--cat-cron); }
.trigger:hover         { background: var(--bg-hover); }

.offset {
  font-size: 10px;
  color: var(--text-dim);
  font-variant-numeric: tabular-nums;
}

.popover {
  position: absolute;
  top: calc(100% + 6px);
  right: 0;
  min-width: 280px;
  padding: var(--space-3);
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
  box-shadow: var(--shadow-lg);
  z-index: 30;
  font-family: var(--font-sans);
}

.pop-head {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  margin-bottom: var(--space-2);
}
.pop-title {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: var(--fs-xs);
  font-weight: 600;
  color: var(--text);
}
.pop-info {
  margin-left: auto;
  font-size: 10px;
  color: var(--text-dim);
}

.slider-row {
  margin: var(--space-2) 0;
}
.slider-row input[type="range"] {
  width: 100%;
  accent-color: var(--accent);
}

.pop-actions {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  margin-top: var(--space-1);
}
.time {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text);
}
.time.dim { color: var(--text-dim); }
.spacer { flex: 1; }

.now {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 4px 10px;
  font-size: var(--fs-xs);
  background: var(--accent);
  color: white;
  border: 1px solid var(--accent);
  border-radius: var(--radius-sm);
  cursor: pointer;
  font-weight: 500;
}
.now:hover:not(:disabled) { background: var(--accent-hover); }
.now:disabled { opacity: 0.5; cursor: not-allowed; }

.hint {
  margin-top: var(--space-2);
  padding: var(--space-2);
  background: var(--bg-subtle);
  border: 1px dashed var(--border-strong);
  border-radius: var(--radius-sm);
  font-size: var(--fs-xs);
  color: var(--text-muted);
}
</style>