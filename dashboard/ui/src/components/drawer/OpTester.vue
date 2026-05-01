<script setup>
import { ref, computed, onUnmounted } from 'vue'
import { Send, Play, Plug, CheckCircle2, XCircle, AlertTriangle } from 'lucide-vue-next'

// OpTester is the single tester used by every op detail drawer. Picks
// the right form based on op.Transport (rest / graphql / websocket),
// auto-populates inputs from the op's declared metadata (path, args,
// type), validates client-side as much as possible, then submits and
// renders the response inline.
//
// Replaces the trio of RestTester.vue / GraphQLTester.vue / WsTester.vue
// — once nothing references those, they can be deleted.
const props = defineProps({
  op: { type: Object, required: true },
})

const transport = computed(() => props.op.Transport || 'rest')

// ─── Shared response state ─────────────────────────────────────────
// The response panel is identical across transports (status pill,
// latency, pretty body). REST + GraphQL push here on send/error;
// WebSocket pushes a frame log instead.
const loading = ref(false)
const response = ref(null) // { status, duration, body, error?, headers? }

function setError(message) {
  response.value = { error: message }
  loading.value = false
}

// ─── REST ──────────────────────────────────────────────────────────
const restPath = ref(props.op.Path || '')
const restHeaders = ref('{\n  "Content-Type": "application/json"\n}')
const restBody = ref('')
const restMethod = computed(() => (props.op.Method || 'GET').toUpperCase())
const hasRestBody = computed(() => ['POST', 'PUT', 'PATCH', 'DELETE'].includes(restMethod.value))
// Path params surface as a hint above the URL input — every :name
// segment in the registered path becomes one. Lets the user spot which
// inputs the URL needs without parsing the path themselves.
const restPathParams = computed(() => {
  const out = []
  const re = /:([a-zA-Z0-9_]+)/g
  let m
  while ((m = re.exec(props.op.Path || '')) !== null) out.push(m[1])
  return out
})

async function sendRest() {
  loading.value = true
  response.value = null
  try {
    let parsedHeaders = {}
    try { parsedHeaders = JSON.parse(restHeaders.value) } catch { /* tolerate stale JSON */ }
    const init = { method: restMethod.value, headers: parsedHeaders }
    if (hasRestBody.value && restBody.value.trim()) init.body = restBody.value
    const start = performance.now()
    const r = await fetch(restPath.value, init)
    const duration = Math.round(performance.now() - start)
    const text = await r.text()
    let body = text
    try { body = JSON.stringify(JSON.parse(text), null, 2) } catch { /* keep raw */ }
    response.value = { status: r.status, duration, body }
  } catch (e) {
    setError(e.message)
    return
  }
  loading.value = false
}

// ─── GraphQL ───────────────────────────────────────────────────────
// Pre-fills the query template + a variables object from op.Args. Same
// builder as the legacy tester so users get the same starting point.
const gqlKind = computed(() => props.op.Method || 'query')
const gqlArgs = computed(() => Array.isArray(props.op.Args) ? props.op.Args : [])
const gqlReturnType = computed(() => props.op.ReturnType || '')
const gqlPath = ref(props.op.Path || '/graphql')
const gqlQuery = ref('')
const gqlVars = ref('')
// Build / rebuild the template when the op identity changes (drawer
// switches from one op to another without unmounting).
function rebuildGqlTemplate() {
  gqlQuery.value = buildGqlTemplate(gqlKind.value, props.op.Name || '', gqlArgs.value, gqlReturnType.value)
  gqlVars.value = buildGqlVars(gqlArgs.value)
}
if (transport.value === 'graphql') rebuildGqlTemplate()

function buildGqlTemplate(kind, name, args, retType) {
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
function buildGqlVars(args) {
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

async function sendGql() {
  loading.value = true
  response.value = null
  try {
    let vars = {}
    try { vars = JSON.parse(gqlVars.value) } catch { /* tolerate stale JSON */ }
    const start = performance.now()
    const r = await fetch(gqlPath.value, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ query: gqlQuery.value, variables: vars }),
    })
    const duration = Math.round(performance.now() - start)
    const data = await r.json()
    response.value = { status: r.status, duration, body: JSON.stringify(data, null, 2) }
  } catch (e) {
    setError(e.message)
    return
  }
  loading.value = false
}

// ─── WebSocket ─────────────────────────────────────────────────────
// Minimal first cut: connect/disconnect, send a JSON envelope of
// { type: msgType, data: ... }, log received frames. Power features
// (per-frame waterfall, room joining) come later.
const wsPath = ref(props.op.Path || '/events')
const wsType = ref(props.op.Name || '')
const wsPayload = ref('{}')
const wsConn = ref(null)
const wsState = ref('idle') // 'idle' | 'connecting' | 'open' | 'closed' | 'error'
const wsLog = ref([])

function wsConnect() {
  if (wsConn.value) return
  wsState.value = 'connecting'
  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  const url = `${proto}://${location.host}${wsPath.value}`
  let s
  try { s = new WebSocket(url) } catch (e) { wsState.value = 'error'; wsLog.value.push({ kind: 'error', text: e.message, t: Date.now() }); return }
  wsConn.value = s
  s.onopen = () => { wsState.value = 'open' }
  s.onmessage = (e) => {
    let txt = e.data
    try { txt = JSON.stringify(JSON.parse(e.data), null, 2) } catch { /* keep raw */ }
    wsLog.value.push({ kind: 'recv', text: txt, t: Date.now() })
  }
  s.onerror = () => { wsState.value = 'error' }
  s.onclose = () => { wsState.value = 'closed'; wsConn.value = null }
}
function wsDisconnect() {
  if (wsConn.value) {
    try { wsConn.value.close() } catch { /* ignore */ }
  }
}
function wsSend() {
  if (!wsConn.value || wsState.value !== 'open') return
  let data = wsPayload.value
  try { data = JSON.parse(wsPayload.value) } catch { /* send as raw */ }
  const envelope = wsType.value ? { type: wsType.value, data } : data
  const frame = JSON.stringify(envelope)
  wsConn.value.send(frame)
  let pretty = frame
  try { pretty = JSON.stringify(JSON.parse(frame), null, 2) } catch { /* keep */ }
  wsLog.value.push({ kind: 'sent', text: pretty, t: Date.now() })
}
function wsClearLog() { wsLog.value = [] }

onUnmounted(() => {
  if (wsConn.value) {
    try { wsConn.value.close() } catch { /* ignore */ }
  }
})

// ─── Helpers ───────────────────────────────────────────────────────
function statusOK(s) { return typeof s === 'number' && s < 400 }
function fmtTime(t) {
  try { return new Date(t).toLocaleTimeString() } catch { return '' }
}
</script>

<template>
  <div class="op-tester">
    <!-- REST ─────────────────────────────────────────────────────── -->
    <template v-if="transport === 'rest'">
      <div class="row">
        <span class="method" :class="restMethod.toLowerCase()">{{ restMethod }}</span>
        <input v-model="restPath" class="path" />
        <button class="primary send" @click="sendRest" :disabled="loading">
          <Send :size="13" :stroke-width="2" />
          {{ loading ? 'Sending' : 'Send' }}
        </button>
      </div>
      <div v-if="restPathParams.length" class="hint">
        Path params: <code v-for="p in restPathParams" :key="p">:{{ p }}</code>
      </div>
      <label>Headers</label>
      <textarea v-model="restHeaders" rows="3" spellcheck="false"></textarea>
      <template v-if="hasRestBody">
        <label>Body</label>
        <textarea v-model="restBody" rows="6" placeholder="{}" spellcheck="false"></textarea>
      </template>
    </template>

    <!-- GraphQL ──────────────────────────────────────────────────── -->
    <template v-else-if="transport === 'graphql'">
      <div class="row">
        <span class="method graphql">{{ gqlKind.toUpperCase() }}</span>
        <input v-model="gqlPath" class="path" />
        <button class="primary send" @click="sendGql" :disabled="loading">
          <Play :size="13" :stroke-width="2" />
          {{ loading ? 'Running' : 'Run' }}
        </button>
      </div>
      <label>Query</label>
      <textarea v-model="gqlQuery" rows="6" spellcheck="false" class="mono"></textarea>
      <label>Variables</label>
      <textarea v-model="gqlVars" rows="4" spellcheck="false" class="mono"></textarea>
    </template>

    <!-- WebSocket ────────────────────────────────────────────────── -->
    <template v-else-if="transport === 'websocket'">
      <div class="row">
        <span class="method ws">WS</span>
        <input v-model="wsPath" class="path" :disabled="wsState === 'open'" />
        <button
          v-if="wsState !== 'open'"
          class="primary send"
          @click="wsConnect"
          :disabled="wsState === 'connecting'"
        >
          <Plug :size="13" :stroke-width="2" />
          {{ wsState === 'connecting' ? 'Connecting' : 'Connect' }}
        </button>
        <button v-else class="send" @click="wsDisconnect">
          <Plug :size="13" :stroke-width="2" />
          Disconnect
        </button>
      </div>
      <div class="ws-status">
        <span class="ws-pill" :class="wsState">{{ wsState }}</span>
        <span v-if="wsType" class="ws-type">type: <code>{{ wsType }}</code></span>
      </div>
      <label>Payload</label>
      <textarea v-model="wsPayload" rows="4" spellcheck="false" class="mono" placeholder="{}"></textarea>
      <div class="row align-end">
        <button class="primary send" @click="wsSend" :disabled="wsState !== 'open'">
          <Send :size="13" :stroke-width="2" />
          Send frame
        </button>
        <button v-if="wsLog.length" class="send ghost" @click="wsClearLog">Clear log</button>
      </div>
      <div v-if="wsLog.length" class="ws-log">
        <div v-for="(f, i) in wsLog" :key="i" class="ws-frame" :class="f.kind">
          <div class="ws-frame-head">
            <span class="ws-frame-kind">{{ f.kind }}</span>
            <span class="ws-frame-time">{{ fmtTime(f.t) }}</span>
          </div>
          <pre>{{ f.text }}</pre>
        </div>
      </div>
    </template>

    <!-- Response panel — shared by REST + GraphQL -->
    <div v-if="response && transport !== 'websocket'" class="response">
      <div v-if="response.error" class="resp-err">
        <XCircle :size="14" :stroke-width="2" />
        <span>{{ response.error }}</span>
      </div>
      <div v-else>
        <div class="status-row">
          <span class="status" :class="statusOK(response.status) ? 'ok' : 'fail'">
            <component
              :is="statusOK(response.status) ? CheckCircle2 : XCircle"
              :size="13"
              :stroke-width="2.2"
            />
            {{ response.status }}
          </span>
          <span class="latency">{{ response.duration }} ms</span>
        </div>
        <pre>{{ response.body }}</pre>
      </div>
    </div>
  </div>
</template>

<style scoped>
.op-tester {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

.row {
  display: flex;
  gap: var(--space-2);
  align-items: center;
}
.row.align-end { align-self: flex-end; gap: var(--space-2); }

/* Method pill — green REST, pink GraphQL, amber WS — matches the
   transport hue tokens used everywhere else. */
.method {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  font-weight: 700;
  padding: 6px 10px;
  border-radius: var(--radius-sm);
  letter-spacing: 0.04em;
  min-width: 70px;
  text-align: center;
  flex-shrink: 0;
}
.method.get,
.method                { background: var(--rest-soft);    color: var(--rest); }
.method.post           { background: var(--ws-soft);      color: var(--ws); }
.method.put            { background: var(--accent-soft);  color: var(--accent); }
.method.patch          { background: var(--graphql-soft); color: var(--graphql); }
.method.delete         { background: var(--st-error-soft); color: var(--st-error); }
.method.graphql        { background: var(--graphql-soft); color: var(--graphql); }
.method.ws             { background: var(--ws-soft);      color: var(--ws); }

.path {
  flex: 1;
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
}

.send {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 6px 12px;
  font-size: var(--fs-sm);
}
.send.primary {
  background: var(--accent);
  color: white;
  border-color: var(--accent);
}
.send.primary:hover { background: var(--accent-hover); }
.send.primary:disabled { opacity: 0.6; cursor: not-allowed; }
.send.ghost {
  background: transparent;
  border: 1px solid var(--border);
  color: var(--text-muted);
}

label {
  font-size: var(--fs-xs);
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.06em;
  color: var(--text-muted);
  margin-top: var(--space-2);
}
textarea {
  resize: vertical;
  min-height: 60px;
}
textarea.mono {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
}

.hint {
  font-size: var(--fs-xs);
  color: var(--text-dim);
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
  align-items: center;
}
.hint code {
  font-family: var(--font-mono);
  background: var(--bg-hover);
  padding: 1px 6px;
  border-radius: var(--radius-sm);
  color: var(--text);
}

/* WS-specific bits */
.ws-status {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  margin-top: var(--space-1);
}
.ws-pill {
  display: inline-flex;
  align-items: center;
  font-size: var(--fs-xs);
  font-weight: 500;
  padding: 2px 8px;
  border-radius: 999px;
  text-transform: capitalize;
  background: var(--st-inactive-soft);
  color: var(--st-inactive);
}
.ws-pill.connecting { background: var(--st-throttled-soft); color: var(--st-throttled); }
.ws-pill.open       { background: var(--st-healthy-soft);   color: var(--st-healthy); }
.ws-pill.error      { background: var(--st-error-soft);     color: var(--st-error); }
.ws-pill.closed     { background: var(--bg-hover);          color: var(--text-muted); }
.ws-type {
  font-size: var(--fs-xs);
  color: var(--text-dim);
}
.ws-type code {
  font-family: var(--font-mono);
  color: var(--text);
}

.ws-log {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  max-height: 300px;
  overflow-y: auto;
}
.ws-frame {
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--bg-subtle);
  overflow: hidden;
}
.ws-frame.recv { border-color: color-mix(in srgb, var(--st-healthy) 25%, var(--border)); }
.ws-frame.sent { border-color: color-mix(in srgb, var(--accent) 25%, var(--border)); }
.ws-frame.error { border-color: color-mix(in srgb, var(--st-error) 25%, var(--border)); }
.ws-frame-head {
  display: flex;
  justify-content: space-between;
  padding: 4px 8px;
  font-size: 10px;
  font-family: var(--font-mono);
  color: var(--text-dim);
  text-transform: uppercase;
  letter-spacing: 0.06em;
}
.ws-frame.recv .ws-frame-kind { color: var(--st-healthy); }
.ws-frame.sent .ws-frame-kind { color: var(--accent); }
.ws-frame.error .ws-frame-kind { color: var(--st-error); }
.ws-frame pre {
  margin: 0;
  padding: 0 8px 8px;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  white-space: pre-wrap;
  word-break: break-word;
}

/* Response panel */
.response {
  margin-top: var(--space-3);
  padding: var(--space-3);
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: var(--radius-md);
}
.status-row {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  margin-bottom: var(--space-2);
}
.status {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  font-weight: 600;
  padding: 3px 10px;
  border-radius: 999px;
}
.status.ok   { background: var(--st-healthy-soft); color: var(--st-healthy); }
.status.fail { background: var(--st-error-soft);   color: var(--st-error); }
.latency {
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  color: var(--text-dim);
  font-variant-numeric: tabular-nums;
}
.response pre {
  margin: 0;
  max-height: 360px;
  overflow: auto;
  padding: var(--space-3);
  background: var(--bg-subtle);
  border-radius: var(--radius-sm);
  font-family: var(--font-mono);
  font-size: var(--fs-xs);
  white-space: pre-wrap;
  word-break: break-word;
}
.resp-err {
  display: flex;
  align-items: center;
  gap: 6px;
  color: var(--st-error);
  font-size: var(--fs-sm);
}
</style>