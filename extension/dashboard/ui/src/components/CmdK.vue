<script setup>
import { ref, computed, watch, onMounted, onUnmounted, nextTick } from 'vue'
import { Search, CornerDownLeft } from 'lucide-vue-next'
import CategoryIcon from './CategoryIcon.vue'

// CmdK is the keyboard-driven palette that flies the user to any node
// on the canvas. Opens via Cmd/Ctrl+K (handled by the parent), filters
// the items list as the user types, navigates with ↑/↓, opens the
// matching drawer on Enter.
//
// Props:
//   open  — controls visibility (parent owns the toggle)
//   items — flat list of { id, kind, label, sublabel, searchKey,
//           drawerSpec }; the parent rebuilds it from latestSnapshot
// Emits:
//   close   — palette wants to dismiss (Esc, backdrop, or after select)
//   select  — payload: drawerSpec ({ kind, key })
const props = defineProps({
  open: { type: Boolean, default: false },
  items: { type: Array, default: () => [] },
})
const emit = defineEmits(['close', 'select'])

const query = ref('')
const cursor = ref(0)
const inputEl = ref(null)
const listEl = ref(null)

// Top N matches; substring match against searchKey is enough for now.
// Upgrade to fuzzy scoring (sublime-style) later if needed; substring
// is predictable and keeps "GET /users" intact instead of pulling
// "user" matches that the operator wasn't asking for.
const MAX_RESULTS = 12
const results = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return props.items.slice(0, MAX_RESULTS)
  return props.items
    .filter(it => it.searchKey.includes(q))
    .slice(0, MAX_RESULTS)
})

watch(() => props.open, async (open) => {
  if (open) {
    query.value = ''
    cursor.value = 0
    await nextTick()
    inputEl.value?.focus()
  }
})

// Reset cursor when results shape changes so it never points past the
// end (typing narrows the list).
watch(results, () => {
  if (cursor.value >= results.value.length) cursor.value = Math.max(0, results.value.length - 1)
})

function move(delta) {
  if (!results.value.length) return
  const n = results.value.length
  cursor.value = (cursor.value + delta + n) % n
  // Keep selected row in view inside the scrolling result list.
  nextTick(() => {
    const el = listEl.value?.querySelector('.cmdk-row.active')
    if (el && typeof el.scrollIntoView === 'function') {
      el.scrollIntoView({ block: 'nearest' })
    }
  })
}

function select(item) {
  if (!item) return
  emit('select', item.drawerSpec)
  emit('close')
}

function onKey(e) {
  if (!props.open) return
  if (e.key === 'Escape') {
    e.preventDefault()
    emit('close')
    return
  }
  if (e.key === 'ArrowDown') { e.preventDefault(); move(1); return }
  if (e.key === 'ArrowUp')   { e.preventDefault(); move(-1); return }
  if (e.key === 'Enter')     { e.preventDefault(); select(results.value[cursor.value]); return }
}

onMounted(() => window.addEventListener('keydown', onKey))
onUnmounted(() => window.removeEventListener('keydown', onKey))

// Render the right CategoryIcon type for each result kind. Kept in
// the component so the palette is a leaf — no dependency on outer
// kind→icon maps.
function iconType(kind) {
  if (kind === 'resource-database') return 'database'
  if (kind === 'resource-cache')    return 'cache'
  if (kind === 'resource-queue')    return 'queue'
  if (kind === 'resource')          return 'database'
  if (kind === 'worker')            return 'worker'
  if (kind === 'cron')              return 'cron'
  if (kind === 'auth')              return 'auth'
  return 'service'
}
</script>

<template>
  <Teleport to="body">
    <Transition name="cmdk">
      <div v-if="open" class="cmdk-root" @click.self="emit('close')">
        <div class="cmdk-panel" role="dialog" aria-modal="true">
          <div class="cmdk-head">
            <Search :size="16" :stroke-width="2" class="cmdk-search-ico" />
            <input
              ref="inputEl"
              v-model="query"
              type="text"
              placeholder="Jump to op, resource, or worker…"
              autocomplete="off"
              spellcheck="false"
            />
            <kbd class="esc">Esc</kbd>
          </div>
          <div ref="listEl" class="cmdk-list" v-if="results.length">
            <button
              v-for="(it, i) in results"
              :key="it.id"
              class="cmdk-row"
              :class="{ active: i === cursor }"
              @mouseenter="cursor = i"
              @click="select(it)"
            >
              <CategoryIcon :type="iconType(it.kind)" :size="22" />
              <div class="cmdk-text">
                <div class="cmdk-label">{{ it.label }}</div>
                <div v-if="it.sublabel" class="cmdk-sub">{{ it.sublabel }}</div>
              </div>
              <CornerDownLeft v-if="i === cursor" :size="13" :stroke-width="2" class="cmdk-enter-ico" />
            </button>
          </div>
          <div v-else class="cmdk-empty">
            No results for <code>{{ query }}</code>
          </div>
          <div class="cmdk-foot">
            <span><kbd>↑</kbd><kbd>↓</kbd> navigate</span>
            <span><kbd>↵</kbd> open</span>
            <span><kbd>Esc</kbd> close</span>
          </div>
        </div>
      </div>
    </Transition>
  </Teleport>
</template>

<style scoped>
.cmdk-root {
  position: fixed;
  inset: 0;
  z-index: 200;
  display: flex;
  justify-content: center;
  align-items: flex-start;
  padding-top: 12vh;
  background: rgba(15, 23, 42, 0.36);
  backdrop-filter: blur(2px);
  font-family: var(--font-sans);
}
.cmdk-panel {
  width: 560px;
  max-width: 92vw;
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: var(--radius-lg);
  box-shadow: 0 18px 48px rgba(15, 23, 42, 0.18);
  overflow: hidden;
  display: flex;
  flex-direction: column;
}

.cmdk-head {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-3) var(--space-4);
  border-bottom: 1px solid var(--border);
}
.cmdk-search-ico { color: var(--text-muted); flex-shrink: 0; }
.cmdk-head input {
  flex: 1;
  border: none;
  outline: none;
  background: transparent;
  padding: 4px 0;
  font-size: var(--fs-md);
  color: var(--text);
  font-family: inherit;
}
.cmdk-head input::placeholder { color: var(--text-dim); }
.esc {
  font-family: var(--font-mono);
  font-size: 10px;
  color: var(--text-muted);
  background: var(--bg-hover);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 1px 5px;
}

.cmdk-list {
  max-height: 50vh;
  overflow-y: auto;
  padding: 6px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.cmdk-row {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  padding: 8px 10px;
  border-radius: var(--radius-sm);
  background: transparent;
  border: none;
  cursor: pointer;
  text-align: left;
  width: 100%;
  font-family: inherit;
}
.cmdk-row.active { background: var(--bg-active); }
.cmdk-text { flex: 1; min-width: 0; }
.cmdk-label {
  font-size: var(--fs-md);
  color: var(--text);
  font-weight: 500;
  font-family: var(--font-mono);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.cmdk-sub {
  font-size: var(--fs-xs);
  color: var(--text-dim);
  margin-top: 1px;
}
.cmdk-enter-ico { color: var(--accent); flex-shrink: 0; }

.cmdk-empty {
  padding: var(--space-5) var(--space-4);
  font-size: var(--fs-sm);
  color: var(--text-muted);
  text-align: center;
}
.cmdk-empty code {
  font-family: var(--font-mono);
  background: var(--bg-hover);
  padding: 1px 6px;
  border-radius: var(--radius-sm);
  color: var(--text);
}

.cmdk-foot {
  display: flex;
  gap: var(--space-4);
  justify-content: center;
  padding: var(--space-2) var(--space-4);
  border-top: 1px solid var(--border);
  font-size: var(--fs-xs);
  color: var(--text-dim);
}
.cmdk-foot kbd {
  font-family: var(--font-mono);
  font-size: 10px;
  background: var(--bg-hover);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 1px 5px;
  margin-right: 2px;
  color: var(--text-muted);
}

/* Slide-down + fade from the top, so the palette feels as if it was
   summoned with a keypress rather than appearing flat. */
.cmdk-enter-active, .cmdk-leave-active {
  transition: opacity 160ms ease;
}
.cmdk-enter-active .cmdk-panel, .cmdk-leave-active .cmdk-panel {
  transition: transform 200ms cubic-bezier(0.32, 0.72, 0, 1);
}
.cmdk-enter-from, .cmdk-leave-to { opacity: 0; }
.cmdk-enter-from .cmdk-panel, .cmdk-leave-to .cmdk-panel {
  transform: translateY(-10px);
}
</style>