<script setup>
import { ref, computed } from 'vue'
import { Play, CheckCircle2, XCircle, AlertTriangle } from 'lucide-vue-next'

const props = defineProps(['endpoint'])

const kind = props.endpoint.Method || 'query'
const args = computed(() => props.endpoint.Args || [])
const returnType = computed(() => props.endpoint.ReturnType || '')

const url = ref(props.endpoint.Path || '/graphql')
const query = ref(buildTemplate(kind, props.endpoint.Name, args.value, returnType.value))
const variables = ref(buildVars(args.value))
const response = ref(null)
const loading = ref(false)

function buildTemplate(kind, name, args, retType) {
  const varDefs = args.length
    ? '(' + args.map(a => `$${a.Name}: ${a.Type || 'String'}`).join(', ') + ')'
    : ''
  const argList = args.length
    ? '(' + args.map(a => `${a.Name}: $${a.Name}`).join(', ') + ')'
    : ''
  const opName = args.length ? ' ' + name.charAt(0).toUpperCase() + name.slice(1) : ''
  const selection = isCompositeType(retType) ? ' {\n    __typename\n  }' : ''
  return `${kind}${opName}${varDefs} {\n  ${name}${argList}${selection}\n}`
}

function buildVars(args) {
  if (!args.length) return '{}'
  const obj = {}
  for (const a of args) obj[a.Name] = defaultForType(a.Type)
  return JSON.stringify(obj, null, 2)
}

function defaultForType(t) {
  if (!t) return null
  const base = t.replace(/[!\[\]]/g, '')
  if (t.startsWith('[')) return []
  if (['Int', 'Float'].includes(base)) return 0
  if (base === 'Boolean') return false
  return ''
}

function isCompositeType(t) {
  if (!t) return false
  const base = t.replace(/[!\[\]]/g, '')
  return !['String', 'Int', 'Float', 'Boolean', 'ID'].includes(base)
}

function validatorChipLabel(v) {
  if (v.Kind === 'length') {
    const d = v.Details || {}
    if (d.min >= 0 && d.max >= 0) return `length ${d.min}–${d.max}`
    if (d.min >= 0) return `min ${d.min}`
    if (d.max >= 0) return `max ${d.max}`
    return 'length'
  }
  if (v.Kind === 'range') {
    const d = v.Details || {}
    return `range ${d.min}–${d.max}`
  }
  if (v.Kind === 'regex') {
    return 'pattern'
  }
  if (v.Kind === 'oneOf') {
    const d = v.Details || {}
    return `oneOf: ${(d.allowed || []).join(', ')}`
  }
  return v.Kind
}

async function send() {
  loading.value = true
  response.value = null
  try {
    let vars = {}
    try { vars = JSON.parse(variables.value) } catch { /* ignore */ }
    const start = performance.now()
    const r = await fetch(url.value, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ query: query.value, variables: vars })
    })
    const duration = Math.round(performance.now() - start)
    const data = await r.json()
    response.value = { status: r.status, duration, body: JSON.stringify(data, null, 2) }
  } catch (e) {
    response.value = { error: e.message }
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="tester">
    <div v-if="args.length || returnType" class="meta">
      <div v-if="args.length" class="args-panel">
        <div class="meta-label">Arguments</div>
        <div class="args-list">
          <div v-for="a in args" :key="a.Name" class="arg-row">
            <code class="arg-name">{{ a.Name }}</code>
            <code class="arg-type" :class="{ required: a.Required }">{{ a.Type }}</code>
            <div v-if="a.Validators && a.Validators.length" class="validators">
              <span
                v-for="v in a.Validators"
                :key="v.Kind + (v.Message || '')"
                class="val-chip"
                :class="v.Kind"
                :title="v.Message"
              >
                {{ validatorChipLabel(v) }}
              </span>
            </div>
          </div>
        </div>
      </div>
      <div v-if="returnType" class="returns-row">
        <span class="meta-label">Returns</span>
        <code class="arg-type">{{ returnType }}</code>
      </div>
    </div>

    <label>Endpoint URL</label>
    <input v-model="url" />
    <label>Query</label>
    <textarea v-model="query" rows="10"></textarea>
    <label>Variables</label>
    <textarea v-model="variables" rows="4"></textarea>
    <button class="primary run" @click="send" :disabled="loading">
      <Play :size="14" :stroke-width="2" />
      <span>{{ loading ? 'Running' : 'Run query' }}</span>
    </button>
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
.tester { display: flex; flex-direction: column; gap: 6px; }

.meta {
  padding: 14px;
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  margin-bottom: 10px;
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.args-panel .meta-label { margin-bottom: 8px; }
.meta-label {
  color: var(--text-muted);
  font-size: 10.5px;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  font-weight: 600;
}
.args-list { display: flex; flex-direction: column; gap: 8px; }
.arg-row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  padding: 6px 10px;
  background: var(--bg-subtle);
  border-radius: var(--radius-sm);
  border: 1px solid var(--border);
}
.arg-name {
  color: var(--text);
  font-family: var(--font-mono);
  font-size: 12px;
  font-weight: 600;
}
.arg-type {
  color: var(--graphql);
  background: var(--graphql-soft);
  padding: 1px 6px;
  border-radius: 3px;
  font-family: var(--font-mono);
  font-size: 11px;
  font-weight: 500;
}
.arg-type.required { box-shadow: inset 0 0 0 1px var(--graphql); }

.validators {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
  margin-left: auto;
}
.val-chip {
  display: inline-flex;
  align-items: center;
  font-family: var(--font-mono);
  font-size: 10.5px;
  padding: 1px 7px;
  border-radius: 10px;
  font-weight: 600;
  border: 1px solid transparent;
  letter-spacing: 0.02em;
}
.val-chip.required { background: #fee2e2; color: #b91c1c; border-color: #fecaca; }
.val-chip.length   { background: #e0e7ff; color: #3730a3; border-color: #c7d2fe; }
.val-chip.range    { background: #e0e7ff; color: #3730a3; border-color: #c7d2fe; }
.val-chip.regex    { background: #fef3c7; color: #92400e; border-color: #fde68a; }
.val-chip.oneOf    { background: #dbeafe; color: #1e40af; border-color: #bfdbfe; }
.val-chip.custom   { background: var(--bg-hover); color: var(--text); border-color: var(--border); }

.returns-row {
  display: flex;
  align-items: center;
  gap: 10px;
  padding-top: 10px;
  border-top: 1px solid var(--border);
}

label {
  color: var(--text-muted);
  font-size: 11px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  margin-top: 10px;
}
.run { align-self: flex-start; margin-top: 12px; }
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
.err { display: flex; align-items: center; gap: 6px; color: var(--error); font-size: 13px; }
</style>
