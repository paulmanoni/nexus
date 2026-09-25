import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import nexus from './sdk/nexus-vite-plugin.js'

// nexus() connects Vite to the Go app: under "vite dev" it tells the app
// where the dev server is (dist/.vite/nexus-hot.json), under "vite build"
// it writes the manifest the app reads, and it checks that every page the
// Go side registers has a component under src/Pages. Open the app's URL,
// not Vite's, so there is no proxy block.
export default defineConfig({
  plugins: [vue(), nexus()],
  resolve: {
    alias: { '@': '/src' },
  },
})
