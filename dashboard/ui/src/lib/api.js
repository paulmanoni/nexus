export async function fetchConfig() {
  try {
    const r = await fetch('/__nexus/config')
    if (!r.ok) return { Name: 'Nexus' }
    return r.json()
  } catch {
    return { Name: 'Nexus' }
  }
}

export async function fetchEndpoints() {
  const r = await fetch('/__nexus/endpoints')
  if (!r.ok) throw new Error(`endpoints: ${r.status}`)
  return r.json()
}

export async function fetchResources() {
  const r = await fetch('/__nexus/resources')
  if (!r.ok) throw new Error(`resources: ${r.status}`)
  return r.json()
}

// fetchMiddlewares returns { middlewares: [Info...], global: [name...] }.
// The `global` slice is ordered — request hits them in that sequence on
// the engine root before any per-endpoint stack.
export async function fetchMiddlewares() {
  const r = await fetch('/__nexus/middlewares')
  if (!r.ok) throw new Error(`middlewares: ${r.status}`)
  return r.json()
}

export async function fetchCrons() {
  const r = await fetch('/__nexus/crons')
  if (!r.ok) throw new Error(`crons: ${r.status}`)
  return r.json()
}

export async function triggerCron(name) {
  const r = await fetch(`/__nexus/crons/${encodeURIComponent(name)}/trigger`, { method: 'POST' })
  if (!r.ok) throw new Error(`trigger ${name}: ${r.status}`)
  return r.json()
}

export async function setCronPaused(name, paused) {
  const action = paused ? 'pause' : 'resume'
  const r = await fetch(`/__nexus/crons/${encodeURIComponent(name)}/${action}`, { method: 'POST' })
  if (!r.ok) throw new Error(`${action} ${name}: ${r.status}`)
  return r.json()
}

export async function fetchRateLimits() {
  const r = await fetch('/__nexus/ratelimits')
  if (!r.ok) throw new Error(`ratelimits: ${r.status}`)
  return r.json()
}

export async function configureRateLimit(service, op, limit) {
  const r = await fetch(
    `/__nexus/ratelimits/${encodeURIComponent(service)}/${encodeURIComponent(op)}`,
    {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(limit),
    }
  )
  if (!r.ok) throw new Error(`configure ${service}.${op}: ${r.status}`)
  return r.json()
}

export async function resetRateLimit(service, op) {
  const r = await fetch(
    `/__nexus/ratelimits/${encodeURIComponent(service)}/${encodeURIComponent(op)}`,
    { method: 'DELETE' }
  )
  if (!r.ok) throw new Error(`reset ${service}.${op}: ${r.status}`)
  return r.json()
}

// fetchStats returns the per-endpoint metrics snapshot — request counts,
// error counts, and the last error's text/time. Architecture polls this
// so its per-op badges stay live. RecentErrors are NOT included; call
// fetchErrorEvents(service, op) on demand (dialog opens).
export async function fetchStats() {
  const r = await fetch('/__nexus/stats')
  if (!r.ok) throw new Error(`stats: ${r.status}`)
  return r.json()
}

// fetchErrorEvents returns the full ring of recent error events for a
// specific endpoint — time + IP + message per event, newest-first.
// Called on dialog open so the per-poll /stats payload stays small
// regardless of how many errors are retained.
export async function fetchErrorEvents(service, op) {
  const r = await fetch(
    `/__nexus/stats/${encodeURIComponent(service)}/${encodeURIComponent(op)}/errors`
  )
  if (!r.ok) throw new Error(`errors ${service}.${op}: ${r.status}`)
  return r.json()
}

// subscribeEvents opens /__nexus/events and auto-reconnects on close with
// exponential backoff. It tracks the highest seen event id so a reconnect
// resumes via the server's `since` query param — no gap, no duplicates.
//
// Returns a controller with .close(). onStatus is called with 'open' | 'closed'
// so the UI can show a connection indicator.
export function subscribeEvents(onEvent, onStatus, since = 0) {
  let socket = null
  let stopped = false
  let retryMs = 500
  let lastId = since
  const MAX_RETRY = 10000

  function connect() {
    if (stopped) return
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    socket = new WebSocket(`${proto}://${location.host}/__nexus/events?since=${lastId}`)
    socket.onopen = () => {
      retryMs = 500
      onStatus && onStatus('open')
    }
    socket.onmessage = ev => {
      try {
        const e = JSON.parse(ev.data)
        if (typeof e.id === 'number' && e.id > lastId) lastId = e.id
        onEvent(e)
      } catch (err) {
        console.error('bad event', err)
      }
    }
    socket.onclose = () => {
      socket = null
      onStatus && onStatus('closed')
      if (stopped) return
      setTimeout(connect, retryMs)
      retryMs = Math.min(retryMs * 2, MAX_RETRY)
    }
    socket.onerror = () => {
      try { socket && socket.close() } catch { /* ignore */ }
    }
  }

  connect()

  return {
    close() {
      stopped = true
      if (socket) {
        try { socket.close() } catch { /* ignore */ }
      }
    }
  }
}
