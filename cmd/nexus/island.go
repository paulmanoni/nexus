package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
)

// newIslandCmd builds the `nexus island` subcommand. Scaffolds
// the file-set for one nl-island in the user's project:
//
//	nexus island --vue MyChart       → islands.src/MyChart.vue
//	                                   islands.src/MyChart.island.ts
//	nexus island --react MyChart     → islands.src/MyChart.tsx
//	                                   islands.src/MyChart.island.tsx
//	                                   islands.src/_nl-react-runtime.ts (once)
//	nexus island --vanilla mychart   → islands/mychart.js
//
// The Vue / React paths assume a Vite build step (see
// cookbook/islands/vite.config.ts). The vanilla path drops a
// hand-editable .js straight into islands/ — no build needed,
// just have template.WithStatic("islands", "") on the engine.
//
// Refuses to overwrite existing files (errors loudly) so a
// re-run never clobbers your edits. Pass --force to override.
//
// On success prints the <nl-island/> snippet to paste in the
// .nlt template + the Go-side helper signature for the props
// blob.
func newIslandCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		useVue     bool
		useReact   bool
		useVanilla bool
		dir        string
		force      bool
	)
	_ = stderr // reserved for future warning output (currently we use RunE return values)
	cmd := &cobra.Command{
		Use:   "island [flags] <name>",
		Short: "Scaffold an nl-island (Vue / React / vanilla bridge)",
		Long: `Scaffold one nl-island source file-set in the current project.

Each variant writes the framework-appropriate bridge to islands.src/
(or islands/ for vanilla) and prints the snippet to paste into your
.nlt template. Authoring shape mirrors cookbook/islands/.

Examples:
  nexus island --vue Counter
  nexus island --react Counter
  nexus island --vanilla counter --dir islands
`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			flavor := pickIslandFlavor(useVue, useReact, useVanilla)
			if flavor == "" {
				return fmt.Errorf("pick one of --vue / --react / --vanilla")
			}
			if err := validateIslandName(name); err != nil {
				return err
			}
			if dir == "" {
				dir = defaultIslandDir(flavor)
			}
			files, err := scaffoldIsland(flavor, name, dir, force)
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Created:\n")
			for _, f := range files {
				fmt.Fprintf(stdout, "  %s\n", f)
			}
			// When project configs landed alongside island
			// sources, surface a one-liner so the user knows
			// to install the JS deps before running.
			wroteConfigs := false
			for _, f := range files {
				if f == "package.json" {
					wroteConfigs = true
					break
				}
			}
			if wroteConfigs {
				fmt.Fprintln(stdout)
				fmt.Fprintln(stdout, "Bootstrapped JS build (package.json, vite.config.ts, tsconfig.json, .gitignore).")
				fmt.Fprintln(stdout, "Project-level — shared across every nl-island in this project.")
				fmt.Fprintln(stdout, "If you ever mix Vue + React in the same project, the configs only")
				fmt.Fprintln(stdout, "match the first-scaffolded flavor; --force on the second variant")
				fmt.Fprintln(stdout, "rewrites them (or merge by hand).")
			}
			fmt.Fprintln(stdout)
			fmt.Fprintln(stdout, "Embed in your .nlt:")
			fmt.Fprintf(stdout, "  <nl-island name=\"%s\"\n", name)
			fmt.Fprintf(stdout, "             src=\"/islands/%s.js\"\n", name)
			fmt.Fprintf(stdout, "             :props=\"%sProps()\"/>\n\n", name)
			fmt.Fprintln(stdout, "And on the Go side (in the component holding the page):")
			fmt.Fprintf(stdout, "  func (c *Page) %sProps() map[string]any {\n", name)
			fmt.Fprintln(stdout, "    return map[string]any{ /* whatever the island reads */ }")
			fmt.Fprintln(stdout, "  }")
			if flavor != "vanilla" {
				fmt.Fprintln(stdout)
				fmt.Fprintln(stdout, "Build before running:")
				fmt.Fprintln(stdout, "  npm install        # one-time")
				fmt.Fprintln(stdout, "  npm run build      # outputs to islands/")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&useVue, "vue", false, "scaffold a Vue 3 SFC + bridge")
	cmd.Flags().BoolVar(&useReact, "react", false, "scaffold a React 18 component + bridge")
	cmd.Flags().BoolVar(&useVanilla, "vanilla", false, "scaffold a vanilla-JS island (no build step)")
	cmd.Flags().StringVar(&dir, "dir", "", "destination directory (defaults: islands.src for vue/react, islands for vanilla)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing files")
	return cmd
}

func pickIslandFlavor(vue, react, vanilla bool) string {
	count := 0
	flavor := ""
	if vue {
		count++
		flavor = "vue"
	}
	if react {
		count++
		flavor = "react"
	}
	if vanilla {
		count++
		flavor = "vanilla"
	}
	if count != 1 {
		return ""
	}
	return flavor
}

// validateIslandName rejects names with characters that would
// break a file path or a JS identifier. JS-identifier rules are
// stricter than POSIX file rules — leaning on the JS side is
// safe because the name doubles as the island registry key on
// the wire.
func validateIslandName(name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	first := name[0]
	if !(first >= 'a' && first <= 'z') && !(first >= 'A' && first <= 'Z') && first != '_' {
		return fmt.Errorf("island name must start with a letter or underscore: %q", name)
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
		if !ok {
			return fmt.Errorf("invalid character in island name at position %d: %q", i, c)
		}
	}
	return nil
}

func defaultIslandDir(flavor string) string {
	if flavor == "vanilla" {
		return "islands"
	}
	return "islands.src"
}

// scaffoldIsland writes all files for one island variant. Stops
// at the first conflict (unless --force was passed) so a re-run
// never half-clobbers a tree.
//
// Vue + React variants additionally write project-config files
// (package.json / vite.config.ts / tsconfig.json / .gitignore)
// at the current working directory so a single `nexus island
// --vue MyChart` is enough to bootstrap a fresh app's whole
// build chain. Those files are marked shared — re-runs and
// the second-flavor case (user did --vue then --react) see
// the existing files and skip without --force.
func scaffoldIsland(flavor, name, dir string, force bool) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	data := struct{ Name string }{Name: name}
	var spec []fileSpec
	switch flavor {
	case "vue":
		spec = []fileSpec{
			{path: filepath.Join(dir, name+".vue"), tmpl: vueComponentTmpl},
			{path: filepath.Join(dir, name+".island.ts"), tmpl: vueBridgeTmpl},
			{path: "package.json", tmpl: vuePkgJSONTmpl, shared: true},
			{path: "vite.config.ts", tmpl: vueViteConfigTmpl, shared: true},
			{path: "tsconfig.json", tmpl: tsConfigTmpl, shared: true},
			{path: ".gitignore", tmpl: gitignoreTmpl, shared: true},
		}
	case "react":
		spec = []fileSpec{
			{path: filepath.Join(dir, name+".tsx"), tmpl: reactComponentTmpl},
			{path: filepath.Join(dir, name+".island.tsx"), tmpl: reactBridgeTmpl},
			// Runtime helper is shared across every React island
			// in the project. Written once; subsequent runs see
			// it already exists and skip without --force.
			{path: filepath.Join(dir, "_nl-react-runtime.ts"), tmpl: reactRuntimeTmpl, shared: true},
			{path: "package.json", tmpl: reactPkgJSONTmpl, shared: true},
			{path: "vite.config.ts", tmpl: reactViteConfigTmpl, shared: true},
			{path: "tsconfig.json", tmpl: tsConfigTmpl, shared: true},
			{path: ".gitignore", tmpl: gitignoreTmpl, shared: true},
		}
	case "vanilla":
		// No build step — vanilla JS lives in islands/ and the
		// browser dynamic-imports it as-is. No package.json,
		// no vite, no tsconfig.
		spec = []fileSpec{
			{path: filepath.Join(dir, name+".js"), tmpl: vanillaTmpl},
		}
	default:
		return nil, fmt.Errorf("unknown flavor %q", flavor)
	}

	var written []string
	for _, f := range spec {
		exists := fileExists(f.path)
		if exists && !force {
			// Shared files (the React runtime helper +
			// project-config files) are idempotent — skip the
			// conflict and don't error.
			if f.shared {
				continue
			}
			return written, fmt.Errorf("refusing to overwrite %s (pass --force)", f.path)
		}
		tmpl, err := template.New(filepath.Base(f.path)).Funcs(islandTmplFuncs).Parse(f.tmpl)
		if err != nil {
			return written, fmt.Errorf("template %s: %w", f.path, err)
		}
		out, err := os.Create(f.path)
		if err != nil {
			return written, fmt.Errorf("create %s: %w", f.path, err)
		}
		if err := tmpl.Execute(out, data); err != nil {
			_ = out.Close()
			return written, fmt.Errorf("write %s: %w", f.path, err)
		}
		if err := out.Close(); err != nil {
			return written, fmt.Errorf("close %s: %w", f.path, err)
		}
		written = append(written, f.path)
	}
	return written, nil
}

type fileSpec struct {
	path   string
	tmpl   string
	shared bool // skip silently when exists; never errors
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// --- templates ------------------------------------------------
//
// Inlined as Go const strings rather than //go:embed-ed from a
// templates/ directory. Trade-off: 200 fewer lines of plumbing,
// slightly noisier diff when these change. The cookbook copies
// at cookbook/islands/ are the human-readable versions; these
// are the parameterized generator inputs and may diverge.
//
// {{.Name}} is the user-supplied island name. Same identifier
// flows into the file basename, the SFC name, the JS export,
// and the nl-island="…" attribute on the wire.

const vueComponentTmpl = `<!--
  {{.Name}}.vue — generated by ` + "`nexus island --vue {{.Name}}`" + `.

  Edit freely. The .island.ts bridge file alongside this one
  imports it as the default export; you don't need to wire
  anything beyond what's already here.
-->
<script setup lang="ts">
import { ref, inject, onBeforeUnmount } from 'vue'

const _props = defineProps<{
  // Add your props here. Whatever the server-side
  // {{.Name}}Props() returns shows up parsed via JSON.
}>()

type Channel = {
  on(event: string, fn: (payload: any) => void): () => void
}
const channel = inject<Channel>('nlChannel')

// Example state — replace with your component logic.
const count = ref(0)

// Subscribe to server pushes scoped to this island:
//   ctx.PushIsland("{{.Name}}", "reset", nil) on the Go side
//   fires this listener with payload = nil.
const offReset = channel?.on('reset', () => {
  count.value = 0
})

onBeforeUnmount(() => {
  offReset?.()
})
</script>

<template>
  <div class="{{.Name}}-island">
    <button @click="count += 1">Count: {{"{{ count }}"}}</button>
  </div>
</template>
`

const vueBridgeTmpl = `// {{.Name}}.island.ts — generated by ` + "`nexus island --vue {{.Name}}`" + `.
//
// Bridges the nl-island lifecycle to Vue 3's createApp/mount/
// unmount. Same shape for every Vue island in this project —
// safe to regenerate, just don't hand-edit if you might.

import { createApp, reactive, type App } from 'vue'
import Component from './{{.Name}}.vue'

type Channel = {
  on(event: string, fn: (payload: any) => void): () => void
}

type Inst = {
  app: App
  props: Record<string, unknown>
}

export function mount(el: Element, props: any, channel: Channel): Inst {
  const reactiveProps = reactive((props ?? {}) as Record<string, unknown>)
  const app = createApp(Component, reactiveProps as any)
  app.provide('nlChannel', channel)
  app.mount(el)
  return { app, props: reactiveProps }
}

export function updated(_el: Element, newProps: any, inst: Inst) {
  // Mutate in place so Vue's reactivity sees the diff;
  // replacing the reference would break the binding.
  Object.assign(inst.props, newProps ?? {})
  for (const k of Object.keys(inst.props)) {
    if (newProps == null || !(k in newProps)) {
      delete (inst.props as any)[k]
    }
  }
}

export function destroyed(_el: Element, inst: Inst) {
  inst.app.unmount()
}
`

const reactComponentTmpl = `// {{.Name}}.tsx — generated by ` + "`nexus island --react {{.Name}}`" + `.
//
// Edit freely. useChannel() is the only nexus-specific
// touchpoint; everything else is plain React.

import { useState, useEffect } from 'react'
import { useChannel } from './_nl-react-runtime'

type Props = {
  // Add your props here. Whatever the server-side
  // {{.Name}}Props() returns shows up parsed via JSON.
}

export default function {{.Name}}(_props: Props) {
  const [count, setCount] = useState(0)
  const channel = useChannel()

  useEffect(() => {
    // ctx.PushIsland("{{.Name}}", "reset", nil) on the Go
    // side zeros this without remounting.
    const off = channel.on('reset', () => setCount(0))
    return off
  }, [channel])

  return (
    <button onClick={() => setCount((c) => c + 1)}>Count: {count}</button>
  )
}
`

const reactBridgeTmpl = `// {{.Name}}.island.tsx — generated by ` + "`nexus island --react {{.Name}}`" + `.
//
// Bridges the nl-island lifecycle to React 18's createRoot.
// Same shape for every React island; safe to regenerate.

import { createElement, useSyncExternalStore } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import {{.Name}} from './{{.Name}}'
import { ChannelContext, type Channel } from './_nl-react-runtime'

type Inst = {
  root: Root
  setProps: (p: any) => void
}

export function mount(el: Element, props: any, channel: Channel): Inst {
  let current = props ?? {}
  const listeners = new Set<() => void>()
  const subscribe = (fn: () => void) => {
    listeners.add(fn)
    return () => listeners.delete(fn)
  }
  const setProps = (next: any) => {
    current = next ?? {}
    listeners.forEach((fn) => fn())
  }

  const App = () => {
    const p = useSyncExternalStore(subscribe, () => current)
    return createElement(
      ChannelContext.Provider,
      { value: channel },
      createElement({{.Name}}, p as any),
    )
  }

  const root = createRoot(el as HTMLElement)
  root.render(createElement(App))
  return { root, setProps }
}

export function updated(_el: Element, newProps: any, inst: Inst) {
  inst.setProps(newProps)
}

export function destroyed(_el: Element, inst: Inst) {
  inst.root.unmount()
}
`

const reactRuntimeTmpl = `// _nl-react-runtime.ts — shared across every React island in
// this project. Generated once by ` + "`nexus island --react …`" + `;
// re-running the scaffolder leaves an existing copy alone.
//
// Underscore-prefixed so Vite's *.island.tsx glob doesn't pick
// it up as an entry point.

import { createContext, useContext } from 'react'

export type Channel = {
  on(event: string, fn: (payload: any) => void): () => void
}

// Default no-op so a missing Provider (e.g. rendering an
// island standalone in Storybook for testing) doesn't crash.
export const ChannelContext = createContext<Channel>({
  on: () => () => undefined,
})

export const useChannel = (): Channel => useContext(ChannelContext)
`

const vanillaTmpl = `// {{.Name}}.js — generated by ` + "`nexus island --vanilla {{.Name}}`" + `.
//
// The whole module is the bridge — vanilla means no framework
// is mediating the DOM, so there's no separate component layer.
// Edit freely; the live-template engine only needs the three
// exports below.

export function mount(el, props, channel) {
  // Initial render. Use whatever DOM you like — el is yours.
  let count = (props && props.initial) || 0
  el.innerHTML = ` + "`" + `<button>Count: <span></span></button>` + "`" + `
  const span = el.querySelector('span')
  const render = () => { span.textContent = String(count) }
  render()

  el.querySelector('button').addEventListener('click', () => {
    count += 1
    render()
  })

  // Server pushes via ctx.PushIsland("{{.Name}}", "reset", nil)
  // land in channel.on listeners.
  const offReset = channel.on('reset', () => { count = 0; render() })

  return { offReset }
}

export function updated(el, newProps, instance) {
  // Optional — called when :nl-island-props changes server-side.
}

export function destroyed(el, instance) {
  instance?.offReset?.()
}
`

// --- project-config templates (Vue + React only) -----------
//
// Written at cwd on first `nexus island --vue|--react` run so
// `npm install && npm run build` works without any external
// scaffolding step. Marked shared so re-runs (and the
// second-flavor case) skip silently.
//
// Pinned to specific versions known to work with each other,
// since auto-resolving `^5.4` can pull in pre-release majors
// (Vite 8 with rolldown, observed mid-2024) that break.

const vuePkgJSONTmpl = `{
  "name": "{{.Name | lower}}-islands",
  "private": true,
  "type": "module",
  "scripts": {
    "build": "vite build",
    "dev": "vite",
    "typecheck": "vue-tsc --noEmit"
  },
  "devDependencies": {
    "@vitejs/plugin-vue": "^5.1.0",
    "fast-glob": "^3.3.0",
    "typescript": "^5.5.0",
    "vite": "5.4.20",
    "vue-tsc": "^2.0.0"
  },
  "dependencies": {
    "vue": "^3.4.0"
  }
}
`

const reactPkgJSONTmpl = `{
  "name": "{{.Name | lower}}-islands",
  "private": true,
  "type": "module",
  "scripts": {
    "build": "vite build",
    "dev": "vite",
    "typecheck": "tsc --noEmit"
  },
  "devDependencies": {
    "@types/react": "^18.3.0",
    "@types/react-dom": "^18.3.0",
    "@vitejs/plugin-react": "^4.3.0",
    "fast-glob": "^3.3.0",
    "typescript": "^5.5.0",
    "vite": "5.4.20"
  },
  "dependencies": {
    "react": "^18.3.0",
    "react-dom": "^18.3.0"
  }
}
`

const vueViteConfigTmpl = `// vite.config.ts — multi-entry build for nl-islands.
//
// Every file matching islands.src/*.island.{ts,js,jsx,tsx}
// becomes one bundle in islands/. Glob-discovered, so adding
// a new island via ` + "`nexus island --vue <name>`" + ` needs no edit
// here.
//
// vue + the runtime get externalized to esm.sh so the
// committed islands/<name>.js stays tiny (~1.5 KB). Flip the
// rollupOptions.external + output.paths block off if you'd
// rather ship a self-contained bundle.

import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
// fast-glob is CJS; named-import via default-export destructure
// is the recipe its README recommends for ESM consumers.
import fastGlob from 'fast-glob'
const { sync: globSync } = fastGlob
import { basename, resolve } from 'path'
import { fileURLToPath } from 'url'

const root = fileURLToPath(new URL('.', import.meta.url))

const entries = Object.fromEntries(
  globSync('islands.src/*.island.{ts,js,jsx,tsx}', { cwd: root }).map((file) => {
    const name = basename(file).replace(/\.island\.[jt]sx?$/, '')
    return [name, resolve(root, file)]
  }),
)

export default defineConfig({
  plugins: [vue()],
  // Inline Vue's compile-time flags into OUR code (the SFC's
  // generated render fn). The Vue runtime ITSELF (imported
  // from esm.sh below) reads them as runtime globals — those
  // are set by /__live/nexus.js at the top of the script
  // before any island dynamic-imports Vue, so you don't have
  // to do anything in your template.
  define: {
    __VUE_OPTIONS_API__: 'true',
    __VUE_PROD_DEVTOOLS__: 'false',
    __VUE_PROD_HYDRATION_MISMATCH_DETAILS__: 'false',
  },
  build: {
    outDir: 'islands',
    emptyOutDir: true,
    target: 'es2022',
    minify: 'esbuild',
    cssCodeSplit: false,
    rollupOptions: {
      input: entries,
      preserveEntrySignatures: 'strict',
      external: ['vue'],
      output: {
        entryFileNames: '[name].js',
        chunkFileNames: 'chunks/[name]-[hash].js',
        format: 'es',
        paths: {
          'vue': 'https://esm.sh/vue@3.4.0',
        },
      },
    },
  },
  server: { port: 5173, strictPort: true },
})
`

const reactViteConfigTmpl = `// vite.config.ts — multi-entry build for nl-islands.
//
// Every file matching islands.src/*.island.{ts,js,jsx,tsx}
// becomes one bundle in islands/. Glob-discovered, so adding
// a new island via ` + "`nexus island --react <name>`" + ` needs no edit
// here.
//
// react + react-dom get externalized to esm.sh so the
// committed islands/<name>.js stays tiny. Flip the
// rollupOptions.external + output.paths block off if you'd
// rather ship a self-contained bundle.

import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import fastGlob from 'fast-glob'
const { sync: globSync } = fastGlob
import { basename, resolve } from 'path'
import { fileURLToPath } from 'url'

const root = fileURLToPath(new URL('.', import.meta.url))

const entries = Object.fromEntries(
  globSync('islands.src/*.island.{ts,js,jsx,tsx}', { cwd: root }).map((file) => {
    const name = basename(file).replace(/\.island\.[jt]sx?$/, '')
    return [name, resolve(root, file)]
  }),
)

export default defineConfig({
  plugins: [react()],
  build: {
    outDir: 'islands',
    emptyOutDir: true,
    target: 'es2022',
    minify: 'esbuild',
    cssCodeSplit: false,
    rollupOptions: {
      input: entries,
      preserveEntrySignatures: 'strict',
      external: ['react', 'react-dom', 'react-dom/client'],
      output: {
        entryFileNames: '[name].js',
        chunkFileNames: 'chunks/[name]-[hash].js',
        format: 'es',
        paths: {
          'react': 'https://esm.sh/react@18.3.1',
          'react-dom': 'https://esm.sh/react-dom@18.3.1?deps=react@18.3.1',
          'react-dom/client': 'https://esm.sh/react-dom@18.3.1/client?deps=react@18.3.1',
        },
      },
    },
  },
  server: { port: 5173, strictPort: true },
})
`

const tsConfigTmpl = `{
  "compilerOptions": {
    "target": "ES2022",
    "module": "ESNext",
    "moduleResolution": "Bundler",
    "lib": ["ES2022", "DOM", "DOM.Iterable"],
    "jsx": "react-jsx",
    "strict": true,
    "noUnusedLocals": true,
    "noUnusedParameters": true,
    "isolatedModules": true,
    "skipLibCheck": true,
    "esModuleInterop": true,
    "resolveJsonModule": true,
    "types": ["vite/client"]
  },
  "include": [
    "islands.src/**/*.ts",
    "islands.src/**/*.tsx",
    "islands.src/**/*.vue",
    "vite.config.ts"
  ]
}
`

const gitignoreTmpl = `node_modules/
`

func init() {
	// Sanity check: panic at process start if any template
	// can't be parsed. Cheaper to fail loudly here than to
	// surface a template parse error to the first user who
	// runs ` + "`nexus island`" + `.
	for name, body := range map[string]string{
		"vue/component": vueComponentTmpl,
		"vue/bridge":    vueBridgeTmpl,
		"vue/pkg":       vuePkgJSONTmpl,
		"vue/vite":      vueViteConfigTmpl,
		"react/comp":    reactComponentTmpl,
		"react/bridge":  reactBridgeTmpl,
		"react/runtime": reactRuntimeTmpl,
		"react/pkg":     reactPkgJSONTmpl,
		"react/vite":    reactViteConfigTmpl,
		"tsconfig":      tsConfigTmpl,
		"gitignore":     gitignoreTmpl,
		"vanilla":       vanillaTmpl,
	} {
		if _, err := template.New(name).Funcs(islandTmplFuncs).Parse(body); err != nil {
			panic(fmt.Sprintf("nexus island: malformed embedded template %s: %v", name, err))
		}
	}
}

// islandTmplFuncs are the Go-template helpers the project-
// config templates use. `lower` lets package.json's "name"
// field be a sanitized lowercase form of the island name
// regardless of how the user capitalized it on the CLI.
var islandTmplFuncs = template.FuncMap{
	"lower": strings.ToLower,
}
