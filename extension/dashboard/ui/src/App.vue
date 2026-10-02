<script setup>
// The dashboard is a single full-height view now: Architecture owns the
// whole redesigned shell — header (brand · plugins · jump · tweaks · theme),
// the topology canvas with its floating controls, the persistent right-hand
// Inspector, the Tweaks panel, and the teleported deep-dive drawer / Cmd-K.
import Architecture from './views/Architecture.vue'

defineProps({ embedded: Boolean })
</script>

<template>
  <Architecture :embedded="embedded" />
</template>

<style scoped>
</style>
