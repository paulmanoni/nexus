# Vite frontend

A nexus frontend is an ordinary npm-managed [Vite](https://vite.dev) project under
`web/`. It can use any framework, Vite plugin or npm library. `nexus build` runs the Vite
build and embeds `web/dist` in the Go binary, so you still deploy one file, and the
server needs no Node.js.

## Create one

```bash
nexus new my-app --frontend vue      # or react
nexus new my-app --inertia [--ssr]   # server-driven Inertia pages (Vue)
nexus init --frontend vue            # add web/ to an existing project
```

Then run `go mod tidy && nexus dev`. The first run installs dependencies.

## Layout

```
web/
  package.json        vite, the framework plugin, typescript
  package-lock.json   commit it (or pnpm / yarn / bun's lockfile)
  vite.config.ts      plugins: [vue(), nexus()]
  tsconfig.json
  index.html          the entry HTML (also the Inertia page shell)
  sdk/                commit it: the Vite plugin and the typed client SDK
  src/
    main.ts
    App.vue
  dist/               build output, embedded in the binary
    index.html        a committed stub, so the first go build compiles
```

`node_modules/` and `dist/*` (except the stub) are gitignored.

## Wiring

```go
import "embed"

//go:embed all:web/dist
var webFS embed.FS

func main() {
    nexus.Boot(nexus.ServeFrontend(webFS, "web/dist"), usersModule)
}
```

`ServeFrontend` works with single-page apps:

- A path without an extension falls back to `index.html`.
- REST, GraphQL and WebSocket routes win on conflict.
- `nexus.FrontendAt("/admin")` mounts the app under a sub-path.

**Caching follows the build.** A file is served `immutable` only when the Vite manifest
lists it and its name carries a content hash. Everything else, including `index.html`,
is revalidated with an ETag.

In production, boot fails fast if the bundle has neither `index.html` nor a Vite
manifest. In development, an unbuilt bundle serves a placeholder page.

## Development: one origin

The Vite plugin connects the two processes:

```ts
// web/vite.config.ts
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import nexus from './sdk/nexus-vite-plugin.js'

export default defineConfig({
  plugins: [vue(), nexus()],
})
```

While `vite dev` runs, the plugin writes a small hot file
(`web/dist/.vite/nexus-hot.json`) with the dev server's real address, and removes it on
exit. The Go app reads that file, and pages it serves load their modules from Vite with
full HMR.

**You always open the app's own origin** (`http://localhost:8080`). There is no proxy
block to maintain and no second URL. `nexus dev` runs Vite for you. Running
`npm run dev` and `go run .` in two terminals works the same way.

The hot file is followed only under `nexus dev` or with `environment = "development"`,
and only while its dev server is alive. A file left behind by a killed Vite is ignored.

## Production build

```bash
nexus build -o my-app
```

1. Installs dependencies if `node_modules/.bin/vite` is missing. It uses the project's
   own package manager (from `packageManager` or the lockfile), frozen to the lockfile.
2. Refreshes `web/sdk/nexus-vite-plugin.*`.
3. Runs `vite build`, with `build.manifest` forced on.
4. Runs `vite build --ssr src/ssr.ts` too, when that file exists.
5. Runs `go build`, which embeds `web/dist`.

A directory without a `package.json` is served as-is and never built. That fits a
hand-written or prebuilt `dist`.

## Which directory

`nexus dev` and `nexus build` pick the frontend directory in this order:

1. the `--frontend <dir>` flag
2. the `NEXUS_FRONTEND_DIR` environment variable
3. the directory named in `main.go`'s `ServeFrontend` call
4. `web/`, when it has a `package.json`

## Values from `nexus.toml`

`[env]` values reach frontend code as `import.meta.env.<dotted.key>`. See
[the `[env]` bridge](./configuration#the-env-bridge).
