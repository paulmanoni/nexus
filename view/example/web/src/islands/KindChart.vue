<script setup lang="ts">
import { computed } from 'vue'
import type { NexusIslandProps } from 'nexus-client'

// query is the page's search signal: typing in the search box highlights a
// bar, and clicking a bar searches for that kind — the search results (a
// server-rendered shard) follow.
const props = defineProps<NexusIslandProps['KindChart']>()
const emit = defineEmits<{ 'update:query': [query: string] }>()
const max = computed(() => Math.max(1, ...props.kinds.map((k) => k.count)))
const matches = (kind: string) => props.query.trim() !== '' && kind.includes(props.query.trim().toLowerCase())
</script>

<template>
  <svg id="kind-chart" :viewBox="`0 0 300 ${kinds.length * 24}`" class="w-full" role="img" aria-label="Pets by kind">
    <g v-for="(k, i) in kinds" :key="k.kind" :transform="`translate(0 ${i * 24})`" class="cursor-pointer"
       :data-kind="k.kind" @click="emit('update:query', k.kind)">
      <text x="0" y="15" font-size="12" fill="currentColor">{{ k.kind }}</text>
      <rect x="70" y="4" :width="(k.count / max) * 210" height="14" rx="3" :fill="matches(k.kind) ? '#f97316' : '#6366f1'" />
      <text :x="76 + (k.count / max) * 210" y="15" font-size="12" fill="currentColor">{{ k.count }}</text>
    </g>
  </svg>
</template>
