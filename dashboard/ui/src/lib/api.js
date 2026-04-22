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

export async function fetchMiddlewares() {
  const r = await fetch('/__nexus/middlewares')
  if (!r.ok) throw new Error(`middlewares: ${r.status}`)
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
