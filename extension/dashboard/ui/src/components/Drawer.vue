<script setup>
import { ref, watch, onMounted, onUnmounted, nextTick } from 'vue'
import { X } from 'lucide-vue-next'

// Drawer is the right-slide modal that holds detail surfaces for any
// canvas node (ops, resources, workers, crons). Used by Architecture.vue
// as the single click-to-open destination for the canvas — endpoint
// rows + resource cards + worker cards all funnel through here, instead
// of having a separate tab per kind.
//
// Props:
//   open       — show/hide; controls Transition + Teleport mount
//   title      — main title in the header (e.g. "GET /users")
//   subtitle   — small grey line under title (e.g. service name)
//
// Slots:
//   header     — full custom header (overrides the default title block)
//   default    — drawer body content
//
// Behaviour:
//   - Esc closes
//   - Click on backdrop closes (canvas selection stays)
//   - Focus moves to the drawer on open (basic focus trap)
//   - Teleported to body so canvas transforms / fitView don't shift it
const props = defineProps({
  open: { type: Boolean, default: false },
  title: { type: String, default: '' },
  subtitle: { type: String, default: '' },
})
const emit = defineEmits(['close'])

const dialogEl = ref(null)

function onKey(e) {
  if (e.key === 'Escape' && props.open) emit('close')
}

watch(() => props.open, async (open) => {
  if (open) {
    await nextTick()
    dialogEl.value?.focus()
  }
})

onMounted(() => window.addEventListener('keydown', onKey))
onUnmounted(() => window.removeEventListener('keydown', onKey))
</script>

<template>
  <Teleport to="body">
    <Transition name="drawer">
      <div v-if="open" class="drawer-root">
        <div class="backdrop" @click="emit('close')" />
        <aside
          class="drawer"
          ref="dialogEl"
          tabindex="-1"
          aria-modal="true"
          role="dialog"
        >
          <header class="drawer-header">
            <slot name="header">
              <div class="title-stack">
                <div class="title">{{ title }}</div>
                <div v-if="subtitle" class="subtitle">{{ subtitle }}</div>
              </div>
            </slot>
            <button class="close" @click="emit('close')" aria-label="Close">
              <X :size="18" :stroke-width="2" />
            </button>
          </header>
          <div class="drawer-body">
            <slot />
          </div>
        </aside>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.drawer-root {
  position: fixed;
  inset: 0;
  z-index: 100;
  display: flex;
  justify-content: flex-end;
  font-family: var(--font-sans);
}
.backdrop {
  position: absolute;
  inset: 0;
  background: rgba(15, 23, 42, 0.32);
  backdrop-filter: blur(2px);
}
.drawer {
  position: relative;
  width: 480px;
  max-width: 92vw;
  height: 100%;
  background: var(--bg-card);
  border-left: 1px solid var(--border);
  box-shadow: -4px 0 24px rgba(15, 23, 42, 0.12);
  display: flex;
  flex-direction: column;
  outline: none;
}
.drawer-header {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: var(--space-4) var(--space-5);
  border-bottom: 1px solid var(--border);
  gap: var(--space-3);
  background: var(--bg-card);
}
.title-stack { flex: 1; min-width: 0; }
.title {
  font-size: var(--fs-lg);
  font-weight: 600;
  color: var(--text);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.subtitle {
  font-size: var(--fs-sm);
  color: var(--text-muted);
  margin-top: 2px;
}
.close {
  padding: 6px;
  border-radius: var(--radius-sm);
  background: transparent;
  border: 1px solid transparent;
  color: var(--text-muted);
  cursor: pointer;
  flex-shrink: 0;
}
.close:hover {
  background: var(--bg-hover);
  border-color: var(--border);
  color: var(--text);
}
.drawer-body {
  flex: 1;
  overflow-y: auto;
}

/* Slide + fade in/out. Backdrop fades; the drawer panel translates in
   from the right edge for the AWS console feel. 240ms with a snappy
   curve so it doesn't feel sluggish on repeated clicks. */
.drawer-enter-active, .drawer-leave-active {
  transition: opacity 200ms ease;
}
.drawer-enter-active .drawer, .drawer-leave-active .drawer {
  transition: transform 240ms cubic-bezier(0.32, 0.72, 0, 1);
}
.drawer-enter-from, .drawer-leave-to { opacity: 0; }
.drawer-enter-from .drawer, .drawer-leave-to .drawer {
  transform: translateX(100%);
}
</style>