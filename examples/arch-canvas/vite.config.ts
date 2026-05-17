// vite.config.ts — builds islands.src/*.island.ts into
// islands/<name>.js. Vue + @vue-flow/core are externalized to
// esm.sh URLs so the committed bundle stays small (~5 KB
// instead of ~250 KB) and `go run ./examples/arch-canvas`
// still works without anyone running `npm install` first.
//
// For a real production build you'd flip externals OFF and
// commit the full bundle locally — that's a one-line change
// (drop the rollupOptions.external + output.paths block).

import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
// fast-glob is CJS; named-imports don't work from an ESM
// config file. Default-import + destructure is the recipe
// the lib itself recommends.
import fastGlob from 'fast-glob'
const { sync: globSync } = fastGlob
import { basename, resolve } from 'path'
import { fileURLToPath } from 'url'

const root = fileURLToPath(new URL('.', import.meta.url))

const entries = Object.fromEntries(
  globSync('islands.src/*.island.{ts,js,tsx,jsx}', { cwd: root }).map((file) => {
    const name = basename(file).replace(/\.island\.[jt]sx?$/, '')
    return [name, resolve(root, file)]
  }),
)

export default defineConfig({
  plugins: [vue()],
  // Define Vue's compile-time flags as build-time constants
  // so the bundled output doesn't need the inline <script>
  // hack the prior version of this example used to set them
  // on window. Cleaner than runtime globals.
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
    // Inline the SFC's scoped styles directly into the JS
    // bundle instead of emitting a separate hashed .css
    // file under islands/assets/. Keeps the island self-
    // contained — one file, one URL, no asset directory to
    // ship.
    cssCodeSplit: false,
    cssTarget: 'es2022',
    rollupOptions: {
      input: entries,
      // Treat each entry as a library, NOT as an app bundle.
      // Without this, rollup tree-shakes the
      // mount/updated/destroyed exports away because nothing
      // inside the module consumes them — they're called
      // externally by /__live/nexus.js after dynamic import().
      preserveEntrySignatures: 'strict',
      // Externalize the heavy deps so the bundle is just
      // OUR code (the SFC's compiled template/script + the
      // bridge). The browser loads vue + vue-flow from
      // esm.sh at runtime, sharing the cached copy if any
      // other island uses the same versions.
      external: ['vue', '@vue-flow/core'],
      output: {
        entryFileNames: '[name].js',
        chunkFileNames: 'chunks/[name]-[hash].js',
        format: 'es',
        // Rewrite the externals' import paths in the built
        // output to esm.sh URLs. Without this, the browser
        // would try to resolve "vue" as a bare specifier —
        // and bare specifiers don't work in plain <script
        // type="module">.
        paths: {
          'vue': 'https://esm.sh/vue@3.4.0',
          '@vue-flow/core': 'https://esm.sh/@vue-flow/core@1.41.0?deps=vue@3.4.0',
        },
      },
    },
  },
  server: {
    port: 5173,
    strictPort: true,
  },
})
