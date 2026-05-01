<script setup>
import { computed } from 'vue'

// ResourceDetail is the drawer content for a clicked Resource node
// (DB / cache / queue / other). Surfaces:
//   - Health pill                    — green/red, matches the canvas dot
//   - Description                    — server-supplied human text
//   - Details map                    — engine, schema, backend, ttl, …
//   - Attached services              — which service constructors took
//                                      this resource as a dep, derived
//                                      from the registry's attached map
//   - Actions placeholder            — recheck / drain in a later pass
//
// The resource is passed live from Architecture.vue via a computed that
// re-resolves on each /__nexus/live snapshot, so health and details
// update in place while the drawer is open.
const props = defineProps({
  resource: { type: Object, required: true },
})

const r = computed(() => props.resource)

const detailEntries = computed(() => {
  const d = r.value.details || {}
  return Object.entries(d)
})

const attachedTo = computed(() => Array.isArray(r.value.attachedTo) ? r.value.attachedTo : [])
</script>

<template>
  <div class="resource-detail">
    <!-- Health summary — the headline answer to "is it up?" -->
    <section class="section">
      <h3>Health</h3>
      <div class="health-row">
        <span class="status" :class="{ healthy: r.healthy, error: !r.healthy }">
          <span class="dot" />
          {{ r.healthy ? 'healthy' : 'down' }}
        </span>
        <span class="kind-label">{{ r.kind }}</span>
      </div>
      <p v-if="r.description" class="desc">{{ r.description }}</p>
    </section>

    <!-- Details map — engine, backend, broker, schema, etc. The raw
         data comes from each Resource's NexusResources() call, so it
         varies per kind; we render whatever's there. -->
    <section v-if="detailEntries.length" class="section">
      <h3>Details</h3>
      <dl class="details">
        <template v-for="[k, v] in detailEntries" :key="k">
          <dt>{{ k }}</dt>
          <dd>{{ v }}</dd>
        </template>
      </dl>
    </section>

    <!-- Attached services — which service / worker constructors took
         this resource as a dep. One tag per attachment. -->
    <section class="section">
      <h3>Attached to</h3>
      <div v-if="attachedTo.length" class="tags">
        <code v-for="name in attachedTo" :key="name" class="tag tag-svc">{{ name }}</code>
      </div>
      <div v-else class="empty">Not currently attached to any service.</div>
    </section>

    <!-- Actions placeholder — recheck / pause-on-failure / drain in a
         later pass. Kept visible so the drawer's surface area is stable
         across resource kinds. -->
    <section class="section">
      <h3>Actions</h3>
      <div class="placeholder">
        Recheck health and drain affordances coming soon.
      </div>
    </section>
  </div>
</template>

<style scoped>
.resource-detail {
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

.health-row {
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
}
.status .dot { width: 6px; height: 6px; border-radius: 50%; background: currentColor; }
.status.healthy { background: var(--st-healthy-soft); color: var(--st-healthy); }
.status.error   { background: var(--st-error-soft);   color: var(--st-error); }
.kind-label {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-dim);
  text-transform: lowercase;
}
.desc {
  margin: var(--space-2) 0 0;
  font-size: var(--fs-sm);
  color: var(--text-muted);
}

/* Definition-list layout for the details map. Keys mono+muted, values
   readable mono. Two-column auto-grid; long values wrap. */
.details {
  margin: 0;
  display: grid;
  grid-template-columns: max-content 1fr;
  gap: 6px var(--space-3);
}
.details dt {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-dim);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.details dd {
  margin: 0;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text);
  word-break: break-word;
}

.tags {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-1);
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

.empty, .placeholder {
  padding: var(--space-3);
  background: var(--bg-subtle);
  border: 1px dashed var(--border-strong);
  border-radius: var(--radius-md);
  font-size: var(--fs-sm);
  color: var(--text-muted);
}
.placeholder { text-align: center; }
</style>