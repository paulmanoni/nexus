<script setup>
import { ref, onBeforeUnmount } from 'vue'

// PacketOverlay renders flying "packet" dots on top of the VueFlow
// canvas. Each packet is a short-lived div that tweens from a source
// screen point to a target screen point via CSS transforms. Screen
// coords are captured at spawn time so the animation doesn't need to
// track pan/zoom mid-flight — at ~800ms, it's good enough.
//
// state === 'ok'    → accent-coloured packet, smooth arrival
// state === 'error' → red packet that pulses to a stop marker at the
//                     landing point so the viewer sees "request
//                     reached here but did not pass through".
const packets = ref([])
let seq = 0
const DURATION_MS = 800
const STOP_HOLD_MS = 500   // error: how long the stop mark lingers

// spawn(from, to, opts?) schedules a packet. opts.state: 'ok'|'error'.
// Returns nothing; the packet auto-removes after the animation + any
// error "stop hold" so the DOM stays small.
function spawn(from, to, opts = {}) {
  if (!from || !to) return
  const state = opts.state === 'error' ? 'error' : 'ok'
  const id = ++seq
  packets.value = [
    ...packets.value,
    { id, state, x0: from.x, y0: from.y, x1: to.x, y1: to.y },
  ]
  const lifetime = DURATION_MS + (state === 'error' ? STOP_HOLD_MS : 50)
  setTimeout(() => {
    packets.value = packets.value.filter(p => p.id !== id)
  }, lifetime)
}

defineExpose({ spawn })

onBeforeUnmount(() => { packets.value = [] })
</script>

<template>
  <div class="packet-layer">
    <template v-for="p in packets" :key="p.id">
      <!-- Moving dot -->
      <div
        class="packet"
        :class="p.state"
        :style="{
          '--x0': p.x0 + 'px',
          '--y0': p.y0 + 'px',
          '--x1': p.x1 + 'px',
          '--y1': p.y1 + 'px',
        }"
      ></div>
      <!-- Error: stop marker lingers briefly at the destination so the
           viewer registers "the request got HERE and went no further". -->
      <div
        v-if="p.state === 'error'"
        class="stop"
        :style="{ '--x1': p.x1 + 'px', '--y1': p.y1 + 'px' }"
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
  animation-duration: 800ms;
  animation-timing-function: cubic-bezier(0.4, 0.0, 0.2, 1);
  animation-fill-mode: forwards;
}
.packet.ok {
  background: var(--accent);
  box-shadow: 0 0 10px var(--accent-soft), 0 0 0 3px rgba(79, 70, 229, 0.15);
  animation-name: packet-fly-ok;
}
.packet.error {
  background: var(--error);
  box-shadow: 0 0 10px rgba(185, 28, 28, 0.35), 0 0 0 3px var(--error-soft);
  animation-name: packet-fly-err;
}
@keyframes packet-fly-ok {
  0%   { transform: translate(var(--x0), var(--y0)); opacity: 0.0; }
  15%  { opacity: 1; }
  85%  { opacity: 1; }
  100% { transform: translate(var(--x1), var(--y1)); opacity: 0.0; }
}
@keyframes packet-fly-err {
  /* Start normal, but towards the end decelerate harder + stay briefly
     at the landing point so the stop feels abrupt rather than smooth. */
  0%   { transform: translate(var(--x0), var(--y0)); opacity: 0.0; }
  10%  { opacity: 1; }
  80%  { transform: translate(calc(var(--x1) - 2px), var(--y1)); opacity: 1; }
  100% { transform: translate(var(--x1), var(--y1)); opacity: 1; }
}

/* Stop marker: an X that appears where the error packet landed. Fades
   in as the packet arrives (≈800ms) and out again after STOP_HOLD_MS. */
.stop {
  position: absolute;
  left: 0;
  top: 0;
  margin: -9px 0 0 -9px;
  width: 18px;
  height: 18px;
  border-radius: 50%;
  background: var(--error);
  color: #fff;
  font-family: var(--font-mono);
  font-size: 11px;
  font-weight: 700;
  display: grid;
  place-items: center;
  box-shadow: 0 0 0 4px var(--error-soft);
  transform: translate(var(--x1), var(--y1));
  opacity: 0;
  animation: stop-show 1300ms ease-out forwards;
  /* Lifetime: fade in around the 700ms mark (packet landing), hold,
     then fade out. Works without JS coordination because we know
     DURATION_MS + STOP_HOLD_MS = 1300ms. */
}
@keyframes stop-show {
  0%   { opacity: 0; transform: translate(var(--x1), var(--y1)) scale(0.3); }
  55%  { opacity: 0; transform: translate(var(--x1), var(--y1)) scale(0.3); }
  65%  { opacity: 1; transform: translate(var(--x1), var(--y1)) scale(1.15); }
  75%  { transform: translate(var(--x1), var(--y1)) scale(1); }
  92%  { opacity: 1; transform: translate(var(--x1), var(--y1)) scale(1); }
  100% { opacity: 0; transform: translate(var(--x1), var(--y1)) scale(0.9); }
}
</style>
