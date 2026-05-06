// Nexus client SDK — Vue 3 composables. Served at
// <client-path>/vue.js. Plain ESM, no build step.
//
// Imports `ref`, `watch`, `unref`, `computed`, `onUnmounted` from
// 'vue'. The host app must resolve 'vue' (a bundler entry, an
// importmap, or a CDN ESM URL) — keeping this file dependency-free
// would force a hard pin on a Vue version inside the SDK, which is
// the wrong tradeoff for a framework consumed by N apps.
//
// Each composable is a thin reactive wrapper over the underlying
// NexusClient method. They share ONE module-level NexusClient
// instance so manifest fetches + auth state are deduplicated across
// every component on the page; pass an explicit client (or
// constructor opts) to useNexus() the first time if you need a
// custom origin / fetch / token store.
//
// Example:
//
//   <script setup>
//   import { useQuery, useCrud, useAuth } from '/__nexus/client/vue.js'
//
//   const auth = useAuth()
//   const pets = useCrud('pets', { wsPath: '/events' })
//   const search = ref('')
//   const filtered = useQuery('GET', '/pets', () => ({ q: search.value }))
//   </script>

import { ref, watch, unref, computed, onUnmounted } from 'vue'
import { NexusClient, localStorageTokenStore, memoryTokenStore } from './client.js'

// -- Shared client -------------------------------------------------

let _shared = null

// envDefaults reads Vite-style env vars (harmless under any other
// bundler — import.meta.env is just absent and falls back to {}).
// VITE_NEXUS_API   → origin override (trailing slashes stripped).
// VITE_NEXUS_TOKEN → localStorage key for the bearer token; default
//                    'access_token' so it pairs with the common
//                    Pinia auth-store convention.
//
// Returns a NexusClient-options object. Explicit caller opts on
// useNexus() always win — this only fills holes. Calling
// useNexus() with no opts in a Vite app picks up VITE_NEXUS_* and
// constructs a same-origin / localStorage-backed client without
// any wiring file.
function envDefaults() {
  const env = (typeof import.meta !== 'undefined' && import.meta.env) || {}
  const origin = String(env.VITE_NEXUS_API ?? '').replace(/\/+$/, '')
  const tokenKey = String(env.VITE_NEXUS_TOKEN ?? 'access_token')
  return {
    origin: origin || undefined,
    tokenStore:
      typeof window === 'undefined'
        ? memoryTokenStore()
        : localStorageTokenStore(tokenKey),
  }
}

/**
 * useNexus returns the page-shared NexusClient. The first caller's
 * opts seed the singleton; subsequent calls ignore opts and return
 * the existing client. Apps that need multiple clients (e.g. two
 * backends) construct a `new NexusClient(...)` directly and pass it
 * to the other composables via the `client` option.
 *
 * Default construction picks up VITE_NEXUS_API (origin) and
 * VITE_NEXUS_TOKEN (localStorage key) so a bare `useNexus()` works
 * in a typical Vite app with zero wiring. Explicit opts always
 * override the env-derived defaults field-by-field.
 *
 * @param {object} [opts]  forwarded to NexusClient constructor on
 *                         first call only.
 * @returns {NexusClient}
 */
export function useNexus(opts) {
  if (!_shared) _shared = new NexusClient({ ...envDefaults(), ...(opts || {}) })
  return _shared
}

/**
 * setNexus replaces the shared client. Useful in tests / SSR
 * harnesses that want to swap in a stub. Production code never
 * needs this.
 */
export function setNexus(client) { _shared = client }

// resolve picks the active client for a composable call: explicit
// override on opts wins over the shared singleton.
function resolve(opts) {
  return (opts && opts.client) || useNexus()
}

// readArgs unwraps a getter / ref / plain-value into a snapshot
// suitable for passing to nx.rest(). Lets callers pass any of:
//   useQuery('GET', '/pets', { limit: 20 })
//   useQuery('GET', '/pets', argsRef)
//   useQuery('GET', '/pets', () => ({ limit: search.value }))
function readArgs(args) {
  if (typeof args === 'function') return args()
  return unref(args) ?? {}
}

// -- useQuery (REST) -----------------------------------------------

/**
 * useQuery wraps a REST call as reactive state. Re-fires the call
 * whenever args (deep-watched) changes. Returns:
 *
 *   { data, error, loading, refresh, abort }
 *
 * data starts as null. error / loading reset on each call. refresh
 * forces a refetch with current args; abort cancels an in-flight
 * call (the next refresh starts a new one).
 *
 * @param {string} method            HTTP verb
 * @param {string} path              route path; :params resolved from args
 * @param {object|Ref|()=>object} [args]
 * @param {object} [opts]            { client, headers, watch: false }
 */
export function useQuery(method, path, args, opts = {}) {
  const nx = resolve(opts)
  const data = ref(null)
  const error = ref(null)
  const loading = ref(false)
  let inflight = null

  async function refresh() {
    abort()
    const ac = new AbortController()
    inflight = ac
    loading.value = true
    error.value = null
    try {
      const a = readArgs(args)
      data.value = await nx.rest(method, path, a, { headers: opts.headers, signal: ac.signal })
    } catch (e) {
      if (e?.name !== 'AbortError') error.value = e
    } finally {
      if (inflight === ac) inflight = null
      loading.value = false
    }
  }

  function abort() {
    if (inflight) {
      try { inflight.abort() } catch {}
      inflight = null
    }
  }

  // Deep-watch args so reactive bags trigger a refetch on change.
  // Pass watch:false to disable auto-refresh — caller drives via
  // refresh() manually.
  if (opts.watch !== false) {
    watch(() => readArgs(args), () => refresh(), { deep: true, immediate: true })
  } else {
    refresh()
  }
  onUnmounted(abort)

  return { data, error, loading, refresh, abort }
}

// -- useMutation (REST) --------------------------------------------

/**
 * useMutation wraps a write-style REST call. Manual-fire — does not
 * run on mount, fires only when mutate() is called. Returns:
 *
 *   { mutate, data, error, loading }
 *
 * mutate(args?, opts?) returns the response body and updates data.
 * Throws on error so try/catch in the caller works naturally;
 * error.value is also populated for template-side conditionals.
 */
export function useMutation(method, path, opts = {}) {
  const nx = resolve(opts)
  const data = ref(null)
  const error = ref(null)
  const loading = ref(false)

  async function mutate(args, callOpts) {
    loading.value = true
    error.value = null
    try {
      const r = await nx.rest(method, path, args, { ...opts, ...callOpts })
      data.value = r
      return r
    } catch (e) {
      error.value = e
      throw e
    } finally {
      loading.value = false
    }
  }

  return { mutate, data, error, loading }
}

// -- useGqlQuery / useGqlMutation ----------------------------------

/**
 * useGqlQuery wraps a GraphQL query op (registered via
 * nexus.AsQuery). Same shape as useQuery — deep-watches args,
 * exposes { data, error, loading, refresh }.
 */
export function useGqlQuery(name, args, opts = {}) {
  const nx = resolve(opts)
  const data = ref(null)
  const error = ref(null)
  const loading = ref(false)

  async function refresh() {
    loading.value = true
    error.value = null
    try {
      data.value = await nx.query(name, readArgs(args))
    } catch (e) {
      error.value = e
    } finally {
      loading.value = false
    }
  }
  if (opts.watch !== false) {
    watch(() => readArgs(args), () => refresh(), { deep: true, immediate: true })
  } else {
    refresh()
  }
  return { data, error, loading, refresh }
}

/**
 * useGqlMutation wraps a GraphQL mutation. Manual-fire; same shape
 * as useMutation.
 */
export function useGqlMutation(name, opts = {}) {
  const nx = resolve(opts)
  const data = ref(null)
  const error = ref(null)
  const loading = ref(false)

  async function mutate(vars) {
    loading.value = true
    error.value = null
    try {
      const r = await nx.mutate(name, vars)
      data.value = r
      return r
    } catch (e) {
      error.value = e
      throw e
    } finally {
      loading.value = false
    }
  }
  return { mutate, data, error, loading }
}

// -- useCrud -------------------------------------------------------

/**
 * useCrud is the reactive CRUD list. Wraps NexusClient.crud(name)
 * with a refreshing items array, optimistic-update helpers, and
 * optional WS-backed live updates.
 *
 * Optional WS subscription:
 *   - When opts.wsPath is set (or defaults to '/' + name when
 *     opts.subscribe !== false), the composable opens a WebSocket
 *     and listens for three message types matching the entity:
 *       <name>.created  → push to items
 *       <name>.updated  → replace by id
 *       <name>.deleted  → filter by id
 *     Apps emit these from their own AsWS handlers when an entity
 *     mutates; the dashboard's CRUD reactivity story is built on
 *     this convention. Not opinionated — opts.subscribe = false
 *     skips the WS leg entirely.
 *
 * @returns {{ items, loading, error, refresh, create, update, remove, ws }}
 */
export function useCrud(name, opts = {}) {
  const nx = resolve(opts)
  const handle = nx.crud(name)
  const items = ref([])
  const loading = ref(false)
  const error = ref(null)
  const idField = opts.idField || 'id'

  async function refresh(args) {
    loading.value = true
    error.value = null
    try {
      const r = await handle.list(args ?? opts.initialArgs)
      items.value = Array.isArray(r) ? r : (r?.items ?? [])
    } catch (e) {
      error.value = e
    } finally {
      loading.value = false
    }
  }

  async function create(body) {
    const r = await handle.create(body)
    if (r && typeof r === 'object') items.value.push(r)
    return r
  }

  async function update(id, body) {
    const r = await handle.update(id, body)
    const i = items.value.findIndex(x => x?.[idField] === id)
    if (i >= 0 && r && typeof r === 'object') items.value[i] = r
    return r
  }

  async function remove(id) {
    await handle.delete(id)
    items.value = items.value.filter(x => x?.[idField] !== id)
  }

  // WS subscription. Optional; opts.subscribe === false disables.
  let ws = null
  if (opts.subscribe !== false) {
    const path = opts.wsPath || `/${name}`
    ws = nx.ws(path)
      .on(`${name}.created`, (item) => {
        if (item && typeof item === 'object') items.value.push(item)
      })
      .on(`${name}.updated`, (item) => {
        const i = items.value.findIndex(x => x?.[idField] === item?.[idField])
        if (i >= 0) items.value[i] = item
      })
      .on(`${name}.deleted`, (payload) => {
        const id = (payload && payload[idField]) ?? payload
        items.value = items.value.filter(x => x?.[idField] !== id)
      })
    ws.connect().catch(() => { /* WS optional — fall back to manual refresh */ })
  }

  // Initial fetch unless caller opts out (e.g. when args aren't ready yet).
  if (opts.lazy !== true) refresh()
  onUnmounted(() => ws && ws.close())

  return { items, loading, error, refresh, create, update, remove, ws }
}

// -- useWS ---------------------------------------------------------

/**
 * useWS returns a reactive WebSocket handle. Connection state is
 * exposed as a ref; .on/.send/.close mirror the underlying
 * WSHandle. Auto-closes on component unmount.
 *
 * @returns {{ connected, on, send, connect, close, handle }}
 */
export function useWS(path, opts = {}) {
  const nx = resolve(opts)
  const handle = nx.ws(path)
  const connected = ref(false)

  handle
    .on('@close', () => { connected.value = false })
    .on('@error', () => { connected.value = false })

  async function connect() {
    if (connected.value) return handle
    await handle.connect()
    connected.value = true
    return handle
  }

  function close() { handle.close(); connected.value = false }

  if (opts.eager !== false) {
    connect().catch(() => {})
  }
  onUnmounted(close)

  return {
    connected,
    handle,
    on:   (type, fn) => handle.on(type, fn),
    send: (type, data) => handle.send(type, data),
    connect,
    close,
  }
}

// -- useAuth -------------------------------------------------------

/**
 * useAuth returns reactive auth state plus the login / logout / me
 * actions. On mount, if a token is already in the store, fires me()
 * to bootstrap the identity (cookie-based apps work the same way
 * because the cookie ride-along makes me() return the user).
 *
 * @returns {{
 *   token, identity, loading, error,
 *   isAuthenticated,
 *   login, logout, refresh,
 * }}
 */
export function useAuth(opts = {}) {
  const nx = resolve(opts)
  const token = ref(nx.auth.token)
  const identity = ref(null)
  const loading = ref(false)
  const error = ref(null)

  async function refresh() {
    loading.value = true
    error.value = null
    try {
      identity.value = await nx.auth.me()
    } catch (e) {
      identity.value = null
      // 401 is expected when no session exists; surface other errors
      // so apps can distinguish "not logged in" from "/me broke".
      if (e?.status !== 401) error.value = e
    } finally {
      loading.value = false
    }
  }

  async function login(creds) {
    loading.value = true
    error.value = null
    try {
      const r = await nx.auth.login(creds)
      token.value = nx.auth.token
      // login response typically includes the user shape; if not,
      // fall back to /me for the canonical identity.
      if (r && typeof r === 'object' && (r.id || r.user || r.identity)) {
        identity.value = r.user ?? r.identity ?? r
      } else {
        await refresh()
      }
      return r
    } catch (e) {
      error.value = e
      throw e
    } finally {
      loading.value = false
    }
  }

  async function logout() {
    loading.value = true
    try {
      await nx.auth.logout()
    } finally {
      token.value = null
      identity.value = null
      loading.value = false
    }
  }

  // Bootstrap: try /me on mount when a token (or cookie) is present.
  // Cookie-based apps will hit /me with credentials:'include' and
  // the cookie ride-along recovers the session without a token in
  // local storage — `eager: false` opts out of the bootstrap call
  // for apps that show a login screen unconditionally.
  if (opts.eager !== false) {
    refresh().catch(() => {})
  }

  const isAuthenticated = computed(() => !!identity.value || !!token.value)

  return { token, identity, loading, error, isAuthenticated, login, logout, refresh }
}

// Default export — the most common composable, so apps can do
//   import useNexus from '/__nexus/client/vue.js'
// alongside the named-import style.
export default useNexus
