// Type declarations for nexus-vite-plugin.js, dumped next to it so the
// TypeScript compiler auto-pairs the two whenever vite.config.ts does
//   import nexus from './sdk/nexus-vite-plugin.js'
// Without this, the import resolves to `any` under a strict tsconfig and
// noImplicitAny flags the call. The runtime stays plain JS (Vite loads
// vite.config.ts itself); this file only supplies the types.
//
// `Plugin` is imported as a type from 'vite', which any npm-managed Vite
// project already has on disk — so the reference resolves with no extra
// install. The factory returns Plugin[] (six sub-plugins in one).

import type { Plugin } from 'vite'

export interface NexusVitePluginOptions {
  /**
   * SDK directory holding manifest.json, absolute or relative to the
   * Vite root. The auto-select, manifest-filter and page checks read it,
   * and the plugin aliases the bare import 'nexus-client' to its
   * client.js (the Go side maps the same name in tsconfig paths).
   * Default: 'sdk' — web/sdk, where the Go app writes the SDK in dev.
   * Left unset, a project that only has src/sdk/manifest.json (the old
   * default) keeps reading that one.
   */
  sdkDir?: string
  /**
   * Inertia pages directory, absolute or relative to the Vite root.
   * Every component the manifest names (inertia.Page's component, the
   * endpoint's `page`) must have a file <pages>/<Name>.vue — or .tsx,
   * .jsx, .svelte, .ts, .js — matched case-exactly, as import.meta.glob
   * keys are. `vite dev` warns once per missing component (again when
   * the Go app rewrites the manifest); `vite build` fails listing them.
   * Without a manifest, or with no pages in it, nothing is checked.
   * `false` turns the check off. Default: 'src/Pages'.
   */
  pages?: string | false
  /**
   * Manifest projection mode. 'usage' walks the source tree at build
   * time and ships only the endpoints the app references; 'off' (the
   * default) keeps the full manifest. Build-only — dev always serves
   * the whole manifest for HMR + endpoint discovery.
   */
  filter?: 'usage' | 'off'
  /**
   * Strictness for dynamic endpoint calls when filter === 'usage'.
   * 'loose' (default) tolerates a dynamic first arg by including
   * everything that build with a warning; 'strict' errors unless every
   * dynamic call carries a `// @nexus-include …` pragma.
   */
  filterMode?: 'strict' | 'loose'
  /**
   * Roots the usage walker scans when filter === 'usage'. Relative to
   * the project root or absolute. Default: ['src'].
   */
  scanInclude?: string[]
  /**
   * Typed-codegen directory watched in dev; a write here fires a Vite
   * full-reload so a changed Go endpoint reaches the browser. Default:
   * 'src/__nexus'.
   */
  codegenDir?: string
  /**
   * optimizeDeps.entries globs the dev server pre-bundles at startup, so
   * deps used only inside Inertia pages (resolved via import.meta.glob,
   * which the scanner doesn't follow) are optimized up front instead of
   * being discovered lazily on navigation — which would force a full
   * reload and break HMR. Default: ['index.html', 'src/**\/*.{vue,ts,tsx,js,jsx}'].
   */
  optimizeEntries?: string[]
  /**
   * The app's entry module(s), root-relative — e.g. 'src/main.ts'.
   * Declared once here, it becomes build.rollupOptions.input (unless
   * that is already set; a conflicting value is warned about and wins),
   * the key nexus looks up in .vite/manifest.json, and the `entries` of
   * the dev hot file. Omit it for an index.html-driven SPA.
   */
  input?: string | string[]
  /**
   * Origin(s) the Go app is visited on that the dev server should answer
   * cross-origin module requests from, e.g. 'https://myapp.example:8443'.
   * The page lives on the app's origin and loads its modules from Vite, so
   * Vite must allow that origin. Without server.cors in your config the
   * plugin already allows, on any port: localhost, *.localhost, 127.0.0.0/8,
   * [::1], *.test names and every address of this machine (so a phone on
   * the LAN visiting http://<your-ip>:8080 works). List anything else here;
   * each value is matched exactly as an origin. Ignored (with a warning)
   * when server.cors is set — allow the origin there instead.
   */
  appOrigin?: string | string[]
}

/**
 * nexus's Vite plugin bundle — auto-select, manifest-filter, loop-guard,
 * the dev codegen→HMR bridge, the page-component check (see pages), and
 * the nexus handshake: under `vite dev` it writes
 * <outDir>/.vite/nexus-hot.json with the dev server's real origin
 * (removed on shutdown; a wildcard `--host` bind is written as the
 * machine's network address so LAN clients reach it), sets server.origin
 * so assets resolve cross-origin, and — unless server.cors is set — a CORS
 * allowlist for the app's origin (see appOrigin). Under `vite build` it
 * forces build.manifest, and a build into the outDir of a running dev
 * server puts that server's hot file back after emptyOutDir. Spread the
 * result into the `plugins` array of vite.config.ts.
 */
export default function nexusAutoSelect(options?: NexusVitePluginOptions): Plugin[]
