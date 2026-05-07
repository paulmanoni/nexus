// Nexus client SDK — runtime ESM module served from the Go binary
// at <client-path>/client.js. Single file, no build step, no
// external dependencies; works in any modern browser via
//   <script type="module"> import { NexusClient } from '/__nexus/client/client.js' </script>
// or as a regular ESM import in a bundler.
//
// Design points:
//   1. Manifest-driven. The SDK fetches /__nexus/client/manifest.json
//      lazily on first call and caches it for the page lifetime;
//      every other call resolves URLs / args from the manifest, so
//      client code never hardcodes route paths.
//   2. Pluggable fetch + token store so the same module runs in the
//      browser, in jsdom tests, and in SSR contexts where window
//      globals are absent.
//   3. Path-param substitution mirrors Go's reflect-based binder:
//      :name in the route path is replaced with args[name] before
//      the rest of args is shaped into a query string (GET) or JSON
//      body (non-GET).
//   4. WS dispatch matches the framework's wire envelope:
//        outbound: { type, data, timestamp }
//        inbound:  { type, data }
//      Per-path one socket; per-type handlers via .on(type, fn).

const DEFAULT_MANIFEST_PATH = '/__nexus/client/manifest.json'

// -- Errors --------------------------------------------------------

export class NexusError extends Error {
  /**
   * @param {string} message
   * @param {object} extra
   * @param {number} [extra.status]   HTTP status (REST), graphql code (GQL)
   * @param {string} [extra.code]     framework-side error code if surfaced
   * @param {any}    [extra.payload]  raw decoded response body
   * @param {string} [extra.endpoint] endpoint identifier for context
   */
  constructor(message, extra = {}) {
    super(message)
    this.name = 'NexusError'
    this.status = extra.status
    this.code = extra.code
    this.payload = extra.payload
    this.endpoint = extra.endpoint
  }
}

// -- Token storage -------------------------------------------------

/**
 * memoryTokenStore is the SSR-safe / test-safe default. Tokens live
 * for the lifetime of the JS module; cleared on page reload.
 */
export function memoryTokenStore() {
  let v = null
  return {
    get: () => v,
    set: (t) => { v = t },
    clear: () => { v = null },
  }
}

/**
 * localStorageTokenStore persists tokens across reloads. Falls back
 * to memory storage when localStorage isn't available (private mode
 * lockdown, SSR). The fallback keeps the SDK from crashing on
 * environments that disable storage — auth still works in-tab.
 */
export function localStorageTokenStore(key = 'nexus.token') {
  try {
    if (typeof localStorage === 'undefined') throw new Error('no localStorage')
    // Probe write — Safari private mode throws here, not on load.
    localStorage.setItem(key + '.probe', '1')
    localStorage.removeItem(key + '.probe')
  } catch {
    return memoryTokenStore()
  }
  return {
    get: () => localStorage.getItem(key),
    set: (t) => t ? localStorage.setItem(key, t) : localStorage.removeItem(key),
    clear: () => localStorage.removeItem(key),
  }
}

// -- NexusClient ---------------------------------------------------

/**
 * NexusClient is the root SDK handle. Construct once per page; all
 * REST / GraphQL / WS / CRUD / auth flows hang off the same instance
 * so they share the manifest fetch, the token store, and the fetch
 * implementation.
 *
 * @example
 *   import { NexusClient } from '/__nexus/client/client.js'
 *   const nx = new NexusClient()
 *   const pets = await nx.rest('GET', '/pets')
 *   await nx.auth.login({ username: 'alice', password: 'hunter2' })
 *   const ws = nx.ws('/events').on('chat.message', m => console.log(m))
 *   ws.connect()
 */
export class NexusClient {
  constructor(opts = {}) {
    this.origin = opts.origin ?? (typeof location !== 'undefined' ? location.origin : '')
    this.manifestPath = opts.manifestPath ?? DEFAULT_MANIFEST_PATH
    this._fetch = opts.fetch ?? (typeof fetch !== 'undefined' ? fetch.bind(globalThis) : null)
    this._tokenStore = opts.tokenStore ?? localStorageTokenStore()
    // opts.manifest seeds the cache so ready() never makes a network
    // request. Production-friendly: the bundler inlines the
    // sdk/manifest.json file at build time, so the prod bundle has
    // zero /__nexus/client/* traffic. Also kills CORS issues from
    // SPAs on a different origin than the API.
    this._manifest = opts.manifest ?? null
    this._loadingManifest = null
    this.auth = new AuthNamespace(this)
  }

  /**
   * ready resolves with the manifest, fetching it on first call and
   * caching for subsequent ones. Useful when client code wants to
   * branch on the schema before firing requests (e.g. "is this
   * endpoint declared?"), but most callers don't need to await it
   * directly — every internal method calls it transparently.
   *
   * @returns {Promise<object>}
   */
  async ready() {
    if (this._manifest) return this._manifest
    if (!this._loadingManifest) {
      this._loadingManifest = this._fetchManifest().finally(() => { this._loadingManifest = null })
    }
    return this._loadingManifest
  }

  async _fetchManifest() {
    if (!this._fetch) {
      throw new NexusError('nexus: no fetch available — pass opts.fetch in non-browser environments')
    }
    const r = await this._fetch(this.origin + this.manifestPath)
    if (!r.ok) {
      throw new NexusError(`nexus: manifest fetch failed (${r.status})`, { status: r.status })
    }
    this._manifest = await r.json()
    return this._manifest
  }

  /** Force a re-fetch of the manifest. Rare — useful in dev hot-reload. */
  async reload() {
    this._manifest = null
    this._loadingManifest = null
    return this.ready()
  }

  /**
   * rest invokes a REST endpoint. Path-param tokens (:name) in path
   * are pulled from args and removed; remaining args become the
   * query string (GET) or the JSON body (non-GET).
   *
   * @param {string} method  HTTP verb (GET/POST/PATCH/PUT/DELETE)
   * @param {string} path    route path, possibly with :params
   * @param {object} [args]  args bag — params + query + body in one
   * @param {object} [opts]  per-call overrides ({ headers, signal })
   * @returns {Promise<any>} parsed JSON body, or null on 204
   */
  async rest(method, path, args = {}, opts = {}) {
    const m = await this.ready()
    method = method.toUpperCase()

    // 1. Substitute :params and strip them from the args bag so
    //    they don't bleed into the query string / body.
    const remaining = { ...args }
    const filledPath = path.replace(/:([a-zA-Z0-9_]+)/g, (_, name) => {
      if (!(name in remaining)) {
        throw new NexusError(`nexus: missing path param :${name} for ${method} ${path}`)
      }
      const v = remaining[name]
      delete remaining[name]
      return encodeURIComponent(String(v))
    })

    // 2. Build URL: origin + manifest.basePath + filled path.
    const url = this._url(filledPath, method === 'GET' ? remaining : null)

    // 3. Build init. Auth header attached via _auth — manifest's
    //    extractor strategy decides whether to use Bearer vs cookie.
    const init = {
      method,
      headers: { 'Accept': 'application/json', ...(opts.headers || {}) },
      signal: opts.signal,
    }
    this._authorize(init, m)

    if (method !== 'GET' && Object.keys(remaining).length > 0) {
      init.headers['Content-Type'] = 'application/json'
      init.body = JSON.stringify(remaining)
    }

    const r = await this._fetch(url, init)
    return this._handleResponse(r, `${method} ${path}`)
  }

  /**
   * query runs a GraphQL query op by name. The manifest's
   * graphql-transport endpoints declare op names; the SDK looks up
   * the path of the GraphQL mount the op lives on so apps with
   * multi-mount schemas (per-module Path) work transparently.
   */
  async query(name, variables = {}, opts = {}) {
    return this._gql('query', name, variables, opts)
  }

  /** mutate runs a GraphQL mutation by name. Same shape as query(). */
  async mutate(name, variables = {}, opts = {}) {
    return this._gql('mutation', name, variables, opts)
  }

  async _gql(kind, name, variables, opts) {
    const m = await this.ready()
    const ep = (m.endpoints || []).find(e => e.transport === 'graphql' && e.method === kind && e.name === name)
    if (!ep) {
      throw new NexusError(`nexus: no GraphQL ${kind} named ${name}`, { endpoint: name })
    }
    const url = this._url(ep.path)
    const init = {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Accept': 'application/json', ...(opts.headers || {}) },
      signal: opts.signal,
      body: JSON.stringify({
        query: buildGqlDocument(kind, name, variables, ep, m.refs || {}),
        variables,
        operationName: capitalize(name),
      }),
    }
    this._authorize(init, m)
    const r = await this._fetch(url, init)
    const body = await r.json()
    if (body.errors && body.errors.length) {
      throw new NexusError(body.errors[0].message, { payload: body, endpoint: name })
    }
    // Return data[name] when present (the typical single-field
    // response); fall back to the whole data object so multi-field
    // queries still surface.
    if (body.data && Object.keys(body.data).length === 1) {
      return body.data[Object.keys(body.data)[0]]
    }
    return body.data
  }

  /**
   * crud returns a CRUD handle scoped to a registered AsCRUD entity
   * by plural name (e.g. nx.crud('pets').list()). The handle uses
   * the conventional REST routes (GET /<plural>, GET /<plural>/:id,
   * POST /<plural>, PATCH /<plural>/:id, DELETE /<plural>/:id).
   */
  crud(name) {
    return new CrudHandle(this, name)
  }

  /**
   * ws returns a WebSocket handle for the given path. One socket per
   * (client, path); .on(type, handler) registers a per-message-type
   * dispatcher; .send(type, data) emits a framework envelope.
   */
  ws(path) {
    return new WSHandle(this, path)
  }

  /** _url builds origin + manifest.basePath + path + ?query. */
  _url(path, query) {
    const base = (this._manifest && this._manifest.basePath) || ''
    let u = this.origin + base + path
    if (query && Object.keys(query).length > 0) {
      const params = new URLSearchParams()
      for (const [k, v] of Object.entries(query)) {
        if (v == null) continue
        if (Array.isArray(v)) v.forEach(x => params.append(k, String(x)))
        else params.append(k, String(v))
      }
      u += (u.includes('?') ? '&' : '?') + params.toString()
    }
    return u
  }

  /**
   * _authorize attaches the auth header / cookie credentials to a
   * fetch init based on the manifest's extractor strategy. No-op
   * when no token is set OR when the auth section is absent (public
   * apps).
   */
  _authorize(init, manifest) {
    const auth = manifest?.auth
    if (!auth) return
    const tok = this._tokenStore.get()
    switch (auth.strategy) {
      case 'bearer':
        if (tok) init.headers[auth.headerName || 'Authorization'] = 'Bearer ' + tok
        break
      case 'apikey':
        if (tok) init.headers[auth.headerName || 'X-API-Key'] = tok
        break
      case 'cookie':
        // Browser sends cookies automatically when we opt in; nothing
        // to add to headers. credentials:'include' covers cross-origin.
        init.credentials = 'include'
        break
      case 'chain':
        // Best-effort: try Bearer first, then cookie credentials. App
        // can call .auth.setToken() to drive the bearer side.
        if (tok) init.headers['Authorization'] = 'Bearer ' + tok
        init.credentials = 'include'
        break
      case 'custom':
        // App-supplied extractor — we don't know the shape. Fall
        // back to credentials:'include' so cookie-based custom
        // schemes still work; bearer-style apps must pass tokens
        // via opts.headers per-call.
        init.credentials = 'include'
        break
    }
  }

  async _handleResponse(r, endpoint) {
    if (r.status === 204) return null
    const ct = r.headers.get('Content-Type') || ''
    let body
    if (ct.includes('application/json')) {
      body = await r.json()
    } else {
      body = await r.text()
    }
    if (!r.ok) {
      const msg = (body && typeof body === 'object' && body.error) || `HTTP ${r.status}`
      throw new NexusError(msg, { status: r.status, payload: body, endpoint })
    }
    return body
  }
}

// -- AuthNamespace -------------------------------------------------

class AuthNamespace {
  constructor(client) { this._c = client }

  /** Current token from the configured store, or null. */
  get token() { return this._c._tokenStore.get() }

  /** Stash a token in the store. Used after login() / external auth flows. */
  setToken(t) { this._c._tokenStore.set(t) }

  /** Drop the cached token. Used after logout() and on 401 retry-once flows. */
  clearToken() { this._c._tokenStore.clear() }

  /**
   * login dispatches the credentials to the auth flow declared by
   * nexus.AuthRoute("login"). Transport is auto-selected from the
   * manifest: REST endpoints go through nx.rest('POST', loginPath),
   * GraphQL mutations go through nx.mutate(loginName). On a response
   * carrying a `token` field the SDK auto-stashes it via the token
   * store. Returns the full response so apps can surface additional
   * fields (refresh token, expiry, profile snapshot).
   */
  async login(creds) {
    const m = await this._c.ready()
    if (!m.auth?.loginPath) {
      throw new NexusError('nexus: app declares no login route — use nexus.AuthRoute("login") on a REST endpoint or GraphQL mutation')
    }
    const r = (m.auth.loginTransport === 'graphql')
      ? await this._c._gql('mutation', m.auth.loginName, creds, {})
      : await this._c.rest('POST', m.auth.loginPath, creds)
    if (r && typeof r === 'object' && typeof r.token === 'string') {
      this.setToken(r.token)
    }
    return r
  }

  /**
   * logout dispatches to the auth.logout flow when declared, then
   * clears the local token. Transport-aware like login. Logout
   * always clears the local token even when the route call fails —
   * the user's intent is "I'm done".
   */
  async logout() {
    const m = await this._c.ready()
    try {
      if (m.auth?.logoutPath) {
        if (m.auth.logoutTransport === 'graphql') {
          await this._c._gql('mutation', m.auth.logoutName, {}, {})
        } else {
          await this._c.rest('POST', m.auth.logoutPath)
        }
      }
    } finally {
      this.clearToken()
    }
  }

  /** me fetches the current Identity from the auth.me flow. */
  async me() {
    const m = await this._c.ready()
    if (!m.auth?.mePath) {
      throw new NexusError('nexus: app declares no /me route — use nexus.AuthRoute("me") on a REST endpoint or GraphQL query')
    }
    if (m.auth.meTransport === 'graphql') {
      return this._c._gql('query', m.auth.meName, {}, {})
    }
    return this._c.rest('GET', m.auth.mePath)
  }
}

// -- CrudHandle ----------------------------------------------------

class CrudHandle {
  constructor(client, name) {
    this._c = client
    this._name = name
    this._base = '/' + name
  }

  list(args = {}) { return this._c.rest('GET', this._base, args) }
  get(id)         { return this._c.rest('GET', `${this._base}/:id`, { id }) }
  create(body)    { return this._c.rest('POST', this._base, body) }
  update(id, body){ return this._c.rest('PATCH', `${this._base}/:id`, { id, ...body }) }
  delete(id)      { return this._c.rest('DELETE', `${this._base}/:id`, { id }) }
}

// -- WSHandle ------------------------------------------------------

class WSHandle {
  constructor(client, path) {
    this._c = client
    this._path = path
    this._sock = null
    this._handlers = new Map()
    this._sendQueue = []
    this._reconnect = false
  }

  /** Register a handler for messages of envelope type. Chainable. */
  on(type, handler) {
    this._handlers.set(type, handler)
    return this
  }

  /**
   * send emits a framework envelope ({type, data, timestamp}).
   * Buffered when the socket isn't open yet; flushed on connect.
   */
  send(type, data) {
    const env = { type, data, timestamp: Math.floor(Date.now() / 1000) }
    if (this._sock && this._sock.readyState === 1) {
      this._sock.send(JSON.stringify(env))
    } else {
      this._sendQueue.push(env)
    }
    return this
  }

  /**
   * connect opens the WebSocket and starts dispatching incoming
   * envelopes by type. Returns this for chaining; the socket is
   * available on .socket after open.
   */
  async connect() {
    const m = await this._c.ready()
    const origin = this._c.origin
    const proto = origin.startsWith('https:') ? 'wss:' : 'ws:'
    const host = origin.replace(/^https?:/, '')
    const url = proto + host + (m.basePath || '') + this._path
    const sock = new WebSocket(url)
    this._sock = sock
    sock.onopen = () => {
      while (this._sendQueue.length > 0) {
        sock.send(JSON.stringify(this._sendQueue.shift()))
      }
    }
    sock.onmessage = (ev) => {
      let env
      try { env = JSON.parse(ev.data) } catch { return }
      const h = this._handlers.get(env.type)
      if (h) h(env.data, env)
      // Wildcard handler — the dashboard uses this for activity rails.
      const wild = this._handlers.get('*')
      if (wild) wild(env.data, env)
    }
    sock.onclose = () => {
      this._sock = null
      const h = this._handlers.get('@close')
      if (h) h()
    }
    sock.onerror = (e) => {
      const h = this._handlers.get('@error')
      if (h) h(e)
    }
    return this
  }

  /** close terminates the socket cleanly. Pending queued sends drop. */
  close() {
    this._sendQueue = []
    if (this._sock) try { this._sock.close() } catch {}
    this._sock = null
  }

  /** Live socket — null until connect() opens it. */
  get socket() { return this._sock }
}

// -- GraphQL document builder --------------------------------------
//
// Builds a minimal GraphQL document from the manifest's endpoint
// info. Selection sets on object returns auto-expand into __typename +
// every field reachable through manifest.refs, recursing through
// nested refs, inline objects, and array elements. Cycle-safe via a
// per-walk visited set; depth-bounded so self-referential types can't
// blow up the document size.
//
// nx.query / nx.mutate intentionally take NO field-selection option —
// the SDK fetches everything reachable within the depth cap. Apps
// that need a tighter or hand-tuned selection set can call
// client.rest against the GraphQL endpoint directly with a
// hand-written document.
//
// SELECTION_MAX_DEPTH bounds how deep the auto-expansion walks. 3
// matches "the field, its sub-objects, and their sub-objects" —
// covers the common Response[Data{User, ...}] shape without
// accidentally pulling N-deep relation graphs.

const SELECTION_MAX_DEPTH = 3

function buildGqlDocument(kind, name, variables, ep, refs) {
  const argDefs = []
  const argList = []
  for (const k of Object.keys(variables)) {
    // The manifest doesn't carry SDL types in EndpointInfo.Args
    // (those are TypeRef-based); fall back to inferred shape.
    argDefs.push(`$${k}: ${inferGqlType(variables[k])}`)
    argList.push(`${k}: $${k}`)
  }
  const sig = argDefs.length ? `(${argDefs.join(', ')})` : ''
  const args = argList.length ? `(${argList.join(', ')})` : ''
  const opName = capitalize(name)
  const selection = buildSelectionSet(ep && ep.return, refs || {})
  return `${kind} ${opName}${sig} { ${name}${args}${selection} }`
}

// buildSelectionSet returns the leading-space selection-set fragment
// (" { a b c }") or "" when the return type is scalar/unknown.
function buildSelectionSet(typeRef, refs) {
  const inner = walkSelection(typeRef, refs, new Set(), SELECTION_MAX_DEPTH)
  return inner ? ' ' + inner : ''
}

function walkSelection(typeRef, refs, seenRefs, depth) {
  if (!typeRef) return ''
  switch (typeRef.kind) {
    case 'primitive':
    case 'any':
      // Leaves of the schema — no selection set.
      return ''
    case 'array':
    case 'list':
      // GraphQL puts the selection on the element, not the wrapper.
      // Lists of primitives unwind to '' so we don't request a sub-
      // selection on a [String] field (which the schema rejects).
      return walkSelection(typeRef.of, refs, seenRefs, depth)
    case 'map':
      // GraphQL has no map type; the framework degrades these to a
      // scalar-shaped JSON blob, so no selection.
      return ''
    case 'ref': {
      const refName = typeRef.ref
      if (!refName) return '{ __typename }'
      // Cycle break + depth cap both fall back to __typename — keeps
      // the document syntactically valid (an object field MUST carry
      // a selection set) without recursing further.
      if (seenRefs.has(refName) || depth <= 0) return '{ __typename }'
      const nt = refs[refName]
      if (!nt || !nt.fields) return '{ __typename }'
      const next = new Set(seenRefs)
      next.add(refName)
      return renderObjectSelection(nt.fields, refs, next, depth - 1)
    }
    case 'object':
      if (!typeRef.object || !typeRef.object.fields) return '{ __typename }'
      if (depth <= 0) return '{ __typename }'
      return renderObjectSelection(typeRef.object.fields, refs, seenRefs, depth - 1)
    default:
      return '{ __typename }'
  }
}

function renderObjectSelection(fields, refs, seenRefs, depth) {
  const parts = ['__typename']
  for (const f of fields) {
    const fieldName = wireFieldName(f)
    if (!fieldName) continue
    const sub = walkSelection(f.type, refs, seenRefs, depth)
    parts.push(sub ? `${fieldName} ${sub}` : fieldName)
  }
  return `{ ${parts.join(' ')} }`
}

// wireFieldName resolves a field's wire name with the same priority
// the server's schema walker uses: graphql tag wins for output
// selections, json tag is the fallback, and the lower-cased Go name
// is the last resort (matching graphql-go's default field naming).
function wireFieldName(f) {
  if (f.graphqlName) return f.graphqlName
  if (f.jsonName) return f.jsonName
  if (!f.name) return ''
  return f.name.charAt(0).toLowerCase() + f.name.slice(1)
}

function inferGqlType(v) {
  if (v == null) return 'String'
  switch (typeof v) {
    case 'string':  return 'String!'
    case 'number':  return Number.isInteger(v) ? 'Int!' : 'Float!'
    case 'boolean': return 'Boolean!'
    case 'object':  return Array.isArray(v) ? '[String!]!' : 'String!' // structured args degrade to String
    default:        return 'String'
  }
}

function capitalize(s) {
  return s ? s.charAt(0).toUpperCase() + s.slice(1) : s
}

// Default export so `import NexusClient from ...` works alongside
// `import { NexusClient } from ...`.
export default NexusClient
