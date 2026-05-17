// vite.config.ts — multi-entry build for nl-islands.
//
// Every file matching islands.src/*.island.{js,ts,jsx,tsx}
// becomes one bundle in islands/. Glob-discovered so adding a
// new island is just creating the file — no config edits.
//
// Output naming: <name>.island.ts → islands/<name>.js. Drop
// the .island.* suffix so <nl-island src="/islands/<name>.js">
// reads naturally in your templates.

import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import react from '@vitejs/plugin-react'
import { sync as globSync } from 'fast-glob'
import { basename, resolve } from 'path'
import { fileURLToPath } from 'url'

const root = fileURLToPath(new URL('.', import.meta.url))

const entries = Object.fromEntries(
  globSync('islands.src/*.island.{js,ts,jsx,tsx}', { cwd: root }).map((file) => {
    const name = basename(file).replace(/\.island\.[jt]sx?$/, '')
    return [name, resolve(root, file)]
  }),
)

export default defineConfig({
  plugins: [
    vue(),
    react(),
  ],
  build: {
    outDir: 'islands',
    emptyOutDir: true,
    // ES modules — they're what `import()` on the client
    // resolves to. No IIFE / UMD wrappers needed; the island
    // contract is "load via dynamic import, call exported
    // mount/updated/destroyed".
    target: 'es2022',
    rollupOptions: {
      input: entries,
      output: {
        entryFileNames: '[name].js',
        chunkFileNames: 'chunks/[name]-[hash].js',
        format: 'es',
      },
    },
  },
  server: {
    port: 5173,
    strictPort: true,
    // CORS off for proxy-mode (nexus dev fronts /islands/*).
    // Set to true if you serve islands directly from :5173
    // and the nexus app from a different port in dev.
    cors: false,
  },
})
