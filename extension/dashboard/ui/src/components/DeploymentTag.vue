<script setup>
import { computed } from 'vue'

// DeploymentTag is the small inline pill shown on every card that
// belongs to a named deployment unit. Same hashed colour palette the
// previous DeploymentFrame used — same name always picks the same
// hue across renders, so all cards in a deployment share a visual
// signal without needing a labelled container.
//
// Usage:
//   <DeploymentTag :name="data.deployment" />
const props = defineProps({
  name: { type: String, required: true },
})

const PALETTE = [
  { fg: '#6366f1', bg: 'rgba(99, 102, 241, 0.10)' }, // indigo
  { fg: '#0ea5e9', bg: 'rgba(14, 165, 233, 0.10)' }, // sky
  { fg: '#10b981', bg: 'rgba(16, 185, 129, 0.10)' }, // emerald
  { fg: '#f59e0b', bg: 'rgba(245, 158, 11, 0.12)' }, // amber
  { fg: '#ec4899', bg: 'rgba(236, 72, 153, 0.10)' }, // pink
  { fg: '#a855f7', bg: 'rgba(168, 85, 247, 0.10)' }, // purple
]
function hashIndex(s) {
  let h = 0
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0
  return Math.abs(h) % PALETTE.length
}
const colors = computed(() => PALETTE[hashIndex(props.name || '')])
const style = computed(() => ({
  color: colors.value.fg,
  background: colors.value.bg,
  borderColor: 'color-mix(in srgb, ' + colors.value.fg + ' 35%, transparent)',
}))
</script>

<template>
  <span class="deployment-tag" :style="style" :title="'Deployment: ' + name">
    {{ name }}
  </span>
</template>

<style scoped>
.deployment-tag {
  display: inline-flex;
  align-items: center;
  font-family: var(--font-mono);
  font-size: 9.5px;
  font-weight: 600;
  letter-spacing: 0.04em;
  text-transform: uppercase;
  padding: 2px 7px;
  border-radius: 999px;
  border: 1px solid;
  white-space: nowrap;
  flex-shrink: 0;
}
</style>