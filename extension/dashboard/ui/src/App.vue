<script setup>
import { ref, onMounted } from 'vue'
import { Box } from 'lucide-vue-next'
import Architecture from './views/Architecture.vue'
import { fetchConfig } from './lib/api.js'

// The dashboard is a single page now — Architecture canvas with its
// drawer surfaces (op / resource / worker / cron / auth), Cmd-K
// palette, and the bottom Activity rail. Endpoints, Crons, Rate
// limits, Auth, and Traces tabs all folded into the canvas; the App
// shell is just brand + a viewport for Architecture.
const brand = ref('Nexus')

onMounted(async () => {
  const cfg = await fetchConfig()
  if (cfg && cfg.Name) {
    brand.value = cfg.Name
    document.title = cfg.Name
  }
})
</script>

<template>
  <div class="shell">
    <header>
      <div class="brand">
        <div class="logo"><Box :size="16" :stroke-width="2.5" /></div>
        <span>{{ brand }}</span>
      </div>
      <span class="hint">
        <kbd>⌘</kbd><kbd>K</kbd> jump
      </span>
    </header>
    <main>
      <Architecture />
    </main>
  </div>
</template>

<style scoped>
.shell {
  display: flex;
  flex-direction: column;
  height: 100%;
  background: var(--bg);
}
header {
  display: flex;
  align-items: center;
  gap: var(--space-4);
  padding: 0 var(--space-4);
  height: 48px;
  border-bottom: 1px solid var(--border);
  background: var(--bg);
  flex-shrink: 0;
}
.brand {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-weight: 600;
  font-size: var(--fs-md);
  color: var(--text);
  letter-spacing: -0.01em;
}
.logo {
  width: 26px;
  height: 26px;
  background: var(--accent);
  color: white;
  display: grid;
  place-items: center;
  border-radius: var(--radius-sm);
}
.hint {
  margin-left: auto;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: var(--fs-xs);
  color: var(--text-dim);
}
.hint kbd {
  font-family: var(--font-mono);
  font-size: 10px;
  background: var(--bg-hover);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 1px 5px;
  color: var(--text-muted);
}
main {
  flex: 1;
  overflow: hidden;
  position: relative;
  background: var(--bg-subtle);
}
main > * { height: 100%; }
</style>