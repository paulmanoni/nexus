<script setup>
import { computed } from 'vue'
import {
  Box, Boxes, Database, Zap, Layers, Clock, Cog, Globe, Server, ShieldCheck,
} from 'lucide-vue-next'

// CategoryIcon renders the colored icon-tile that every node type uses on
// its left edge — a 36×36 (default) rounded square filled at 12% alpha with
// the type's category color, with the icon stroked at full color. Single
// component instead of a hard-coded tile inside each node so the visual
// grammar stays exact across the canvas.
//
// Pick a type from the taxonomy in tokens.css §3. `size` tweaks the tile
// for compact contexts (chips on edge labels, drawer headers).
const props = defineProps({
  type: {
    type: String,
    required: true,
    validator: v => [
      'service', 'module', 'database', 'cache', 'queue',
      'cron', 'worker', 'internet', 'peer', 'auth',
    ].includes(v),
  },
  size: { type: Number, default: 36 },
})

// One Lucide glyph per category. Modules borrow Boxes (group of services)
// while bare services use Box; everything else picks a domain-evocative
// icon. Keep this map narrow — when you need a new node type, add its
// token in tokens.css and a row here.
const ICON_BY_TYPE = {
  service:  Box,
  module:   Boxes,
  database: Database,
  cache:    Zap,
  queue:    Layers,
  cron:     Clock,
  worker:   Cog,
  internet: Globe,
  peer:     Server,
  auth:     ShieldCheck,
}

// Token-driven category color. Keeps the tile, the icon stroke, and any
// status accent that wants to derive from category in lockstep with
// tokens.css; bumping a hue there ripples here for free.
const TOKEN_BY_TYPE = {
  service:  'var(--cat-service)',
  module:   'var(--cat-service)',
  database: 'var(--cat-database)',
  cache:    'var(--cat-cache)',
  queue:    'var(--cat-queue)',
  cron:     'var(--cat-cron)',
  worker:   'var(--cat-worker)',
  internet: 'var(--cat-internet)',
  peer:     'var(--cat-peer)',
  auth:     'var(--cat-auth)',
}

const Icon = computed(() => ICON_BY_TYPE[props.type] || Box)
const color = computed(() => TOKEN_BY_TYPE[props.type] || 'var(--cat-service)')

// Tile + glyph scale together — glyph is ~50% of the tile so the icon
// reads at a glance without dominating. Stroke width matches Lucide's
// default for medium tiles (1.8) and bumps slightly for tiny ones so
// the lines don't disappear.
const tileStyle = computed(() => ({
  width: props.size + 'px',
  height: props.size + 'px',
  background: `color-mix(in srgb, ${color.value} 12%, transparent)`,
  color: color.value,
}))
const glyphSize = computed(() => Math.round(props.size * 0.5))
const strokeWidth = computed(() => (props.size <= 18 ? 2.2 : 1.8))
</script>

<template>
  <div class="cat-icon" :style="tileStyle">
    <component :is="Icon" :size="glyphSize" :stroke-width="strokeWidth" />
  </div>
</template>

<style scoped>
.cat-icon {
  display: inline-grid;
  place-items: center;
  border-radius: var(--radius-md);
  flex-shrink: 0;
  /* Subtle inner ring so the tile reads as elevated even at the
     translucent fill. Avoids a heavier border that would compete
     with the surrounding card border. */
  box-shadow: inset 0 0 0 1px color-mix(in srgb, currentColor 18%, transparent);
}
</style>