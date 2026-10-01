import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import nexus from './sdk/nexus-vite-plugin.js'

// No index.html and no entry: this frontend is only the islands under
// src/islands, which the templ pages mount. ssr also builds
// dist/ssr/islands.js, the server view.SSR islands render on.
// The built files go under /islands/: /assets/ is the app's own (main.go).
export default defineConfig({
  plugins: [vue(), nexus({ islands: { ssr: true } })],
  build: { assetsDir: 'islands' },
})
