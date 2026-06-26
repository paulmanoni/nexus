import { defineConfig } from 'viteless'

// viteless reads this config's static surface (base, plugins, resolve.alias,
// server.proxy, build.outDir). Vue is detected and compiled natively — no
// plugin needed. A relative alias is resolved against the project root.
export default defineConfig({
  resolve: {
    alias: { '@': './src' },
  },
})
