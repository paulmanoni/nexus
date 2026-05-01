<script setup>
import { reactive, ref, onBeforeUnmount } from 'vue'

// PacketOverlay renders flying "packet" dots on top of the VueFlow
// canvas, and now rides the actual edge SVG paths via getPointAtLength
// instead of straight-line tweens. Smoothstep edges turn corners; the
// packet must too, otherwise it cuts across nodes/cards.
//
// Each packet drives its (x, y) every frame from the path's geometry
// converted to canvas-relative coords (path local → screen via
// getScreenCTM, then minus canvas bounding rect). RAF naturally handles
// pan/zoom during transit because we re-read the screen CTM each frame.
//
// state === 'ok'    → accent-coloured packet, smooth arrival
// state === 'error' → red packet that lingers at the landing point with
//                     an X stop marker so the viewer sees "request
//                     reached here but did not pass through".
const packets = ref([])
let seq = 0
const DURATION_MS = 850
const STOP_HOLD_MS = 500

// spawn(pathEl, canvasEl, opts) attaches a packet to one Vue Flow edge
// path. opts.state: 'ok' | 'error'. The packet auto-removes after the
// animation (+ stop hold for errors) so the DOM stays small.
function spawn(pathEl, canvasEl, opts = {}) {
  if (!pathEl || !canvasEl) return
  let len = 0
  try { len = pathEl.getTotalLength() } catch { len = 0 }
  if (!len) return

  const state = opts.state === 'error' ? 'error' : 'ok'
  const id = ++seq
  // reactive() here so the per-frame x/y/opacity mutations actually
  // re-render the bound :style. A bare object pushed to a ref([])
  // would update the ref but not propagate inner-property changes.
  const p = reactive({ id, state, x: 0, y: 0, opacity: 0, endX: 0, endY: 0 })
  packets.value.push(p)

  // Lock the path's terminus in canvas-relative coords so the error
  // stop marker can render at the right spot without re-querying the
  // path each render. Computed once at spawn — pan/zoom shift after
  // landing is rare and the marker decays in 500ms anyway.
  try {
    const endPt = pathEl.getPointAtLength(len)
    const ctm = pathEl.getScreenCTM()
    const cr = canvasEl.getBoundingClientRect()
    if (ctm) {
      p.endX = ctm.a * endPt.x + ctm.c * endPt.y + ctm.e - cr.left
      p.endY = ctm.b * endPt.x + ctm.d * endPt.y + ctm.f - cr.top
    }
  } catch { /* path may have detached mid-spawn — ignore */ }

  const startedAt = performance.now()
  function frame(t) {
    const elapsed = t - startedAt
    const progress = Math.min(elapsed / DURATION_MS, 1)
    if (!pathEl.isConnected) {
      // Edge re-rendered (poll regenerated rawEdges). Drop the packet
      // rather than animate against a stale path.
      packets.value = packets.value.filter(x => x.id !== id)
      return
    }
    let pt
    try { pt = pathEl.getPointAtLength(progress * len) } catch { pt = null }
    if (pt) {
      const ctm = pathEl.getScreenCTM()
      const cr = canvasEl.getBoundingClientRect()
      if (ctm) {
        const sx = ctm.a * pt.x + ctm.c * pt.y + ctm.e
        const sy = ctm.b * pt.x + ctm.d * pt.y + ctm.f
        p.x = sx - cr.left
        p.y = sy - cr.top
      }
    }
    // Opacity envelope — fade in over 0–15%, hold, fade out 85–100%.
    if (progress < 0.15)        p.opacity = progress / 0.15
    else if (progress > 0.85)   p.opacity = (1 - progress) / 0.15
    else                        p.opacity = 1
    if (progress < 1) {
      requestAnimationFrame(frame)
    } else {
      // Error packets stay visible at the landing point until the stop
      // marker fades; ok packets vanish quickly to keep the canvas tidy.
      const lifetime = state === 'error' ? STOP_HOLD_MS : 50
      setTimeout(() => {
        packets.value = packets.value.filter(x => x.id !== id)
      }, lifetime)
    }
  }
  requestAnimationFrame(frame)
}

defineExpose({ spawn })

onBeforeUnmount(() => { packets.value = [] })
</script>

<template>
  <div class="packet-layer">
    <template v-for="p in packets" :key="p.id">
      <div
        class="packet"
        :class="p.state"
        :style="{
          transform: `translate(${p.x}px, ${p.y}px)`,
          opacity: p.opacity,
        }"
      ></div>
      <div
        v-if="p.state === 'error'"
        class="stop"
        :style="{ transform: `translate(${p.endX}px, ${p.endY}px)` }"
      >✕</div>
    </template>
  </div>
</template>

<style scoped>
.packet-layer {
  position: absolute;
  inset: 0;
  pointer-events: none;
  z-index: 20;
}
.packet {
  position: absolute;
  left: 0;
  top: 0;
  width: 10px;
  height: 10px;
  margin: -5px 0 0 -5px;  /* center on (x, y) */
  border-radius: 50%;
  /* Position is driven by JS RAF (transform/opacity inline-styled);
     no CSS keyframes — they'd fight the path-follower. */
  will-change: transform, opacity;
}
.packet.ok {
  background: var(--accent);
  box-shadow:
    0 0 10px var(--accent-soft),
    0 0 0 3px color-mix(in srgb, var(--accent) 18%, transparent);
}
.packet.error {
  background: var(--st-error);
  box-shadow:
    0 0 10px color-mix(in srgb, var(--st-error) 35%, transparent),
    0 0 0 3px var(--st-error-soft);
}

/* Stop marker — fades in as the error packet arrives, holds, fades out.
   Lifetime matches DURATION_MS + STOP_HOLD_MS so no JS coordination. */
.stop {
  position: absolute;
  left: 0;
  top: 0;
  margin: -9px 0 0 -9px;
  width: 18px;
  height: 18px;
  border-radius: 50%;
  background: var(--st-error);
  color: #fff;
  font-family: var(--font-mono);
  font-size: 11px;
  font-weight: 700;
  display: grid;
  place-items: center;
  box-shadow: 0 0 0 4px var(--st-error-soft);
  opacity: 0;
  animation: stop-show 1350ms ease-out forwards;
}
@keyframes stop-show {
  0%   { opacity: 0; }
  55%  { opacity: 0; }
  65%  { opacity: 1; }
  92%  { opacity: 1; }
  100% { opacity: 0; }
}
</style>