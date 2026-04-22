<script setup>
import { ref, onUnmounted } from 'vue'
import { Plug, PlugZap, Send, ArrowDownLeft, ArrowUpRight, Circle } from 'lucide-vue-next'

const props = defineProps(['endpoint'])

const url = ref(props.endpoint.Path || '')
const outgoing = ref('')
const messages = ref([])
const connected = ref(false)
let ws = null

function connect() {
  if (ws) { ws.close(); ws = null }
  const proto = location.protocol === 'https:' ? 'wss' : 'ws'
  ws = new WebSocket(`${proto}://${location.host}${url.value}`)
  ws.onopen = () => { connected.value = true; log('meta', 'connected') }
  ws.onclose = () => { connected.value = false; log('meta', 'closed') }
  ws.onerror = () => log('meta', 'error')
  ws.onmessage = e => log('in', e.data)
}

function disconnect() {
  if (ws) { ws.close(); ws = null }
}

function send() {
  if (!ws || ws.readyState !== 1) return
  ws.send(outgoing.value)
  log('out', outgoing.value)
  outgoing.value = ''
}

function log(dir, text) {
  messages.value.push({ dir, text, t: new Date() })
  if (messages.value.length > 500) messages.value.splice(0, messages.value.length - 500)
}

onUnmounted(disconnect)
</script>

<template>
  <div class="tester">
    <div class="row">
      <input v-model="url" />
      <button v-if="!connected" class="primary" @click="connect">
        <Plug :size="14" :stroke-width="2" />
        <span>Connect</span>
      </button>
      <button v-else @click="disconnect">
        <PlugZap :size="14" :stroke-width="2" />
        <span>Disconnect</span>
      </button>
    </div>
    <div class="msgs">
      <div v-if="!messages.length" class="empty-msgs">No messages yet.</div>
      <div v-for="(m, i) in messages" :key="i" :class="['msg', m.dir]">
        <component
          :is="m.dir === 'in' ? ArrowDownLeft : m.dir === 'out' ? ArrowUpRight : Circle"
          :size="12"
          :stroke-width="2"
          class="dir-icon"
        />
        <span class="text">{{ m.text }}</span>
      </div>
    </div>
    <div class="row">
      <input
        v-model="outgoing"
        placeholder="Type a message and press Enter"
        @keydown.enter="send"
        :disabled="!connected"
      />
      <button @click="send" :disabled="!connected">
        <Send :size="14" :stroke-width="2" />
        <span>Send</span>
      </button>
    </div>
  </div>
</template>

<style scoped>
.tester { display: flex; flex-direction: column; gap: 10px; height: 100%; }
.row { display: flex; gap: 8px; }
.msgs {
  flex: 1;
  min-height: 240px;
  max-height: 55vh;
  overflow-y: auto;
  padding: 10px 12px;
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  font-family: var(--font-mono);
  font-size: 12px;
  display: flex;
  flex-direction: column;
  gap: 4px;
  box-shadow: var(--shadow-sm);
}
.empty-msgs { color: var(--text-dim); padding: 20px; text-align: center; }
.msg { display: flex; align-items: center; gap: 8px; padding: 3px 0; }
.msg.meta { color: var(--text-dim); }
.msg.in { color: var(--success); }
.msg.out { color: var(--accent); }
.dir-icon { flex-shrink: 0; }
.text { word-break: break-all; }
</style>
