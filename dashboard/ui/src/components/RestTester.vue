<script setup>
import { ref, computed } from 'vue'
import { Send, CheckCircle2, XCircle } from 'lucide-vue-next'

const props = defineProps(['endpoint'])

const method = computed(() => props.endpoint.Method || 'GET')
const path = ref(props.endpoint.Path || '')
const headers = ref('{\n  "Content-Type": "application/json"\n}')
const body = ref('')
const response = ref(null)
const loading = ref(false)

const hasBody = computed(() => ['POST', 'PUT', 'PATCH'].includes(method.value))

async function send() {
  loading.value = true
  response.value = null
  try {
    let parsedHeaders = {}
    try { parsedHeaders = JSON.parse(headers.value) } catch { /* ignore */ }
    const init = { method: method.value, headers: parsedHeaders }
    if (hasBody.value && body.value.trim()) init.body = body.value
    const start = performance.now()
    const r = await fetch(path.value, init)
    const duration = Math.round(performance.now() - start)
    const text = await r.text()
    let parsed = text
    try { parsed = JSON.stringify(JSON.parse(text), null, 2) } catch { /* ignore */ }
    response.value = { status: r.status, duration, body: parsed }
  } catch (e) {
    response.value = { error: e.message }
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="tester">
    <div class="row">
      <span class="method" :class="method.toLowerCase()">{{ method }}</span>
      <input v-model="path" />
      <button class="primary" @click="send" :disabled="loading">
        <Send :size="14" :stroke-width="2" />
        <span>{{ loading ? 'Sending' : 'Send' }}</span>
      </button>
    </div>
    <label>Headers</label>
    <textarea v-model="headers" rows="3"></textarea>
    <template v-if="hasBody">
      <label>Body</label>
      <textarea v-model="body" rows="6" placeholder="{}"></textarea>
    </template>
    <div v-if="response" class="response">
      <div v-if="response.error" class="err">
        <XCircle :size="14" :stroke-width="2" />
        <span>{{ response.error }}</span>
      </div>
      <div v-else>
        <div class="status-row">
          <div class="status" :class="response.status < 400 ? 'ok' : 'fail'">
            <component :is="response.status < 400 ? CheckCircle2 : XCircle" :size="14" :stroke-width="2" />
            <span>{{ response.status }}</span>
          </div>
          <span class="dim">{{ response.duration }}ms</span>
        </div>
        <pre>{{ response.body }}</pre>
      </div>
    </div>
  </div>
</template>

<style scoped>
.tester { display: flex; flex-direction: column; gap: 8px; }
.row { display: flex; gap: 8px; align-items: center; }
.method {
  font-weight: 700;
  padding: 7px 11px;
  border-radius: var(--radius);
  font-family: var(--font-mono);
  font-size: 12px;
  letter-spacing: 0.02em;
  background: var(--rest-soft);
  color: var(--rest);
  min-width: 72px;
  text-align: center;
}
.method.post { background: var(--ws-soft); color: var(--ws); }
.method.put { background: var(--accent-soft); color: var(--accent); }
.method.patch { background: var(--graphql-soft); color: var(--graphql); }
.method.delete { background: var(--error-soft); color: var(--error); }

label {
  color: var(--text-muted);
  font-size: 11px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  margin-top: 10px;
}

.response {
  margin-top: 16px;
  padding: 14px;
  background: var(--bg-card);
  border-radius: var(--radius);
  border: 1px solid var(--border);
  box-shadow: var(--shadow-sm);
}
.status-row { display: flex; gap: 14px; margin-bottom: 10px; align-items: center; }
.status {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-weight: 600;
  font-size: 12px;
  padding: 3px 8px;
  border-radius: 12px;
}
.status.ok { background: var(--success-soft); color: var(--success); }
.status.fail { background: var(--error-soft); color: var(--error); }
.dim { color: var(--text-dim); font-size: 12px; }
.response pre {
  margin: 0;
  overflow-x: auto;
  max-height: 420px;
  background: var(--bg-subtle);
  padding: 12px;
  border-radius: var(--radius-sm);
  font-family: var(--font-mono);
  font-size: 12px;
}
.err {
  display: flex;
  align-items: center;
  gap: 6px;
  color: var(--error);
  font-size: 13px;
}
</style>
