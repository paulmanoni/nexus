import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// The dashboard SPA is embedded into the Go binary and served at /__nexus/
// (see extension/dashboard/ui.go: //go:embed all:ui/dist + the /__nexus
// mount). base must match so Vite stamps /__nexus/assets/... URLs into the
// built index.html. Output goes to dist/ with hashed files under assets/,
// which the embed + the /__nexus/assets/*filepath handler expect.
export default defineConfig({
  base: '/__nexus/',
  plugins: [vue()],
  build: {
    outDir: 'dist',
    assetsDir: 'assets',
    emptyOutDir: true,
  },
})
