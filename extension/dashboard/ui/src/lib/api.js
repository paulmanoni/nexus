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

// configureRateLimit + resetRateLimit pass service + op as QUERY
// params for the same reason fetchErrorEvents does — REST op names
// like "GET /v1/api/users" contain slashes that break gin's
// path-param matching.
export async function configureRateLimit(service, op, limit) {
  const params = new URLSearchParams({ service, op })
  const r = await fetch(`/__nexus/ratelimits?${params}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(limit),
  })
  if (!r.ok) throw new Error(`configure ${service}.${op}: ${r.status}`)
  return r.json()
}

export async function resetRateLimit(service, op) {
  const params = new URLSearchParams({ service, op })
  const r = await fetch(`/__nexus/ratelimits?${params}`, { method: 'DELETE' })
  if (!r.ok) throw new Error(`reset ${service}.${op}: ${r.status}`)
  return r.json()
}

// fetchStats returns the per-endpoint metrics snapshot — request counts,
// error counts, and the last error's text/time. Architecture polls this
// so its per-op badges stay live. RecentErrors are NOT included; call
// fetchErrorEvents(service, op) on demand (dialog opens).
// fetchWorkers returns { workers: [...] } — one entry per AsWorker
// registration. Empty slice when no workers are wired.
export async function fetchWorkers() {
  const r = await fetch('/__nexus/workers')
  if (!r.ok) return { workers: [] }
  return r.json()
}

// fetchAuth returns { identities: [...], cachingEnabled: bool }, or null
// when auth isn't wired (the /__nexus/auth route returns 404). Null lets
// the Auth tab render a "not configured" state rather than flash an error.
export async function fetchAuth() {
  const r = await fetch('/__nexus/auth')
  if (r.status === 404) return null
  if (!r.ok) throw new Error(`auth: ${r.status}`)
  return r.json()
}

// invalidateAuth calls POST /__nexus/auth/invalidate with {id} or {token}.
// Returns { dropped: N } so the UI can confirm "logged out 3 sessions".
export async function invalidateAuth(body) {
  const r = await fetch('/__nexus/auth/invalidate', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  })
  if (!r.ok) throw new Error(`invalidate: ${r.status}`)
  return r.json()
}

// fetchTrace returns { traceId, spans: [...] } for a single trace. Spans
// carry { spanId, parentId, name, kind, service, endpoint, startMs,
// durationMs, status, error, remote, attrs } — startMs is relative to the
// trace's earliest event so the waterfall renders without knowing absolute
// clock. 404 ⇒ the trace has aged out of the ring buffer.
export async function fetchTrace(traceId) {
  const r = await fetch(`/__nexus/traces/${encodeURIComponent(traceId)}`)
  if (r.status === 404) return null
  if (!r.ok) throw new Error(`trace ${traceId}: ${r.status}`)
  return r.json()
}

export async function fetchStats() {
  const r = await fetch('/__nexus/stats')
  if (!r.ok) throw new Error(`stats: ${r.status}`)
  return r.json()
}

// fetchErrorEvents returns the full ring of recent error events for a
// specific endpoint — time + IP + message per event, newest-first.
// Called on dialog open so the per-poll /stats payload stays small
// regardless of how many errors are retained.
//
// service + op pass as QUERY params because REST op names are
// "<METHOD> <path>" — they contain spaces and slashes that gin's
// path-param matcher can't capture across segment boundaries.
export async function fetchErrorEvents(service, op) {
  const params = new URLSearchParams({ service, op })
  const r = await fetch(`/__nexus/stats/errors?${params}`)
  if (!r.ok) throw new Error(`errors ${service}.${op}: ${r.status}`)
  return r.json()
}

// subscribeLive opens /__nexus/live and consumes the unified state-snapshot
// stream that replaces the dashboard's 5-second polling fan-out. The server
// pushes a fresh snapshot every ~2s carrying { services, endpoints,
// resources, workers, stats, crons, ratelimits } — the UI subscribes once
// and re-renders on each frame.
//
// Auto-reconnects on close with exponential backoff. Returns a controller
// with .close(). onStatus is called with 'open' | 'closed' for connection-
// indicator UI.
export function subscribeLive(onSnapshot, onStatus) {
  let socket = null
  let stopped = false
  let retryMs = 500
  const MAX_RETRY = 10000

  function connect() {
    if (stopped) return
    const proto = location.protocol === 'https:' ? 'wss' : 'ws'
    socket = new WebSocket(`${proto}://${location.host}/__nexus/live`)
    socket.onopen = () => {
      retryMs = 500
      onStatus && onStatus('open')
    }
    socket.onmessage = ev => {
      try {
        const snap = JSON.parse(ev.data)
        if (snap && snap.kind === 'snapshot') onSnapshot(snap)
      } catch (err) {
        console.error('bad snapshot', err)
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
