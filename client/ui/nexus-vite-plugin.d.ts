// Type declarations for nexus-vite-plugin.js, dumped next to it so the
// TypeScript compiler auto-pairs the two whenever vite.config.ts does
//   import nexus from './sdk/nexus-vite-plugin.js'
// Without this, the import resolves to `any` under a strict tsconfig and
// noImplicitAny flags the call. The runtime stays plain JS (Vite loads
// vite.config.ts itself); this file only supplies the types.
//
// `Plugin` is imported as a type from 'vite', which any npm-managed Vite
// project already has on disk — so the reference resolves with no extra
// install. The factory returns Plugin[] (four sub-plugins in one).

import type { Plugin } from 'vite'

export interface NexusVitePluginOptions {
  /**
   * SDK directory holding manifest.json, absolute or relative to the
   * project root. The auto-select + manifest-filter plugins read it.
   * Default: 'src/sdk'.
   */
  sdkDir?: string
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
}

/**
 * nexus's Vite plugin bundle — auto-select, manifest-filter, loop-guard
 * and the dev codegen→HMR bridge. Spread the result into the `plugins`
 * array of vite.config.ts.
 */
export default function nexusAutoSelect(options?: NexusVitePluginOptions): Plugin[]
