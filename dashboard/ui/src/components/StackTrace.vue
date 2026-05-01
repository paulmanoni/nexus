<script setup>
// StackTrace is the disclosure widget every error context uses to
// surface the captured Go runtime stack. Renders nothing when the
// `stack` prop is empty (no panic, or the error originated from a
// plain return value), so call sites can drop it in unconditionally
// after the message and let the data decide whether it shows.
defineProps({
  stack: { type: String, default: '' },
  // label override — defaults to "stack" but ErrorDialog uses
  // "view stack" for clarity within a longer row.
  label: { type: String, default: 'stack' },
})
</script>

<template>
  <details v-if="stack" class="stack">
    <summary>{{ label }}</summary>
    <pre>{{ stack }}</pre>
  </details>
</template>

<style scoped>
.stack {
  margin-top: 6px;
}
.stack summary {
  cursor: pointer;
  font-family: var(--font-mono);
  font-size: 10px;
  font-weight: 600;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--text-dim);
  list-style: none;
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 2px 6px;
  border-radius: var(--radius-sm);
  user-select: none;
}
.stack summary::-webkit-details-marker { display: none; }
.stack summary::before { content: '▸'; font-size: 9px; }
.stack[open] summary::before { content: '▾'; }
.stack summary:hover {
  background: var(--bg-hover);
  color: var(--text);
}
.stack pre {
  margin: 6px 0 0;
  padding: var(--space-2);
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  font-family: var(--font-mono);
  font-size: 10.5px;
  line-height: 1.5;
  color: var(--text-muted);
  white-space: pre;
  overflow: auto;
  max-height: 280px;
}
</style>