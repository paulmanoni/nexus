package frontend

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/paulmanoni/nexus/extension"
	"github.com/paulmanoni/nexus/registry"
)

// renderClientTS emits _client.ts — the transport-neutral RPC
// dispatcher. Phase-1 shape: a fetch wrapper that knows how to call
// GraphQL, REST, and WebSocket endpoints, plus a token store. No
// manifest fetch, no schema knowledge — the op-specific calls live
// in index.ts.
//
// Output is byte-stable across runs (no map iteration, no time
// values) so `nexus generate --check` can compare against disk
// without spurious diffs.
func renderClientTS(cfg Config, ctx extension.GenerateContext) string {
	basePath := ctx.BasePath
	var b strings.Builder
	writeBanner(&b, "transport dispatcher")
	b.WriteString(`import type * as T from './types'

export class NexusError extends Error {
  status?: number
  code?: string
  payload?: unknown
  endpoint?: string
  constructor(message: string, init: Partial<NexusError> = {}) {
    super(message)
    this.name = 'NexusError'
    Object.assign(this, init)
  }
}

export interface TokenStore {
  get(): string | null
  set(token: string | null): void
  clear(): void
}

export function memoryTokenStore(): TokenStore {
  let v: string | null = null
  return { get: () => v, set: (t) => { v = t }, clear: () => { v = null } }
}

export function localStorageTokenStore(key = 'nexus.token'): TokenStore {
  try {
    if (typeof localStorage === 'undefined') throw new Error('no localStorage')
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

export interface NexusOpts {
  signal?: AbortSignal
  headers?: Record<string, string>
  /** GraphQL only — overrides the default field selection. */
  select?: unknown
}

export interface NexusClientOptions {
  origin?: string
  fetch?: typeof globalThis.fetch
  tokenStore?: TokenStore
}

export class NexusClient {
  readonly origin: string
  readonly basePath: string
  private fetch: typeof globalThis.fetch
  readonly tokens: TokenStore

  constructor(opts: NexusClientOptions = {}) {
    this.origin = (opts.origin ?? '').replace(/\/+$/, '')
    this.basePath = ` + tsLiteral(basePath) + `
    this.fetch = opts.fetch ?? globalThis.fetch.bind(globalThis)
    this.tokens = opts.tokenStore ?? localStorageTokenStore()
  }

  private url(p: string): string {
    if (/^https?:\/\//i.test(p)) return p
    const base = this.origin + this.basePath
    return base.replace(/\/$/, '') + (p.startsWith('/') ? p : '/' + p)
  }

  private authHeaders(extra?: Record<string, string>): Record<string, string> {
    const h: Record<string, string> = { 'Content-Type': 'application/json', ...(extra ?? {}) }
    const tok = this.tokens.get()
    if (tok) h['Authorization'] = 'Bearer ' + tok
    return h
  }

  /** GraphQL op — name + variables. The typed wrappers in index.ts
   *  call this; consumers normally import those instead of using
   *  gql() directly. */
  async gql<V, R>(name: string, vars: V, opts?: NexusOpts): Promise<R> {
    const body = { query: buildOperation(name, vars, opts?.select), variables: vars ?? {} }
    const res = await this.fetch(this.url('/graphql'), {
      method: 'POST',
      headers: this.authHeaders(opts?.headers),
      body: JSON.stringify(body),
      signal: opts?.signal,
    })
    const json = await res.json().catch(() => null)
    if (!res.ok || (json && json.errors && json.errors.length)) {
      throw new NexusError('graphql ' + name + ' failed', {
        status: res.status, payload: json, endpoint: name,
      })
    }
    return (json?.data?.[name] ?? null) as R
  }

  /** REST call — method + path + body/query. Path params (:name) are
   *  substituted from args before the rest is shaped into a query
   *  string (GET) or JSON body (non-GET). */
  async rest<R>(method: string, path: string, vars?: any, opts?: NexusOpts): Promise<R> {
    let resolved = path
    const remaining: Record<string, any> = {}
    if (vars && typeof vars === 'object') {
      for (const [k, v] of Object.entries(vars)) {
        const tok = ':' + k
        if (resolved.includes(tok)) {
          resolved = resolved.replace(tok, encodeURIComponent(String(v)))
        } else {
          remaining[k] = v
        }
      }
    }
    let url = this.url(resolved)
    const init: RequestInit = { method, headers: this.authHeaders(opts?.headers), signal: opts?.signal }
    if (method.toUpperCase() === 'GET') {
      const qs = new URLSearchParams()
      for (const [k, v] of Object.entries(remaining)) {
        if (v == null) continue
        qs.set(k, typeof v === 'object' ? JSON.stringify(v) : String(v))
      }
      const s = qs.toString()
      if (s) url += (url.includes('?') ? '&' : '?') + s
    } else if (Object.keys(remaining).length > 0) {
      init.body = JSON.stringify(remaining)
    }
    const res = await this.fetch(url, init)
    const ct = res.headers.get('content-type') ?? ''
    const json = ct.includes('application/json') ? await res.json().catch(() => null) : null
    if (!res.ok) {
      throw new NexusError('rest ' + method + ' ' + path + ' failed', {
        status: res.status, payload: json, endpoint: method + ' ' + path,
      })
    }
    return json as R
  }

  /** WebSocket — opens one socket per path, multiplexed by message
   *  type. Inbound envelope: { type, data }. Outbound: same plus a
   *  client-stamped timestamp. */
  ws(path: string): NexusSocket {
    return new NexusSocket(this.url(path).replace(/^http/, 'ws'), this.tokens)
  }
}

export class NexusSocket {
  private socket: WebSocket | null = null
  private handlers = new Map<string, Array<(data: any) => void>>()
  private url: string
  private tokens: TokenStore
  constructor(url: string, tokens: TokenStore) { this.url = url; this.tokens = tokens }

  open(): WebSocket {
    if (this.socket) return this.socket
    const tok = this.tokens.get()
    const wsURL = tok ? this.url + (this.url.includes('?') ? '&' : '?') + 'access_token=' + encodeURIComponent(tok) : this.url
    const sock = new WebSocket(wsURL)
    sock.addEventListener('message', (ev) => {
      let env: { type: string; data: unknown }
      try { env = JSON.parse(ev.data) } catch { return }
      const list = this.handlers.get(env.type)
      if (list) for (const fn of list) fn(env.data)
    })
    this.socket = sock
    return sock
  }

  on(type: string, fn: (data: any) => void): () => void {
    let list = this.handlers.get(type)
    if (!list) { list = []; this.handlers.set(type, list) }
    list.push(fn)
    this.open()
    return () => {
      const cur = this.handlers.get(type)
      if (!cur) return
      const i = cur.indexOf(fn)
      if (i >= 0) cur.splice(i, 1)
    }
  }

  send(type: string, data: unknown): void {
    const sock = this.open()
    const payload = JSON.stringify({ type, data, timestamp: Date.now() })
    if (sock.readyState === WebSocket.OPEN) sock.send(payload)
    else sock.addEventListener('open', () => sock.send(payload), { once: true })
  }

  close(): void {
    this.socket?.close()
    this.socket = null
    this.handlers.clear()
  }
}

// buildOperation renders a GraphQL operation string. With opts.select
// provided as an object or array, the selection set narrows to those
// fields; otherwise the operation is emitted scalar-only ('{ __typename }'
// for object returns is handled by the typed wrappers in index.ts).
function buildOperation(name: string, vars: any, select: unknown): string {
  const sel = renderSelection(select)
  const varDecl = vars && typeof vars === 'object' && Object.keys(vars).length
    ? '(' + Object.keys(vars).map((k) => '$' + k + ': Any').join(', ') + ')'
    : ''
  const argList = vars && typeof vars === 'object' && Object.keys(vars).length
    ? '(' + Object.keys(vars).map((k) => k + ': $' + k).join(', ') + ')'
    : ''
  return ` + "`query " + "${name}" + "${varDecl} { " + "${name}" + "${argList}" + "${sel}" + " }`" + `
}

function renderSelection(select: unknown): string {
  if (!select) return ''
  if (typeof select === 'string') return ' ' + select
  if (Array.isArray(select)) return ' { ' + select.join(' ') + ' }'
  if (typeof select === 'object') return ' ' + renderSelectionObject(select as Record<string, unknown>)
  return ''
}

function renderSelectionObject(obj: Record<string, unknown>): string {
  const parts: string[] = []
  for (const [k, v] of Object.entries(obj)) {
    if (v === true) parts.push(k)
    else if (v && typeof v === 'object') parts.push(k + ' ' + renderSelectionObject(v as Record<string, unknown>))
  }
  return '{ ' + parts.join(' ') + ' }'
}

// Module-level singleton consumed by the generated per-op wrappers.
// Re-create with a custom origin / token store by importing
// NexusClient directly and exporting your own instance.
export const client = new NexusClient({
  origin: typeof import.meta !== 'undefined' && (import.meta as any).env?.VITE_NEXUS_API
    ? String((import.meta as any).env.VITE_NEXUS_API).replace(/\/+$/, '')
    : '',
})
`)
	return b.String()
}

// renderTypesTS emits types.ts — one `export interface` per named
// struct in the registry's shared ref pool. Sorted alphabetically so
// byte-stable output is preserved across builds.
func renderTypesTS(cfg Config, ctx extension.GenerateContext) string {
	var b strings.Builder
	writeBanner(&b, "named type definitions")
	if len(ctx.Refs) == 0 {
		b.WriteString("// (no named types — every endpoint takes/returns primitives or anonymous structs)\n")
		return b.String()
	}
	names := make([]string, 0, len(ctx.Refs))
	for n := range ctx.Refs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		nt := ctx.Refs[n]
		if nt.Description != "" {
			fmt.Fprintf(&b, "/** %s */\n", escapeComment(nt.Description))
		}
		fmt.Fprintf(&b, "export interface %s {\n", tsIdent(n))
		writeFields(&b, nt.Fields, "  ")
		b.WriteString("}\n\n")
	}
	return b.String()
}

// renderIndexTS emits index.ts — one named export per endpoint.
// GraphQL ops are keyed by name (consumer writes `listUsers(...)`);
// REST endpoints get a name synthesized from method+path when the
// registry didn't record one; WS endpoints expose a socket factory.
//
// Per-op exports are tree-shakable: the bundler drops any op the
// user app doesn't import, so the production bundle never carries
// unused schema — replacing the runtime manifest filter plugin.
func renderIndexTS(cfg Config, ctx extension.GenerateContext) string {
	var b strings.Builder
	writeBanner(&b, "per-op typed exports")
	b.WriteString("import { client } from './_client'\n")
	b.WriteString("import type * as T from './types'\n")
	b.WriteString("import type { NexusOpts } from './_client'\n\n")
	b.WriteString("export { client, NexusClient, NexusError, memoryTokenStore, localStorageTokenStore } from './_client'\n")
	b.WriteString("export type { NexusOpts, TokenStore } from './_client'\n\n")

	endpoints := ctx.Registry.Endpoints()
	taken := map[string]bool{}

	// GraphQL ops first — they have the cleanest name story.
	for _, e := range sortedEndpoints(endpoints, registry.GraphQL) {
		fnName := tsFnIdent(e.Name)
		if fnName == "" || taken[fnName] {
			continue
		}
		taken[fnName] = true
		writeOpDoc(&b, e)
		argsType := tsTypeFn(e.ArgsSchema, "Record<string, never>")
		retType := tsTypeFn(e.ReturnSchema, "unknown")
		signature := "(vars: " + argsType + ", opts?: NexusOpts)"
		if argsType == "Record<string, never>" {
			signature = "(vars?: " + argsType + ", opts?: NexusOpts)"
		}
		fmt.Fprintf(&b, "export const %s = %s =>\n  client.gql<%s, %s>(%s, vars ?? ({} as %s), opts)\n\n",
			fnName, signature, argsType, retType, tsLiteral(e.Name), argsType)
	}

	// REST endpoints.
	for _, e := range sortedEndpoints(endpoints, registry.REST) {
		fnName := tsFnIdent(restOpName(e))
		if fnName == "" {
			continue
		}
		base := fnName
		for i := 2; taken[fnName]; i++ {
			fnName = fmt.Sprintf("%s%d", base, i)
		}
		taken[fnName] = true
		writeOpDoc(&b, e)
		argsType := tsTypeFn(e.ArgsSchema, "Record<string, never>")
		retType := tsTypeFn(e.ReturnSchema, "void")
		fmt.Fprintf(&b, "export const %s = (vars?: %s, opts?: NexusOpts) =>\n  client.rest<%s>(%s, %s, vars, opts)\n\n",
			fnName, argsType, retType, tsLiteral(strings.ToUpper(e.Method)), tsLiteral(e.Path))
	}

	// WebSocket endpoints (deduplicated by path — the registry stores
	// one row per message, but a single socket factory covers the
	// whole path).
	wsSeen := map[string]bool{}
	for _, e := range sortedEndpoints(endpoints, registry.WebSocket) {
		if wsSeen[e.Path] {
			continue
		}
		wsSeen[e.Path] = true
		fnName := tsFnIdent(wsOpName(e))
		if fnName == "" || taken[fnName] {
			continue
		}
		taken[fnName] = true
		fmt.Fprintf(&b, "/** WebSocket — %s */\n", e.Path)
		fmt.Fprintf(&b, "export const %s = () => client.ws(%s)\n\n", fnName, tsLiteral(e.Path))
	}

	return b.String()
}

// renderVueTS emits vue.ts — per-op composables. One useX() per
// GraphQL op: queries get a reactive { data, loading, error, refresh };
// mutations get { mutate, loading, error }. REST + WS are left to
// the consumer to wrap (per-op REST composables don't add much over
// `useFetch(getThing)` from common Vue libraries).
//
// Imports 'vue' as a peer — the host app's bundler resolves it
// against whichever Vue 3 version is installed.
func renderVueTS(cfg Config, ctx extension.GenerateContext) string {
	var b strings.Builder
	writeBanner(&b, "Vue 3 composables")
	b.WriteString(`import { ref, toValue, watch, type MaybeRefOrGetter, type Ref } from 'vue'
import { client, NexusError } from './_client'
import type * as T from './types'

interface QueryState<R> {
  data: Ref<R | undefined>
  loading: Ref<boolean>
  error: Ref<NexusError | undefined>
  refresh: () => Promise<void>
}

interface MutationState<V, R> {
  data: Ref<R | undefined>
  loading: Ref<boolean>
  error: Ref<NexusError | undefined>
  mutate: (vars: V) => Promise<R>
}

`)

	endpoints := ctx.Registry.Endpoints()
	taken := map[string]bool{}

	for _, e := range sortedEndpoints(endpoints, registry.GraphQL) {
		base := tsFnIdent(e.Name)
		if base == "" {
			continue
		}
		hookName := "use" + ucFirst(base)
		if taken[hookName] {
			continue
		}
		taken[hookName] = true

		argsType := tsTypeFn(e.ArgsSchema, "Record<string, never>")
		retType := tsTypeFn(e.ReturnSchema, "unknown")
		isMutation := strings.EqualFold(e.Method, "mutation")

		if isMutation {
			fmt.Fprintf(&b, "export function %s(): MutationState<%s, %s> {\n", hookName, argsType, retType)
			b.WriteString("  const data = ref<" + retType + ">()\n")
			b.WriteString("  const loading = ref(false)\n")
			b.WriteString("  const error = ref<NexusError>()\n")
			fmt.Fprintf(&b, "  async function mutate(vars: %s): Promise<%s> {\n", argsType, retType)
			b.WriteString("    loading.value = true\n")
			b.WriteString("    error.value = undefined\n")
			b.WriteString("    try {\n")
			fmt.Fprintf(&b, "      const out = await client.gql<%s, %s>(%s, vars)\n", argsType, retType, tsLiteral(e.Name))
			b.WriteString("      data.value = out as " + retType + "\n")
			b.WriteString("      return out\n")
			b.WriteString("    } catch (e) {\n")
			b.WriteString("      error.value = e as NexusError\n")
			b.WriteString("      throw e\n")
			b.WriteString("    } finally {\n")
			b.WriteString("      loading.value = false\n")
			b.WriteString("    }\n")
			b.WriteString("  }\n")
			b.WriteString("  return { data, loading, error, mutate }\n")
			b.WriteString("}\n\n")
		} else {
			fmt.Fprintf(&b, "export function %s(vars?: MaybeRefOrGetter<%s>): QueryState<%s> {\n", hookName, argsType, retType)
			b.WriteString("  const data = ref<" + retType + ">()\n")
			b.WriteString("  const loading = ref(false)\n")
			b.WriteString("  const error = ref<NexusError>()\n")
			b.WriteString("  async function refresh() {\n")
			b.WriteString("    loading.value = true\n")
			b.WriteString("    error.value = undefined\n")
			b.WriteString("    try {\n")
			fmt.Fprintf(&b, "      const v = (vars ? toValue(vars) : ({} as %s))\n", argsType)
			fmt.Fprintf(&b, "      const out = await client.gql<%s, %s>(%s, v)\n", argsType, retType, tsLiteral(e.Name))
			b.WriteString("      data.value = out as " + retType + "\n")
			b.WriteString("    } catch (e) {\n")
			b.WriteString("      error.value = e as NexusError\n")
			b.WriteString("    } finally {\n")
			b.WriteString("      loading.value = false\n")
			b.WriteString("    }\n")
			b.WriteString("  }\n")
			b.WriteString("  watch(() => (vars ? toValue(vars) : undefined), refresh, { immediate: true, deep: true })\n")
			b.WriteString("  return { data, loading, error, refresh }\n")
			b.WriteString("}\n\n")
		}
	}

	return b.String()
}

// -- helpers --------------------------------------------------------

func writeBanner(b *strings.Builder, role string) {
	b.WriteString("// Auto-generated by the nexus frontend extension — DO NOT EDIT.\n")
	fmt.Fprintf(b, "// Role: %s.\n", role)
	b.WriteString("// Regenerate with: nexus generate\n\n")
}

func sortedEndpoints(eps []registry.Endpoint, t registry.Transport) []registry.Endpoint {
	out := make([]registry.Endpoint, 0, len(eps))
	for _, e := range eps {
		if e.Transport == t {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].Method != out[j].Method {
			return out[i].Method < out[j].Method
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func writeOpDoc(b *strings.Builder, e registry.Endpoint) {
	if e.Description == "" && !e.Deprecated {
		return
	}
	b.WriteString("/**\n")
	if e.Description != "" {
		for _, line := range strings.Split(e.Description, "\n") {
			fmt.Fprintf(b, " * %s\n", escapeComment(line))
		}
	}
	if e.Deprecated {
		reason := e.DeprecationReason
		if reason == "" {
			reason = "deprecated"
		}
		fmt.Fprintf(b, " * @deprecated %s\n", escapeComment(reason))
	}
	b.WriteString(" */\n")
}

func restOpName(e registry.Endpoint) string {
	if e.Name != "" {
		return e.Name
	}
	// Synthesize from method + path: "GET /users/:id" → "getUsersById".
	parts := []string{strings.ToLower(e.Method)}
	for _, seg := range strings.Split(e.Path, "/") {
		if seg == "" {
			continue
		}
		if strings.HasPrefix(seg, ":") {
			parts = append(parts, "By"+ucFirst(seg[1:]))
		} else {
			parts = append(parts, ucFirst(seg))
		}
	}
	return camelJoin(parts)
}

func wsOpName(e registry.Endpoint) string {
	if e.Name != "" {
		return e.Name
	}
	parts := []string{}
	for _, seg := range strings.Split(e.Path, "/") {
		if seg == "" {
			continue
		}
		parts = append(parts, ucFirst(seg))
	}
	if len(parts) == 0 {
		return "events"
	}
	return camelJoin(parts)
}

func camelJoin(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	// First part lower, rest already upper-camel.
	out := strings.ToLower(parts[0])
	for _, p := range parts[1:] {
		out += p
	}
	return out
}

// tsTypeFn renders a TypeRef as a TS type prefixed with the `T.`
// namespace import — `User` becomes `T.User`, anonymous shapes stay
// inline. Keeps index.ts/vue.ts compact (one short namespace import)
// without giving up tree-shaking on the underlying types.
func tsTypeFn(t *registry.TypeRef, fallback string) string {
	if t == nil {
		return fallback
	}
	return prefixRefs(tsType(t))
}

// prefixRefs walks a rendered TS type string and prefixes every
// occurrence of a bare identifier (PascalCase) that matches a ref
// name with `T.`. Done after rendering rather than threading state
// through tsType so this file's helpers stay self-contained and
// copyable into other frameworks' renderers verbatim.
//
// Simple heuristic: any PascalCase token that follows '<', ',' '[',
// ':', '(', or whitespace at a position where a type may begin gets
// the prefix. Avoids matching string-literal payloads (single-quoted)
// because tsType never emits PascalCase string literals.
func prefixRefs(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		// Skip over a single-quoted string verbatim.
		if c == '\'' {
			j := i + 1
			for j < len(s) {
				if s[j] == '\\' && j+1 < len(s) {
					j += 2
					continue
				}
				if s[j] == '\'' {
					j++
					break
				}
				j++
			}
			b.WriteString(s[i:j])
			i = j
			continue
		}
		// At the start of an identifier? Decide by previous non-space char.
		if isIdentStart(c) {
			j := i
			for j < len(s) && isIdentCont(s[j]) {
				j++
			}
			tok := s[i:j]
			prev := byte(' ')
			for k := i - 1; k >= 0; k-- {
				if s[k] != ' ' {
					prev = s[k]
					break
				}
			}
			if tsTypeContext(prev) && isPascal(tok) && !tsBuiltin(tok) {
				b.WriteString("T.")
			}
			b.WriteString(tok)
			i = j
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func tsTypeContext(prev byte) bool {
	switch prev {
	case '<', ',', '[', ':', '(', '|', '&', ' ', '\t', '\n', '=', '>':
		return true
	}
	return false
}

func tsBuiltin(tok string) bool {
	switch tok {
	case "Record", "Array", "Promise", "Map", "Set", "Date", "ReadonlyArray",
		"Partial", "Required", "Readonly", "Pick", "Omit", "Exclude", "Extract",
		"NonNullable", "Parameters", "ReturnType", "InstanceType",
		"T", // already-prefixed (defensive — we never produce this, but the test exercises it)
		"AbortSignal", "URLSearchParams", "WebSocket", "RequestInit":
		return true
	}
	return false
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentCont(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func isPascal(s string) bool {
	if s == "" {
		return false
	}
	r := rune(s[0])
	return r >= 'A' && r <= 'Z'
}

func ucFirst(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func tsFnIdent(s string) string {
	// Strip non-ident chars; lowercase first rune.
	var b strings.Builder
	first := true
	for _, r := range s {
		if first {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || r == '$') {
				continue
			}
			b.WriteRune(unicode.ToLower(r))
			first = false
			continue
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '$' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ── TS type rendering (forked from client/generator.go, kept private
// to extension/frontend so phase 1 doesn't refactor the runtime SDK's
// generator). A follow-up PR can hoist these into a shared package
// once both call sites converge.

func tsType(t *registry.TypeRef) string {
	if t == nil {
		return "unknown"
	}
	core := tsTypeCore(t)
	if t.Optional {
		return core + " | undefined"
	}
	return core
}

func tsTypeCore(t *registry.TypeRef) string {
	switch t.Kind {
	case "primitive":
		switch t.Primitive {
		case "string":
			return "string"
		case "boolean":
			return "boolean"
		case "integer", "number":
			return "number"
		}
		return "unknown"
	case "array":
		return tsType(t.Of) + "[]"
	case "map":
		key := "string"
		if t.KeyOf != nil && t.KeyOf.Kind == "primitive" && t.KeyOf.Primitive == "integer" {
			key = "number"
		}
		return fmt.Sprintf("Record<%s, %s>", key, tsType(t.Of))
	case "ref":
		return tsIdent(t.Ref)
	case "object":
		if t.Object == nil || len(t.Object.Fields) == 0 {
			return "{}"
		}
		var b strings.Builder
		b.WriteString("{ ")
		for i, f := range t.Object.Fields {
			if i > 0 {
				b.WriteString("; ")
			}
			name := f.JSONName
			if name == "" {
				name = f.Name
			}
			opt := ""
			if f.Optional {
				opt = "?"
			}
			fmt.Fprintf(&b, "%s%s: %s", tsKey(name), opt, tsType(&f.Type))
		}
		b.WriteString(" }")
		return b.String()
	case "any", "":
		return "unknown"
	default:
		return "unknown"
	}
}

func tsIdent(name string) string {
	if name == "" {
		return "unknown"
	}
	return name
}

func tsKey(name string) string {
	if isPlainIdent(name) {
		return name
	}
	return tsLiteral(name)
}

func isPlainIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !(r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
				return false
			}
			continue
		}
		if !(r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func tsLiteral(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		if r == '\\' || r == '\'' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

func escapeComment(s string) string {
	return strings.ReplaceAll(s, "*/", "* /")
}

func writeFields(b *strings.Builder, fields []registry.FieldSchema, indent string) {
	for _, f := range fields {
		name := f.JSONName
		if name == "" {
			name = f.Name
		}
		opt := ""
		if f.Optional {
			opt = "?"
		}
		if f.Description != "" {
			fmt.Fprintf(b, "%s/** %s */\n", indent, escapeComment(f.Description))
		}
		fmt.Fprintf(b, "%s%s%s: %s\n", indent, tsKey(name), opt, tsType(&f.Type))
	}
}
