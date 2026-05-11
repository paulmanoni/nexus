<script setup>
import { computed } from 'vue'

// WorkerDetail is the drawer content for a clicked Worker node. Long-
// lived background tasks (cache invalidators, queue consumers,
// schedulers) — same dep model as a service but no HTTP traffic.
//
// Sections:
//   - Status pill                    — matches the canvas pill colours
//   - Last error                     — when the worker panicked or
//                                      returned non-nil
//   - Started/stopped timestamps
//   - Resource + service deps        — the constructor params the
//                                      worker took, as colour-tinted tags
//   - Actions placeholder            — restart / pause in a later pass
const props = defineProps({
  worker: { type: Object, required: true },
})

const w = computed(() => props.worker)

// Map worker lifecycle to the canonical status set so the pill shares
// vocabulary with the resource pill (healthy / error / inactive / …).
const pillState = computed(() => {
  switch (w.value.Status) {
    case 'running':  return 'healthy'
    case 'starting': return 'throttled'
    case 'failed':   return 'error'
    case 'stopped':  return 'inactive'
    default:         return 'inactive'
  }
})

const startedAt = computed(() => {
  const t = w.value.StartedAt
  if (!t) return ''
  try { return new Date(t).toLocaleString() } catch { return '' }
})
const stoppedAt = computed(() => {
  const t = w.value.StoppedAt
  if (!t) return ''
  try { return new Date(t).toLocaleString() } catch { return '' }
})

const resourceDeps = computed(() => Array.isArray(w.value.ResourceDeps) ? w.value.ResourceDeps : [])
const serviceDeps  = computed(() => Array.isArray(w.value.ServiceDeps)  ? w.value.ServiceDeps  : [])
</script>

<template>
  <div class="worker-detail">
    <!-- Status pill + lifecycle timestamps. Started/stopped comes from
         the registry's worker entry which is updated as fx lifecycle
         hooks fire. -->
    <section class="section">
      <h3>Status</h3>
      <div class="status-row">
        <span class="status" :class="pillState">
          <span class="dot" />
          {{ w.Status || 'unknown' }}
        </span>
      </div>
      <p v-if="w.Description" class="desc">{{ w.Description }}</p>
      <dl class="meta">
        <template v-if="startedAt">
          <dt>Started</dt>
          <dd>{{ startedAt }}</dd>
        </template>
        <template v-if="stoppedAt">
          <dt>Stopped</dt>
          <dd>{{ stoppedAt }}</dd>
        </template>
      </dl>
    </section>

    <!-- Last error — present only when the worker panicked or returned
         a non-nil error from its outer loop. -->
    <section v-if="w.LastError" class="section">
      <h3>Last error</h3>
      <div class="last-error">
        <code>{{ w.LastError }}</code>
      </div>
    </section>

    <!-- Dependencies — the constructor's resource + service params.
         Same tinted-tag styling as ServiceNode chip rows. -->
    <section v-if="resourceDeps.length || serviceDeps.length" class="section">
      <h3>Dependencies</h3>
      <div v-if="resourceDeps.length" class="dep-row">
        <span class="dep-kind">Reads</span>
        <code v-for="r in resourceDeps" :key="r" class="tag tag-res">{{ r }}</code>
      </div>
      <div v-if="serviceDeps.length" class="dep-row">
        <span class="dep-kind">Calls</span>
        <code v-for="s in serviceDeps" :key="s" class="tag tag-svc">{{ s }}</code>
      </div>
    </section>

    <!-- Actions placeholder — restart/pause once we wire those
         operations into the runtime. -->
    <section class="section">
      <h3>Actions</h3>
      <div class="placeholder">
        Restart and pause affordances coming soon.
      </div>
    </section>
  </div>
</template>

<style scoped>
.worker-detail {
  padding: var(--space-4) var(--space-5) var(--space-5);
  display: flex;
  flex-direction: column;
  gap: var(--space-5);
}
.section h3 {
  margin: 0 0 var(--space-2);
  font-size: var(--fs-md);
  font-weight: 600;
  color: var(--text);
}

.status-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}
.status {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: var(--fs-xs);
  font-weight: 500;
  padding: 3px 10px;
  border-radius: 999px;
  text-transform: capitalize;
}
.status .dot { width: 6px; height: 6px; border-radius: 50%; background: currentColor; }
.status.healthy   { background: var(--st-healthy-soft);   color: var(--st-healthy); }
.status.throttled { background: var(--st-throttled-soft); color: var(--st-throttled); }
.status.error     { background: var(--st-error-soft);     color: var(--st-error); }
.status.inactive  { background: var(--st-inactive-soft);  color: var(--st-inactive); }

.desc {
  margin: var(--space-2) 0 0;
  font-size: var(--fs-sm);
  color: var(--text-muted);
}

.meta {
  margin: var(--space-3) 0 0;
  display: grid;
  grid-template-columns: max-content 1fr;
  gap: 4px var(--space-3);
}
.meta dt {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-dim);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.meta dd {
  margin: 0;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text);
}

.last-error {
  padding: var(--space-3);
  background: var(--st-error-soft);
  border: 1px solid color-mix(in srgb, var(--st-error) 30%, transparent);
  border-radius: var(--radius-md);
}
.last-error code {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text);
  white-space: pre-wrap;
  word-break: break-word;
}

.dep-row {
  display: flex;
  align-items: baseline;
  flex-wrap: wrap;
  gap: var(--space-2);
  padding: var(--space-2) 0;
  border-bottom: 1px solid var(--border);
}
.dep-row:last-child { border-bottom: none; }
.dep-kind {
  font-size: var(--fs-xs);
  color: var(--text-dim);
  text-transform: uppercase;
  letter-spacing: 0.04em;
  min-width: 84px;
  flex-shrink: 0;
}
.tag {
  display: inline-flex;
  align-items: center;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  padding: 2px 8px;
  border-radius: 999px;
  background: var(--bg-hover);
  color: var(--text);
  border: 1px solid transparent;
}
.tag-svc {
  background: color-mix(in srgb, var(--cat-service) 10%, transparent);
  color: var(--cat-service);
  border-color: color-mix(in srgb, var(--cat-service) 25%, transparent);
}
.tag-res {
  background: color-mix(in srgb, var(--cat-database) 10%, transparent);
  color: var(--cat-database);
  border-color: color-mix(in srgb, var(--cat-database) 25%, transparent);
}

.placeholder {
  padding: var(--space-3);
  background: var(--bg-subtle);
  border: 1px dashed var(--border-strong);
  border-radius: var(--radius-md);
  font-size: var(--fs-sm);
  color: var(--text-muted);
  text-align: center;
}
</style>