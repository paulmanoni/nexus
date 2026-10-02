// console.js — the nexus dashboard console's only script. Pages are rendered
// on the server (templ); this keeps them current and makes their controls
// work:
//
//   live     /__nexus/live pushes a snapshot on every change → re-fetch the
//            page's live regions (X-Nexus-Partial) and morph them in place.
//            /__nexus/events does the same for pages that show trace
//            events (Traces, Auth's rejections: [data-events]).
//   actions  [data-post] buttons and form[data-json-form] call the JSON API.
//   search   input[data-query] keeps ?q= in the URL; the server filters,
//            sorts and pages big lists. input[data-filter] filters small
//            ones in the browser (rows' data-search).
//   tester   form[data-tester] sends a request to an endpoint.
//
// Nothing here owns state the server doesn't: a refresh is always safe.
(() => {
  'use strict'
  const PREFIX = '/__nexus'
  const $ = (sel, root = document) => root.querySelector(sel)
  const $$ = (sel, root = document) => [...root.querySelectorAll(sel)]
  const store = {
    get(k) { try { return sessionStorage.getItem(k) || '' } catch { return '' } },
    set(k, v) { try { v ? sessionStorage.setItem(k, v) : sessionStorage.removeItem(k) } catch {} },
  }

  // ---------- theme ----------
  document.addEventListener('click', (e) => {
    if (!e.target.closest('[data-theme-toggle]')) return
    const next = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark'
    document.documentElement.dataset.theme = next
    try { localStorage.setItem('nexus.theme', next) } catch {}
  })

  // ---------- toasts ----------
  function toast(msg, kind = 'ok') {
    const box = $('#nx-toasts')
    if (!box) return
    const el = document.createElement('div')
    el.className = 'nx-toast'
    el.dataset.kind = kind
    el.textContent = msg
    box.appendChild(el)
    setTimeout(() => el.remove(), kind === 'err' ? 7000 : 3000)
  }

  // ---------- relative times ----------
  // <time data-ago|data-until datetime=…> render as an absolute stamp on the
  // server; here they read "12s ago" / "in 4m" and keep counting.
  function span(ms) {
    const s = Math.round(ms / 1000)
    if (s < 60) return s + 's'
    const m = Math.floor(s / 60)
    if (m < 60) return m + 'm' + (m < 10 && s % 60 ? ' ' + (s % 60) + 's' : '')
    const h = Math.floor(m / 60)
    if (h < 48) return h + 'h' + (m % 60 ? ' ' + (m % 60) + 'm' : '')
    return Math.floor(h / 24) + 'd'
  }
  function tick() {
    const now = Date.now()
    for (const el of document.querySelectorAll('time[data-ago], time[data-until]')) {
      const t = Date.parse(el.getAttribute('datetime'))
      if (isNaN(t)) continue
      let text
      if (el.hasAttribute('data-ago')) text = now - t < 1000 ? 'just now' : span(now - t) + ' ago'
      else text = t - now <= 1000 ? 'now' : 'in ' + span(t - now)
      if (el.textContent !== text) el.textContent = text
    }
  }
  setInterval(tick, 1000)

  // ---------- morph ----------
  // Patch `from` to look like `to`, keeping what the operator is doing:
  // the focused field's value, <details> they opened, and anything marked
  // data-keep (the tester, the Architecture island).
  function morph(from, to) {
    if (from.nodeType !== to.nodeType || from.nodeName !== to.nodeName ||
        (from.nodeType === 1 && (from.id || '') !== (to.id || ''))) {
      from.replaceWith(to.cloneNode(true))
      return
    }
    if (from.nodeType !== 1) {
      if (from.nodeValue !== to.nodeValue) from.nodeValue = to.nodeValue
      return
    }
    if (from.hasAttribute('data-keep')) return
    // A ticking time keeps the text the browser gave it while its moment
    // is the same.
    if (from.tagName === 'TIME' && from.getAttribute('datetime') === to.getAttribute('datetime')) return
    const focused = from === document.activeElement
    for (const a of [...from.attributes]) {
      if (to.hasAttribute(a.name)) continue
      if (a.name === 'open' && from.tagName === 'DETAILS') continue
      if (a.name === 'data-filtered-out') continue
      from.removeAttribute(a.name)
    }
    for (const a of [...to.attributes]) {
      if (a.name === 'open' && from.tagName === 'DETAILS') continue
      if (focused && a.name === 'value') continue
      if (from.getAttribute(a.name) !== a.value) from.setAttribute(a.name, a.value)
    }
    if (from.tagName === 'INPUT' || from.tagName === 'TEXTAREA' || from.tagName === 'SELECT') return
    const a = [...from.childNodes]
    const b = [...to.childNodes]
    for (let i = 0; i < b.length; i++) {
      if (i < a.length) morph(a[i], b[i])
      else from.appendChild(b[i].cloneNode(true))
    }
    for (let i = b.length; i < a.length; i++) a[i].remove()
  }

  // ---------- live refresh ----------
  // One fetch at a time, at most every MIN_GAP ms; an unchanged view
  // answers 304 (the server hashes the regions it would send).
  let inflight = null
  let queued = false
  let last = 0
  let etag = ''
  let etagURL = ''
  const MIN_GAP = 800

  function refresh(now) {
    if (inflight) { queued = true; return }
    const wait = now ? 0 : last + MIN_GAP - Date.now()
    if (wait > 0) { queued = true; setTimeout(flush, wait); return }
    last = Date.now()
    const href = location.href
    const headers = { 'X-Nexus-Partial': '1' }
    if (etag && etagURL === href) headers['If-None-Match'] = etag
    inflight = fetch(href, { headers, credentials: 'same-origin', cache: 'no-store' })
      .then((r) => {
        setLive(true)
        if (r.status === 304) return
        etag = r.headers.get('ETag') || ''
        etagURL = href
        return r.text().then((html) => {
          if (href !== location.href) return // navigated meanwhile; the next one applies
          const doc = new DOMParser().parseFromString('<body>' + html + '</body>', 'text/html')
          const status = doc.getElementById('nx-status')
          if (status && $('#nx-status')) morph($('#nx-status'), status)
          const main = $('#nx-main')
          const next = doc.getElementById('nx-main')
          if (main && next && !main.hasAttribute('data-static') && main.dataset.tab === next.dataset.tab) {
            morph(main, next)
            applyFilters()
          }
          const title = doc.querySelector('title')
          if (title) document.title = title.textContent
          tick()
        })
      })
      .catch(() => setLive(false))
      .finally(() => { inflight = null; if (queued) flush() })
  }
  function flush() { queued = false; refresh() }

  // Links that stay on this tab (sort, pages, the group rail, segments)
  // swap the list in place instead of reloading the page.
  document.addEventListener('click', (e) => {
    const a = e.target.closest('a[href]')
    if (!a || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || a.target) return
    const main = $('#nx-main')
    if (!main || main.hasAttribute('data-static') || !main.contains(a)) return
    const url = new URL(a.href, location.href)
    if (url.origin !== location.origin || url.pathname !== location.pathname) return
    e.preventDefault()
    history.pushState(null, '', url)
    refresh(true)
  })
  window.addEventListener('popstate', () => refresh(true))

  // ?q= search: type, and the list re-renders on the server.
  let searchTimer = 0
  document.addEventListener('input', (e) => {
    const input = e.target.closest('input[data-query]')
    if (!input) return
    clearTimeout(searchTimer)
    searchTimer = setTimeout(() => {
      const url = new URL(location.href)
      const q = input.value.trim()
      q ? url.searchParams.set('q', q) : url.searchParams.delete('q')
      url.searchParams.delete('p')
      history.replaceState(null, '', url)
      refresh(true)
    }, 220)
  })

  function setLive(ok) {
    const dot = $('[data-live-dot]')
    const label = $('[data-live-label]')
    if (dot) dot.style.background = ok ? '' : 'var(--nx-warn)'
    if (label) label.textContent = ok ? 'Live' : 'Reconnecting…'
  }

  function socket(path, onMessage) {
    let delay = 500
    const open = () => {
      const ws = new WebSocket((location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + PREFIX + path)
      ws.onopen = () => { delay = 500; setLive(true) }
      ws.onmessage = onMessage
      ws.onclose = () => {
        setLive(false)
        setTimeout(open, delay)
        delay = Math.min(delay * 2, 8000)
      }
    }
    open()
  }

  // The first snapshot arrives right after connecting; the page is already
  // current then, so it is skipped.
  let primed = false
  socket('/live', () => {
    if (!primed) { primed = true; return }
    if (!document.hidden) refresh()
  })
  // A page that shows trace events names the kinds it shows (data-events);
  // it re-renders when one arrives.
  const kinds = new Set(($('[data-events]')?.dataset.events || '').split(/\s+/).filter(Boolean))
  if (kinds.size) {
    socket('/events', (m) => {
      try { if (!kinds.has(JSON.parse(m.data).kind)) return } catch { return }
      if (!document.hidden) refresh()
    })
  }
  document.addEventListener('visibilitychange', () => { if (!document.hidden) refresh(true) })

  // ---------- filter ----------
  const filterKey = () => 'nexus.filter:' + location.pathname
  function applyFilters() {
    for (const input of $$('input[data-filter]')) {
      const root = $(input.dataset.filter)
      if (!root) continue
      const q = input.value.trim().toLowerCase()
      for (const row of $$('[data-search]', root)) {
        const hit = !q || q.split(/\s+/).every((t) => row.dataset.search.includes(t))
        row.toggleAttribute('data-filtered-out', !hit)
      }
    }
  }
  document.addEventListener('input', (e) => {
    const input = e.target.closest('input[data-filter]')
    if (!input) return
    store.set(filterKey(), input.value)
    applyFilters()
  })
  // A service's endpoint count links to Endpoints filtered by that service.
  document.addEventListener('click', (e) => {
    const a = e.target.closest('a[data-filter-link]')
    if (a) store.set('nexus.filter:' + new URL(a.href).pathname, a.dataset.filterLink)
  })
  function restoreFilter() {
    const input = $('input[data-filter]')
    if (!input) return
    input.value = store.get(filterKey())
    applyFilters()
  }

  // ---------- row links ----------
  document.addEventListener('click', (e) => {
    const row = e.target.closest('[data-href]')
    if (!row || e.target.closest('a,button,input,textarea,select,summary,details')) return
    if (e.metaKey || e.ctrlKey) window.open(row.dataset.href, '_blank')
    else location.href = row.dataset.href
  })

  // ---------- actions ----------
  async function call(url, method, body, success) {
    try {
      const r = await fetch(url, {
        method,
        credentials: 'same-origin',
        headers: body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
        body,
      })
      const text = await r.text()
      if (!r.ok) {
        let msg = text
        try { msg = JSON.parse(text).error || text } catch {}
        throw new Error(msg || r.status + ' ' + r.statusText)
      }
      if (success) toast(success)
      refresh(true)
      return true
    } catch (err) {
      toast(err.message || String(err), 'err')
      return false
    }
  }

  document.addEventListener('click', async (e) => {
    const btn = e.target.closest('[data-post]')
    if (!btn || btn.disabled) return
    e.preventDefault()
    if (btn.dataset.confirm && !confirm(btn.dataset.confirm)) return
    btn.disabled = true
    await call(btn.dataset.post, btn.dataset.method || 'POST', btn.dataset.body, btn.dataset.success)
    btn.disabled = false
  })

  document.addEventListener('submit', async (e) => {
    const form = e.target.closest('form[data-json-form]')
    if (!form) return
    e.preventDefault()
    const body = {}
    for (const el of form.elements) {
      if (!el.name) continue
      if (el.dataset.type === 'bool') body[el.name] = el.checked
      else if (el.dataset.type === 'number') body[el.name] = el.value === '' ? 0 : Number(el.value)
      else if (el.value !== '') body[el.name] = el.value
    }
    const ok = await call(form.dataset.action, form.dataset.method || 'POST', JSON.stringify(body), form.dataset.success)
    if (ok && form.dataset.reset !== undefined) form.reset()
  })

  // ---------- endpoint tester ----------
  function show(form, status, text, kind) {
    const out = $('[data-tester-output]', form)
    const st = $('[data-tester-status]', form)
    st.textContent = status
    st.style.color = kind === 'err' ? 'var(--nx-err)' : kind === 'ok' ? 'var(--nx-ok)' : ''
    out.classList.toggle('hidden', text === null)
    if (text !== null) out.textContent = text
  }
  const pretty = (text) => { try { return JSON.stringify(JSON.parse(text), null, 2) } catch { return text } }
  const parseJSON = (s, what) => {
    if (!s.trim()) return undefined
    try { return JSON.parse(s) } catch (err) { throw new Error(what + ' is not valid JSON: ' + err.message) }
  }

  async function sendHTTP(form, kind) {
    const f = form.elements
    let url = f.url.value.trim()
    let init
    if (kind === 'graphql') {
      const variables = parseJSON(f.variables.value, 'Variables') || {}
      init = { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ query: f.query.value, variables }) }
    } else {
      init = { method: f.method.value, headers: parseJSON(f.headers.value, 'Headers') || {} }
      if (f.body && f.body.value.trim()) init.body = f.body.value
    }
    init.credentials = 'same-origin'
    show(form, 'Sending…', null)
    const t0 = performance.now()
    const r = await fetch(url, init)
    const text = await r.text()
    const ms = Math.round(performance.now() - t0)
    let failed = !r.ok
    if (kind === 'graphql') { try { failed = failed || !!JSON.parse(text).errors } catch {} }
    show(form, r.status + ' ' + r.statusText + ' · ' + ms + 'ms', pretty(text) || '(empty body)', failed ? 'err' : 'ok')
  }

  function sendWS(form) {
    const f = form.elements
    const data = parseJSON(f.data.value, 'Data')
    const lines = []
    const log = (s) => { lines.push(s); show(form, 'Connected', lines.join('\n')) }
    const path = f.url.value.trim()
    const ws = new WebSocket((location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + path)
    show(form, 'Connecting…', null)
    ws.onopen = () => {
      const msg = JSON.stringify({ type: f.type.value, data: data ?? {}, timestamp: Date.now() })
      log('→ ' + msg)
      ws.send(msg)
      setTimeout(() => ws.close(), 5000)
    }
    ws.onmessage = (m) => log('← ' + m.data)
    ws.onerror = () => show(form, 'Socket error', lines.join('\n') || null, 'err')
    ws.onclose = (ev) => show(form, 'Closed (' + ev.code + ') after 5s', lines.join('\n') || '(no frames)', ev.code === 1000 || ev.code === 1005 ? 'ok' : 'err')
  }

  document.addEventListener('submit', async (e) => {
    const form = e.target.closest('form[data-tester]')
    if (!form) return
    e.preventDefault()
    try {
      if (form.dataset.tester === 'websocket') sendWS(form)
      else await sendHTTP(form, form.dataset.tester)
    } catch (err) {
      show(form, 'Request failed', err.message || String(err), 'err')
    }
  })

  const ready = () => { restoreFilter(); tick() }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', ready)
  else ready()
})()
