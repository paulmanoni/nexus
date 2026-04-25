import { onUnmounted } from 'vue'

// usePoll fires fn on a setInterval and clears it on unmount. Must be
// called during a component's setup so onUnmounted has somewhere to
// register; safe in `<script setup>`.
//
// It does NOT call fn immediately — most views need an awaited initial
// fetch before kicking off polling, so the caller does that in
// onMounted (or just calls load() at top level if it's fire-and-forget).
//
// Returns { stop } so a view can pause polling early without unmounting.
export function usePoll(fn, intervalMs) {
  let id = setInterval(fn, intervalMs)
  function stop() {
    if (id !== null) {
      clearInterval(id)
      id = null
    }
  }
  onUnmounted(stop)
  return { stop }
}