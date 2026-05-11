<script setup>
import { computed } from 'vue'

// BoundaryNode draws the "system" perimeter — a dashed rectangle that
// encloses every service + resource card, making it visually obvious
// where the app's own surface ends and external callers begin.
// Architecture.vue sizes and positions this node via the computed bbox
// of the in-system nodes; rendering is a single styled div.
const props = defineProps(['data'])
const styleObj = computed(() => ({
  width:  props.data.width + 'px',
  height: props.data.height + 'px',
}))
</script>

<template>
  <div class="boundary" :style="styleObj">
    <span class="label">System</span>
  </div>
</template>

<style scoped>
.boundary {
  /* Sits BEHIND other nodes thanks to a lower zIndex assigned on the
     VueFlow node; pointer-events off so clicks fall through to the
     real nodes rendered on top. */
  position: relative;
  pointer-events: none;
  background: rgba(79, 70, 229, 0.03);
  border: 1.5px dashed var(--accent);
  border-radius: 14px;
  box-sizing: border-box;
}
.label {
  position: absolute;
  top: -10px;
  left: 14px;
  padding: 1px 8px;
  background: var(--bg);
  color: var(--accent);
  font-family: var(--font-mono);
  font-size: 10px;
  font-weight: 600;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  border-radius: 4px;
  border: 1px solid var(--accent);
}
</style>
