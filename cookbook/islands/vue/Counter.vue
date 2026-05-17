<!--
  Counter.vue — reference nl-island authored as a Vue 3 SFC.

  Demonstrates the three contact points with the bridge:
    1. defineProps for what the server sends in :nl-island-props
    2. inject('nlChannel') to subscribe to ctx.PushIsland events
    3. onBeforeUnmount to cleanly release the channel listener
       (matters when the page navigates away or the live
       template removes the island on a conditional render)
-->
<script setup lang="ts">
import { ref, inject, onBeforeUnmount } from 'vue'

const props = defineProps<{
  initial?: number
}>()

// channel is provided by the bridge (Counter.island.ts). Type
// it locally rather than importing — keeps each SFC self-
// contained and lets us swap channel implementations without
// dragging a shared types package across the project.
type Channel = {
  on(event: string, fn: (payload: any) => void): () => void
}

const channel = inject<Channel>('nlChannel')

const count = ref<number>(props.initial ?? 0)

// Subscribe to server pushes scoped to this island. The
// bridge wires this to ctx.PushIsland(name, event, payload)
// on the Go side — see the README for the matching handler.
const offReset = channel?.on('reset', () => {
  count.value = 0
})

onBeforeUnmount(() => {
  offReset?.()
})
</script>

<template>
  <div class="counter-island">
    <button @click="count += 1">Count: {{ count }}</button>
    <p class="hint">
      Click to bump on the client only.
      Server can fire <code>PushIsland("Counter", "reset", nil)</code>
      to zero this back out.
    </p>
  </div>
</template>

<style scoped>
.counter-island button {
  padding: 0.5rem 1rem;
  font-size: 1rem;
  cursor: pointer;
}
.counter-island .hint {
  color: #666;
  font-size: 0.85rem;
  margin-top: 0.5rem;
}
</style>
