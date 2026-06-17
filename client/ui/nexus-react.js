// Nexus client SDK — React hooks. Served at <client-path>/react.js.
// Plain ESM, no build step.
//
// Imports `useCallback`, `useEffect`, `useMemo`, `useRef`, `useState`
// from 'react'. The host app must resolve 'react' (a bundler entry,
// an importmap, or a CDN ESM URL) — keeping this file dependency-free
// would force a hard pin on a React version inside the SDK, which is
// the wrong tradeoff for a framework consumed by N apps.
//
// Each hook is a thin reactive wrapper over the underlying
// NexusClient method. They share ONE module-level NexusClient
// instance so manifest fetches + auth state are deduplicated across
// every component on the page; pass an explicit client (or
// constructor opts) to useNexus() the first time if you need a
// custom origin / fetch / token store.
//
// Example:
//
//   import { useQuery, useCrud, useAuth } from '/__nexus/client/react.js'
//
//   function PetsList() {
//     const auth = useAuth()
//     const pets = useCrud('pets', { wsPath: '/events' })
//     const [search, setSearch] = useState('')
//     const { data, loading } = useQuery('GET', '/pets', { q: search })
//     return loading ? <Spinner /> : <List items={data} />
//   }

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { NexusClient, localStorageTokenStore, memoryTokenStore } from './client.js'

// -- Shared client -------------------------------------------------

let _shared = null

// envDefaults reads Vite-style env vars (harmless under any other
// bundler — import.meta.env is just absent and falls back to {}).
// VITE_NEXUS_API   → origin override (trailing slashes stripped).
//                    IGNORED in vite dev mode (import.meta.env.DEV)
//                    when it points cross-origin, because nexus dev
//                    relies on vite's server.proxy to forward the
//                    framework's reserved paths (/__nexus, /graphql,
//                    /oauth, /ws) to the Go server same-origin. An
//                    absolute origin would bypass the proxy and
//                    trip CORS. The setting is still honored for
//                    production builds where the SPA and API
//                    legitimately live on different hosts.
// VITE_NEXUS_TOKEN → localStorage key for the bearer token. UNSET by
//                    default → in-memory token (XSS-safe, cleared on
//                    reload). Set it (e.g. 'access_token') to opt into
//                    localStorage persistence and its XSS exposure.
function envDefaults() {
  const env = (typeof import.meta !== 'undefined' && import.meta.env) || {}
  let origin = String(env.VITE_NEXUS_API ?? '').replace(/\/+$/, '')
  if (env.DEV && origin && typeof location !== 'undefined') {
    try {
      const apiOrigin = new URL(origin).origin
      if (apiOrigin !== location.origin) origin = ''
    } catch {
      origin = ''
    }
  }
  // Token persistence is opt-in, matching NexusClient's secure default:
  // in-memory unless the app explicitly names a localStorage key via
  // VITE_NEXUS_TOKEN. A persisted bearer token is readable by any XSS,
  // so apps choose that tradeoff deliberately rather than inherit it.
  const tokenKey = env.VITE_NEXUS_TOKEN
  return {
    origin: origin || undefined,
    tokenStore:
      typeof window === 'undefined' || !tokenKey
        ? memoryTokenStore()
        : localStorageTokenStore(String(tokenKey)),
  }
}

/**
 * useNexus returns the page-shared NexusClient. The first caller's
 * opts seed the singleton; subsequent calls ignore opts and return
 * the existing client. Apps that need multiple clients (e.g. two
 * backends) construct a `new NexusClient(...)` directly and pass it
 * to the other hooks via the `client` option.
 *
 * Default construction picks up VITE_NEXUS_API (origin) and
 * VITE_NEXUS_TOKEN (localStorage key) so a bare `useNexus()` works
 * in a typical Vite app with zero wiring. Explicit opts always
 * override the env-derived defaults field-by-field.
 *
 * Stable across renders — the same reference is returned every call,
 * so the hook is safe to use in useEffect deps and Context values.
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

// resolve picks the active client for a hook call: explicit override
// on opts wins over the shared singleton.
function resolve(opts) {
  return (opts && opts.client) || useNexus()
}

// argsKey stably stringifies args for useEffect deps. JSON.stringify
// is adequate here — args land in URL query / GraphQL variables /
// body, all already JSON-serializable. Function-shaped args are
// supported (callers may pass `() => ({ q: search })` for lazy
// evaluation) by calling them first.
function readArgs(args) {
  if (typeof args === 'function') return args()
  return args ?? {}
}

function argsKey(args) {
  try { return JSON.stringify(readArgs(args)) }
  catch { return String(Date.now()) } // unstringifiable args refresh on every render — caller's bug
}

// -- useQuery (REST) -----------------------------------------------

/**
 * useQuery wraps a REST call as reactive state. Re-fires the call
 * whenever args (shallow-stringified) changes. Returns:
 *
 *   { data, error, loading, refresh, abort }
 *
 * data starts as null. error / loading reset on each call. refresh
 * forces a refetch with current args; abort cancels an in-flight
 * call (the next refresh starts a new one).
 *
 * @param {string} method            HTTP verb
 * @param {string} path              route path; :params resolved from args
 * @param {object|()=>object} [args]
 * @param {object} [opts]            { client, headers, watch: false }
 */
export function useQuery(method, path, args, opts = {}) {
  const nx = resolve(opts)
  const [data, setData] = useState(null)
  const [error, setError] = useState(null)
  const [loading, setLoading] = useState(false)
  const inflight = useRef(null)
  const argsRef = useRef(args)
  argsRef.current = args  // always current, no stale-closure risk

  const refresh = useCallback(async () => {
    abort()
    const ac = new AbortController()
    inflight.current = ac
    setLoading(true)
    setError(null)
    try {
      const a = readArgs(argsRef.current)
      const r = await nx.rest(method, path, a, { headers: opts.headers, signal: ac.signal })
      if (inflight.current === ac) setData(r)
    } catch (e) {
      if (e?.name !== 'AbortError' && inflight.current === ac) setError(e)
    } finally {
      if (inflight.current === ac) {
        inflight.current = null
        setLoading(false)
      }
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nx, method, path, opts.headers])

  function abort() {
    if (inflight.current) {
      try { inflight.current.abort() } catch {}
      inflight.current = null
    }
  }

  // Auto-refetch on args change. argsKey() stringifies args so
  // shape-equal new references don't trigger redundant fetches —
  // matches Vue's deep-watch behavior. opts.watch === false opts
  // out and the caller drives via refresh() manually.
  const key = argsKey(args)
  useEffect(() => {
    if (opts.watch === false) return
    refresh()
    return abort
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, refresh, opts.watch])

  return { data, error, loading, refresh, abort }
}

// -- useMutation (REST) --------------------------------------------

/**
 * useMutation wraps a write-style REST call. Manual-fire — does not
 * run on mount, fires only when mutate() is called. Returns:
 *
 *   { mutate, data, error, loading }
 *
 * mutate(args?, callOpts?) returns the response body and updates
 * data. Throws on error so try/catch in the caller works naturally;
 * error state is also populated for render-side conditionals.
 */
export function useMutation(method, path, opts = {}) {
  const nx = resolve(opts)
  const [data, setData] = useState(null)
  const [error, setError] = useState(null)
  const [loading, setLoading] = useState(false)

  const mutate = useCallback(async (args, callOpts) => {
    setLoading(true)
    setError(null)
    try {
      const r = await nx.rest(method, path, args, { ...opts, ...callOpts })
      setData(r)
      return r
    } catch (e) {
      setError(e)
      throw e
    } finally {
      setLoading(false)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nx, method, path])

  return { mutate, data, error, loading }
}

// -- useGqlQuery / useGqlMutation ----------------------------------

/**
 * useGqlQuery wraps a GraphQL query op (registered via
 * nexus.AsQuery). Same shape as useQuery — args-keyed refetch,
 * exposes { data, error, loading, refresh }.
 */
export function useGqlQuery(name, args, opts = {}) {
  const nx = resolve(opts)
  const [data, setData] = useState(null)
  const [error, setError] = useState(null)
  const [loading, setLoading] = useState(false)
  const argsRef = useRef(args)
  argsRef.current = args

  const refresh = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const r = await nx.query(name, readArgs(argsRef.current))
      setData(r)
    } catch (e) {
      setError(e)
    } finally {
      setLoading(false)
    }
  }, [nx, name])

  const key = argsKey(args)
  useEffect(() => {
    if (opts.watch === false) return
    refresh()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, refresh, opts.watch])

  return { data, error, loading, refresh }
}

/**
 * useGqlMutation wraps a GraphQL mutation. Manual-fire; same shape
 * as useMutation.
 */
export function useGqlMutation(name, opts = {}) {
  const nx = resolve(opts)
  const [data, setData] = useState(null)
  const [error, setError] = useState(null)
  const [loading, setLoading] = useState(false)

  const mutate = useCallback(async (vars) => {
    setLoading(true)
    setError(null)
    try {
      const r = await nx.mutate(name, vars)
      setData(r)
      return r
    } catch (e) {
      setError(e)
      throw e
    } finally {
      setLoading(false)
    }
  }, [nx, name])

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
 *     opts.subscribe !== false), the hook opens a WebSocket and
 *     listens for three message types matching the entity:
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
  const handle = useMemo(() => nx.crud(name), [nx, name])
  const [items, setItems] = useState([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(null)
  const idField = opts.idField || 'id'
  const wsRef = useRef(null)

  const refresh = useCallback(async (args) => {
    setLoading(true)
    setError(null)
    try {
      const r = await handle.list(args ?? opts.initialArgs)
      setItems(Array.isArray(r) ? r : (r?.items ?? []))
    } catch (e) {
      setError(e)
    } finally {
      setLoading(false)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [handle])

  const create = useCallback(async (body) => {
    const r = await handle.create(body)
    if (r && typeof r === 'object') setItems(prev => [...prev, r])
    return r
  }, [handle])

  const update = useCallback(async (id, body) => {
    const r = await handle.update(id, body)
    if (r && typeof r === 'object') {
      setItems(prev => prev.map(x => x?.[idField] === id ? r : x))
    }
    return r
  }, [handle, idField])

  const remove = useCallback(async (id) => {
    await handle.delete(id)
    setItems(prev => prev.filter(x => x?.[idField] !== id))
  }, [handle, idField])

  useEffect(() => {
    if (opts.subscribe === false) return
    const path = opts.wsPath || `/${name}`
    const ws = nx.ws(path)
      .on(`${name}.created`, (item) => {
        if (item && typeof item === 'object') setItems(prev => [...prev, item])
      })
      .on(`${name}.updated`, (item) => {
        setItems(prev => prev.map(x => x?.[idField] === item?.[idField] ? item : x))
      })
      .on(`${name}.deleted`, (payload) => {
        const id = (payload && payload[idField]) ?? payload
        setItems(prev => prev.filter(x => x?.[idField] !== id))
      })
    wsRef.current = ws
    ws.connect().catch(() => { /* WS optional — fall back to manual refresh */ })
    return () => { ws.close(); wsRef.current = null }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nx, name, idField, opts.subscribe, opts.wsPath])

  useEffect(() => {
    if (opts.lazy === true) return
    refresh()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refresh, opts.lazy])

  return { items, loading, error, refresh, create, update, remove, ws: wsRef.current }
}

// -- useWS ---------------------------------------------------------

/**
 * useWS returns a reactive WebSocket handle. Connection state is
 * exposed as a boolean; .on/.send/.close mirror the underlying
 * WSHandle. Auto-closes on component unmount.
 *
 * @returns {{ connected, on, send, connect, close, handle }}
 */
export function useWS(path, opts = {}) {
  const nx = resolve(opts)
  const handle = useMemo(() => nx.ws(path), [nx, path])
  const [connected, setConnected] = useState(false)

  useEffect(() => {
    handle
      .on('@close', () => setConnected(false))
      .on('@error', () => setConnected(false))
    if (opts.eager !== false) {
      handle.connect().then(() => setConnected(true)).catch(() => {})
    }
    return () => { handle.close(); setConnected(false) }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [handle, opts.eager])

  const connect = useCallback(async () => {
    if (connected) return handle
    await handle.connect()
    setConnected(true)
    return handle
  }, [handle, connected])

  const close = useCallback(() => {
    handle.close()
    setConnected(false)
  }, [handle])

  return {
    connected,
    handle,
    on: (type, fn) => handle.on(type, fn),
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
  const [token, setToken] = useState(nx.auth.token)
  const [identity, setIdentity] = useState(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState(null)

  const refresh = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const id = await nx.auth.me()
      setIdentity(id)
    } catch (e) {
      setIdentity(null)
      // 401 is expected when no session exists; surface other errors
      // so apps can distinguish "not logged in" from "/me broke".
      if (e?.status !== 401) setError(e)
    } finally {
      setLoading(false)
    }
  }, [nx])

  const login = useCallback(async (creds) => {
    setLoading(true)
    setError(null)
    try {
      const r = await nx.auth.login(creds)
      setToken(nx.auth.token)
      // login response typically includes the user shape; if not,
      // fall back to /me for the canonical identity.
      if (r && typeof r === 'object' && (r.id || r.user || r.identity)) {
        setIdentity(r.user ?? r.identity ?? r)
      } else {
        await refresh()
      }
      return r
    } catch (e) {
      setError(e)
      throw e
    } finally {
      setLoading(false)
    }
  }, [nx, refresh])

  const logout = useCallback(async () => {
    setLoading(true)
    try {
      await nx.auth.logout()
    } finally {
      setToken(null)
      setIdentity(null)
      setLoading(false)
    }
  }, [nx])

  // Bootstrap: try /me on mount when a token (or cookie) is present.
  // Cookie-based apps will hit /me with credentials:'include' and
  // the cookie ride-along recovers the session without a token in
  // local storage — `eager: false` opts out of the bootstrap call
  // for apps that show a login screen unconditionally.
  useEffect(() => {
    if (opts.eager === false) return
    refresh().catch(() => {})
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refresh, opts.eager])

  const isAuthenticated = useMemo(() => !!identity || !!token, [identity, token])

  return { token, identity, loading, error, isAuthenticated, login, logout, refresh }
}

// Default export — the most common hook, so apps can do
//   import useNexus from '/__nexus/client/react.js'
// alongside the named-import style.
export default useNexus
