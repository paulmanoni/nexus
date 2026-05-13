package frontend

import (
	"fmt"
	"strings"

	"github.com/paulmanoni/nexus/extension"
	"github.com/paulmanoni/nexus/registry"
)

// renderReactTS emits react.ts — per-op React hooks paired with each
// GraphQL endpoint, mirroring the Vue composable surface but built on
// React 18 primitives (useState + useCallback + useEffect). Each
// hook hands back the same { data, loading, error, ... } shape so a
// component switching frameworks only swaps imports.
//
// Query hooks auto-fire on mount and on vars change. The dep is
// JSON.stringify(vars) — coarse but standard for "key a hook by an
// arbitrary object". Consumers that want a deeper / cheaper compare
// import refresh directly and skip the auto-trigger.
//
// Mutation hooks expose a mutate(vars) callback; data/loading/error
// reflect the last invocation. The callback is useCallback'd with an
// empty dep list so a parent re-render doesn't re-create it.
//
// Imports 'react' as a peer — the host app's bundler resolves it
// against whichever React 18+ version is installed.
func renderReactTS(cfg Config, ctx extension.GenerateContext) string {
	var b strings.Builder
	writeBanner(&b, "React 18+ hooks")
	b.WriteString(`import { useState, useEffect, useCallback } from 'react'
import { client, NexusError } from './_client'
import type * as T from './types'

interface QueryState<R> {
  data: R | undefined
  loading: boolean
  error: NexusError | undefined
  refresh: () => Promise<void>
}

interface MutationState<V, R> {
  data: R | undefined
  loading: boolean
  error: NexusError | undefined
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
			fmt.Fprintf(&b, "  const [data, setData] = useState<%s | undefined>(undefined)\n", retType)
			b.WriteString("  const [loading, setLoading] = useState(false)\n")
			b.WriteString("  const [error, setError] = useState<NexusError | undefined>(undefined)\n")
			fmt.Fprintf(&b, "  const mutate = useCallback(async (vars: %s): Promise<%s> => {\n", argsType, retType)
			b.WriteString("    setLoading(true)\n")
			b.WriteString("    setError(undefined)\n")
			b.WriteString("    try {\n")
			fmt.Fprintf(&b, "      const out = await client.gql<%s, %s>(%s, vars)\n", argsType, retType, tsLiteral(e.Name))
			fmt.Fprintf(&b, "      setData(out as %s)\n", retType)
			b.WriteString("      return out\n")
			b.WriteString("    } catch (e) {\n")
			b.WriteString("      setError(e as NexusError)\n")
			b.WriteString("      throw e\n")
			b.WriteString("    } finally {\n")
			b.WriteString("      setLoading(false)\n")
			b.WriteString("    }\n")
			b.WriteString("  }, [])\n")
			b.WriteString("  return { data, loading, error, mutate }\n")
			b.WriteString("}\n\n")
		} else {
			fmt.Fprintf(&b, "export function %s(vars?: %s): QueryState<%s> {\n", hookName, argsType, retType)
			fmt.Fprintf(&b, "  const [data, setData] = useState<%s | undefined>(undefined)\n", retType)
			b.WriteString("  const [loading, setLoading] = useState(false)\n")
			b.WriteString("  const [error, setError] = useState<NexusError | undefined>(undefined)\n")
			b.WriteString("  const refresh = useCallback(async () => {\n")
			b.WriteString("    setLoading(true)\n")
			b.WriteString("    setError(undefined)\n")
			b.WriteString("    try {\n")
			fmt.Fprintf(&b, "      const v = (vars ?? ({} as %s))\n", argsType)
			fmt.Fprintf(&b, "      const out = await client.gql<%s, %s>(%s, v)\n", argsType, retType, tsLiteral(e.Name))
			fmt.Fprintf(&b, "      setData(out as %s)\n", retType)
			b.WriteString("    } catch (e) {\n")
			b.WriteString("      setError(e as NexusError)\n")
			b.WriteString("    } finally {\n")
			b.WriteString("      setLoading(false)\n")
			b.WriteString("    }\n")
			// JSON.stringify is the standard escape hatch for keying a
			// hook on an arbitrary object — cheap for the small vars
			// records a typed GraphQL call carries, and avoids the
			// deep-equal hook dep most React projects don't have.
			b.WriteString("  }, [JSON.stringify(vars ?? null)])\n")
			b.WriteString("  useEffect(() => { void refresh() }, [refresh])\n")
			b.WriteString("  return { data, loading, error, refresh }\n")
			b.WriteString("}\n\n")
		}
	}

	return b.String()
}

// renderSvelteTS emits svelte.ts — per-op store factories returning
// the standard { data, loading, error, refresh } shape, but with each
// field exposed as a Svelte Writable store. Works on Svelte 4 (the
// canonical store API) and Svelte 5 (stores still resolve alongside
// runes, so existing code keeps working under the runes-first model).
//
// Query factories fire refresh() once immediately so the consumer's
// {#await} block or auto-subscription ($data) sees a fetch in flight
// on first read. Re-running with new vars is the caller's job —
// Svelte's reactivity is component-scoped, so the codegen can't
// auto-subscribe to vars changes without baking a component into the
// store factory (which would lock the API to Svelte 4's lifecycle).
//
// Imports 'svelte/store' as a peer.
func renderSvelteTS(cfg Config, ctx extension.GenerateContext) string {
	var b strings.Builder
	writeBanner(&b, "Svelte stores (compatible with Svelte 4 + 5)")
	b.WriteString(`import { writable, type Writable } from 'svelte/store'
import { client, NexusError } from './_client'
import type * as T from './types'

interface QueryState<R> {
  data: Writable<R | undefined>
  loading: Writable<boolean>
  error: Writable<NexusError | undefined>
  refresh: () => Promise<void>
}

interface MutationState<V, R> {
  data: Writable<R | undefined>
  loading: Writable<boolean>
  error: Writable<NexusError | undefined>
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
		// Svelte's idiomatic naming is camelCase factory functions
		// (createUserQuery, etc.), but staying with `useFoo` keeps the
		// surface identical to Vue + React — the consumer switching
		// frameworks doesn't have to relearn the symbol names.
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
			fmt.Fprintf(&b, "  const data = writable<%s | undefined>(undefined)\n", retType)
			b.WriteString("  const loading = writable(false)\n")
			b.WriteString("  const error = writable<NexusError | undefined>(undefined)\n")
			fmt.Fprintf(&b, "  async function mutate(vars: %s): Promise<%s> {\n", argsType, retType)
			b.WriteString("    loading.set(true)\n")
			b.WriteString("    error.set(undefined)\n")
			b.WriteString("    try {\n")
			fmt.Fprintf(&b, "      const out = await client.gql<%s, %s>(%s, vars)\n", argsType, retType, tsLiteral(e.Name))
			fmt.Fprintf(&b, "      data.set(out as %s)\n", retType)
			b.WriteString("      return out\n")
			b.WriteString("    } catch (e) {\n")
			b.WriteString("      error.set(e as NexusError)\n")
			b.WriteString("      throw e\n")
			b.WriteString("    } finally {\n")
			b.WriteString("      loading.set(false)\n")
			b.WriteString("    }\n")
			b.WriteString("  }\n")
			b.WriteString("  return { data, loading, error, mutate }\n")
			b.WriteString("}\n\n")
		} else {
			fmt.Fprintf(&b, "export function %s(vars?: %s): QueryState<%s> {\n", hookName, argsType, retType)
			fmt.Fprintf(&b, "  const data = writable<%s | undefined>(undefined)\n", retType)
			b.WriteString("  const loading = writable(false)\n")
			b.WriteString("  const error = writable<NexusError | undefined>(undefined)\n")
			b.WriteString("  async function refresh() {\n")
			b.WriteString("    loading.set(true)\n")
			b.WriteString("    error.set(undefined)\n")
			b.WriteString("    try {\n")
			fmt.Fprintf(&b, "      const v = (vars ?? ({} as %s))\n", argsType)
			fmt.Fprintf(&b, "      const out = await client.gql<%s, %s>(%s, v)\n", argsType, retType, tsLiteral(e.Name))
			fmt.Fprintf(&b, "      data.set(out as %s)\n", retType)
			b.WriteString("    } catch (e) {\n")
			b.WriteString("      error.set(e as NexusError)\n")
			b.WriteString("    } finally {\n")
			b.WriteString("      loading.set(false)\n")
			b.WriteString("    }\n")
			b.WriteString("  }\n")
			// Fire-and-forget kick so the consumer's {#await} block
			// sees a real fetch in flight on first subscribe. Errors
			// land on the error store and surface via the same
			// reactive read.
			b.WriteString("  void refresh()\n")
			b.WriteString("  return { data, loading, error, refresh }\n")
			b.WriteString("}\n\n")
		}
	}

	return b.String()
}
