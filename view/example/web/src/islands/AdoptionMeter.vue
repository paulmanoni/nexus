<script setup lang="ts">
import { computed, ref } from 'vue'
import type { NexusIslandProps } from 'nexus-client'

// adopted and total come from the live board: each adoption re-renders the
// page on the server, and the new props reach this component without
// remounting it — the cheers survive.
const props = defineProps<NexusIslandProps['AdoptionMeter']>()
const cheers = ref(0)
const percent = computed(() => (props.total ? Math.round((props.adopted / props.total) * 100) : 0))
</script>

<template>
  <div id="meter" class="space-y-2">
    <div class="h-3 w-full rounded-full bg-gray-200">
      <div class="h-3 rounded-full bg-green-500 transition-all" :style="{ width: percent + '%' }"></div>
    </div>
    <p class="text-sm"><b id="meter-percent">{{ percent }}%</b> adopted
      <button id="cheer" class="ml-2 underline" @click="cheers++">cheer</button>
      <span id="cheers">{{ cheers }}</span>
    </p>
  </div>
</template>
