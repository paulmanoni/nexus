package main

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"text/template"
)

// scaffoldOpts captures every choice the scaffolder needs. Flags
// and the interactive prompt both fill the same struct so the
// template path stays identical.
type scaffoldOpts struct {
	Dir        string
	ModulePath string
	Name       string // basename of Dir, used for human-readable strings
	Frontend   string // "none" | "vue" | "react"
	Tooling    string // "viteless" (default) | "vite" — frontend build engine
	DB         string // "none" | "postgres" | "mysql" | "sqlite"
	Cache      string // "none" | "redis"
	Auth       string // "none" | "oauth2"
	Inertia    bool   // Inertia.js server-driven pages (Vue) on top of the frontend
	SSR        bool   // Inertia server-side rendering (needs Node; implies Inertia + vite)
}

// Predicate helpers for the templates so they stay free of empty-
// string comparisons. Lowercase exported names so text/template
// can call them by their bare-method form.
func (o scaffoldOpts) HasDB() bool       { return o.DB != "" && o.DB != "none" }
func (o scaffoldOpts) HasCache() bool    { return o.Cache != "" && o.Cache != "none" }
func (o scaffoldOpts) HasFrontend() bool { return o.Frontend != "" && o.Frontend != "none" }
func (o scaffoldOpts) HasAuth() bool     { return o.Auth != "" && o.Auth != "none" }
func (o scaffoldOpts) HasResources() bool {
	return o.HasDB() || o.HasCache()
}
func (o scaffoldOpts) IsVue() bool   { return o.Frontend == "vue" }
func (o scaffoldOpts) IsReact() bool { return o.Frontend == "react" }

// IsInertia reports whether to scaffold Inertia.js server-driven pages.
// Gated on a Vue frontend — Inertia rides on top of the Vue project.
func (o scaffoldOpts) IsInertia() bool { return o.Inertia && o.IsVue() }

// IsInertiaSSR reports whether to scaffold Inertia server-side rendering — the
// hydrating client entry, the SSR bundle entry, and the Go-side SSR wiring.
// Gated on Inertia (which gates on Vue).
func (o scaffoldOpts) IsInertiaSSR() bool { return o.SSR && o.IsInertia() }

// HasVite reports whether the frontend should be scaffolded as a standard
// npm-managed Vite project (vs the zero-install viteless default).
func (o scaffoldOpts) HasVite() bool { return o.HasFrontend() && o.Tooling == "vite" }

// renderTemplate executes a text/template string against opts and
// returns the rendered bytes. Panic-free helper used by every
// per-file template below.
func renderTemplate(name, body string, opts scaffoldOpts) (string, error) {
	t, err := template.New(name).Parse(body)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", name, err)
	}
	var b bytes.Buffer
	if err := t.Execute(&b, opts); err != nil {
		return "", fmt.Errorf("exec %s: %w", name, err)
	}
	return b.String(), nil
}

// buildFiles assembles path → contents for the chosen options.
// Empty paths in returned map become directories implicitly via
// MkdirAll on the parent of each written file.
func buildFiles(opts scaffoldOpts) (map[string]string, error) {
	out := map[string]string{}
	add := func(path, body string) error {
		rendered, err := renderTemplate(path, body, opts)
		if err != nil {
			return err
		}
		out[path] = rendered
		return nil
	}
	if err := add("go.mod", tmplGoMod2); err != nil {
		return nil, err
	}
	if err := add("main.go", tmplMainGoTpl); err != nil {
		return nil, err
	}
	if err := add("module.go", tmplModuleGo); err != nil {
		return nil, err
	}
	if err := add(".gitignore", tmplGitignoreTpl); err != nil {
		return nil, err
	}
	if err := add("README.md", tmplReadmeTpl); err != nil {
		return nil, err
	}
	if err := add("nexus.toml", tmplDeployTOML); err != nil {
		return nil, err
	}
	if opts.HasResources() {
		if err := add(".env.example", tmplEnvExampleTpl); err != nil {
			return nil, err
		}
	}
	if opts.HasDB() {
		if err := add("resources/database.go", tmplDatabaseGoTpl); err != nil {
			return nil, err
		}
	}
	if opts.HasCache() {
		if err := add("resources/cache.go", tmplCacheGo); err != nil {
			return nil, err
		}
	}
	if opts.HasFrontend() {
		// Frontend project under web/, served by the embedded viteless
		// engine — zero-Node by default (deps from esm.sh, cached). No
		// package.json, no node_modules, no npm. `nexus dev` runs the
		// viteless HMR dev server; `nexus build` produces web/dist, embedded
		// via //go:embed all:web/dist in main.go. A committed
		// web/dist/index.html stub makes the first `go build` compile.
		// viteless.config.ts is read for config (alias/proxy/plugins); the
		// committed tsconfig.json + viteless-env.d.ts keep the editor's
		// TypeScript happy with nothing installed. Run `npm install` to opt
		// into node_modules (or an installed Vite) instead.
		if err := add("web/index.html", tmplViteIndexHTML); err != nil {
			return nil, err
		}
		if err := add("web/tsconfig.json", tmplViteTSConfig); err != nil {
			return nil, err
		}
		if err := add("web/dist/index.html", tmplViteDistStub); err != nil {
			return nil, err
		}
		if opts.HasVite() {
			// Standard npm-managed Vite project: package.json + vite.config.ts
			// (with a dev proxy to the Go app) + the framework's Vite plugin.
			// viteless delegates to the installed Vite. `npm install` first.
			if err := add("web/vite.config.ts", tmplViteConfig); err != nil {
				return nil, err
			}
			pkg := tmplViteVuePackageJSON
			if opts.IsReact() {
				pkg = tmplViteReactPackageJSON
			}
			if opts.IsInertia() {
				pkg = tmplViteInertiaPackageJSON
			}
			if opts.IsInertiaSSR() {
				pkg = tmplViteInertiaSSRPackageJSON
			}
			if err := add("web/package.json", pkg); err != nil {
				return nil, err
			}
		} else {
			// Zero-install viteless project: viteless.config.ts + ambient
			// types; no package.json / node_modules.
			if err := add("web/viteless.config.ts", tmplVitelessConfig); err != nil {
				return nil, err
			}
			envDTS := tmplVitelessEnvDTS
			if opts.IsInertia() {
				envDTS = tmplVitelessInertiaEnvDTS
			}
			if err := add("web/viteless-env.d.ts", envDTS); err != nil {
				return nil, err
			}
		}
		switch {
		case opts.IsInertiaSSR():
			// SSR: the client entry HYDRATES the server-rendered markup
			// (createSSRApp), and ssr.ts is the Node SSR bundle entry
			// (createServer + renderToString). The sample page is shared.
			if err := add("web/src/main.ts", tmplInertiaSSRMainTS); err != nil {
				return nil, err
			}
			if err := add("web/src/ssr.ts", tmplInertiaSSRTS); err != nil {
				return nil, err
			}
			if err := add("web/src/Pages/Home.vue", tmplInertiaHomeVue); err != nil {
				return nil, err
			}
		case opts.IsInertia():
			// Inertia entry + a sample page component; the page's props
			// come from the Go handler in pages.go (added below).
			if err := add("web/src/main.ts", tmplInertiaMainTS); err != nil {
				return nil, err
			}
			if err := add("web/src/Pages/Home.vue", tmplInertiaHomeVue); err != nil {
				return nil, err
			}
		case opts.IsVue():
			if err := add("web/src/main.ts", tmplMainTS); err != nil {
				return nil, err
			}
			if err := add("web/src/App.vue", tmplAppVueTpl); err != nil {
				return nil, err
			}
		case opts.IsReact():
			if err := add("web/src/main.tsx", tmplMainTSXTpl); err != nil {
				return nil, err
			}
			if err := add("web/src/App.tsx", tmplAppTSXTpl); err != nil {
				return nil, err
			}
		}
	}
	// Inertia adds a Go page module (a server-rendered "/" page) alongside
	// the REST hello example.
	if opts.IsInertia() {
		if err := add("pages.go", tmplPagesGo); err != nil {
			return nil, err
		}
	}
	if opts.HasAuth() {
		if err := add("auth/auth.go", tmplAuthGoTpl); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// nextStepsLines returns the per-option follow-up commands the
// scaffolder prints. Order matches the flow a fresh user takes:
// install Go deps → install npm deps → run.
func nextStepsLines(opts scaffoldOpts) []string {
	lines := []string{
		"  cd " + opts.Dir,
		"  go mod tidy",
	}
	if opts.HasVite() {
		// Standard Vite project — install npm deps (one-time).
		lines = append(lines,
			"  cd web && npm install   # install Vite + framework plugin",
			"  cd ..",
		)
	}
	// Otherwise (viteless) the frontend needs no install — viteless fetches
	// deps on first run (zero-Node).
	if opts.HasResources() {
		lines = append(lines,
			"  cp .env.example .env    # then fill in real credentials",
		)
	}
	lines = append(lines,
		"  nexus dev               # go run + viteless dev server (HMR); dashboard at /__nexus/",
	)
	if opts.HasFrontend() {
		lines = append(lines,
			"                          # SPA on http://localhost:5173 (zero-install) ; nexus build embeds web/dist",
		)
	}
	if opts.IsInertiaSSR() {
		lines = append(lines,
			"",
			"  # Server-side rendering (production): build both bundles, then run the SSR sidecar",
			"  cd web && npm run build   # vite build + vite build --ssr → web/dist/ssr/ssr.js",
			"  node web/dist/ssr/ssr.js  # the Node SSR server on :13714 (run alongside the app)",
			"  # In nexus dev the sidecar isn't running — pages render client-side with HMR.",
		)
	}
	return lines
}

// tmplVitelessConfig is the scaffolded web/viteless.config.ts. It imports
// defineConfig from 'viteless' (no vite package needed) and sets the "@/"
// alias. viteless reads it for config and compiles the framework natively.
const tmplVitelessConfig = `import { defineConfig } from 'viteless'

// viteless reads this config's static surface (base, plugins, resolve.alias,
// server.proxy, build.outDir). Vue/React are detected and compiled natively
// — no plugin needed. A relative alias is resolved against the project root.
export default defineConfig({
  resolve: {
    alias: { '@': './src' },
  },
})
`

// tmplVitelessEnvDTS is the scaffolded web/viteless-env.d.ts — ambient types
// so the editor resolves project imports with nothing installed.
const tmplVitelessEnvDTS = `// Ambient declarations so TypeScript resolves a viteless project's imports
// with nothing installed. viteless reads viteless.config.ts itself.

declare module "viteless" {
  export function defineConfig<T>(config: T): T
}

declare module "*.vue" {
  import type { DefineComponent } from "vue"
  const component: DefineComponent<{}, {}, any>
  export default component
}
`

// tmplViteConfig is the standard npm-Vite project's vite.config.ts. Unlike
// the viteless engine (which proxies to the Go app automatically), a real
// Vite dev server needs its own server.proxy to reach the backend — so the
// nexus prefixes are proxied to :8080 here (adjust if your app binds
// elsewhere).
const tmplViteConfig = `import { defineConfig } from 'vite'
{{if .IsReact}}import react from '@vitejs/plugin-react'{{else}}import vue from '@vitejs/plugin-vue'{{end}}

export default defineConfig({
  plugins: [{{if .IsReact}}react(){{else}}vue(){{end}}],
  server: {
    proxy: {
      '/__nexus': 'http://localhost:8080',
      '/graphql': 'http://localhost:8080',
      '/oauth': 'http://localhost:8080',
      '/ws': { target: 'http://localhost:8080', ws: true },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
})
`

const tmplViteVuePackageJSON = `{
  "name": "{{.Name}}-web",
  "private": true,
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "vite build",
    "preview": "vite preview"
  },
  "dependencies": {
    "vue": "^3.5.0"
  },
  "devDependencies": {
    "@vitejs/plugin-vue": "^5.2.0",
    "vite": "^5.4.0"
  }
}
`

const tmplViteReactPackageJSON = `{
  "name": "{{.Name}}-web",
  "private": true,
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "vite build",
    "preview": "vite preview"
  },
  "dependencies": {
    "react": "^18.3.0",
    "react-dom": "^18.3.0"
  },
  "devDependencies": {
    "@vitejs/plugin-react": "^4.3.0",
    "vite": "^5.4.0"
  }
}
`

// ── templates ───────────────────────────────────────────────────────

const tmplGoMod2 = `module {{.ModulePath}}

go 1.25.1
`

// tmplMainGoTpl wires only the chosen pieces. Empty branches collapse
// to nothing so the generated file is never noisy with stubs.
const tmplMainGoTpl = `package main

import (
{{- if .HasFrontend}}
	"embed"
{{end}}
	"github.com/paulmanoni/nexus"
{{- if .IsInertia}}
	"github.com/paulmanoni/nexus/extension/inertia"
{{- end}}
{{- if .IsInertiaSSR}}
	"github.com/paulmanoni/nexus/extension/inertia/ssrhttp"
{{- end}}
{{- if .HasResources}}
	"go.uber.org/zap"
{{- end}}
{{- if or .HasResources .HasAuth}}

{{- end}}
{{- if .HasResources}}
	"{{.ModulePath}}/resources"
{{- end}}
{{- if .HasAuth}}
	"{{.ModulePath}}/auth"
{{- end}}
{{- if .HasResources}}

	_ "github.com/joho/godotenv/autoload"
{{- end}}
)
{{if .HasFrontend}}
// webFS holds the Vite-built SPA (web/dist). nexus build runs npm run
// build before go build, so web/dist is populated; this embed bakes it
// into the binary. A committed web/dist/index.html stub lets go build
// succeed before the first frontend build.
//
//go:embed all:web/dist
var webFS embed.FS
{{end}}
func main() {
	// nexus.Boot loads nexus.toml automatically — runtime config (server
	// addr, dashboard, introspection, environment, …), every [extensions.*]
	// block, and the nexus.Get value store — then runs the app. Edit
	// nexus.toml to change settings without touching code; absent fields
	// fall back to framework defaults. (Use nexus.Run if you'd rather build
	// Config in Go.)
	nexus.Boot(
{{- if .HasFrontend}}
		nexus.ServeFrontend(webFS, "web/dist"),
{{- end}}
{{- if .IsInertiaSSR}}
		// Inertia pages render through the engine; ServeFrontend above
		// still serves the built JS/CSS assets the shell references. SSR
		// POSTs each initial page to the Node SSR server (run
		// "node web/dist/ssr/ssr.js", default :13714) and hydrates its
		// markup; any renderer error falls back to client rendering, so a
		// down SSR sidecar never takes the page down. In "nexus dev" the
		// sidecar isn't running, so pages render client-side with HMR.
		inertia.Module(inertia.Config{
			Frontend: webFS,
			Root:     "web/dist",
			SSR:      ssrhttp.New(""), // "" → http://127.0.0.1:13714
		}),
{{- else if .IsInertia}}
		// Inertia pages render through the engine; ServeFrontend above
		// still serves the built JS/CSS assets the shell references.
		inertia.Module(inertia.Config{Frontend: webFS, Root: "web/dist"}),
{{- end}}
{{- if .HasResources}}
		nexus.Provide(zap.NewExample),
{{- end}}
{{- if .HasDB}}
		nexus.Provide(resources.NewDB),
{{- end}}
{{- if .HasCache}}
		nexus.Provide(resources.NewCacheManager),
{{- end}}
{{- if .HasAuth}}
		auth.Module,
{{- end}}
{{- if .IsInertia}}
		pagesModule,
{{- end}}
		helloModule,
	)
}
`

const tmplModuleGo = `package main

import "github.com/paulmanoni/nexus"

// HelloService — typed wrapper around *nexus.Service so fx can route
// by type. Every handler that declares *HelloService as a dep grounds
// under the "hello" service on the dashboard's Architecture view.
type HelloService struct{ *nexus.Service }

func NewHelloService(app *nexus.App) *HelloService {
	return &HelloService{app.Service("hello").Describe("Hello world")}
}

type HelloResponse struct {
	Message string ` + "`json:\"message\"`" + `
}

type HelloArgs struct {
	Name string ` + "`graphql:\"name\" json:\"name\"`" + `
}

func NewHello(svc *HelloService, p nexus.Params[HelloArgs]) (*HelloResponse, error) {
	name := p.Args.Name
	if name == "" {
		name = "world"
	}
	return &HelloResponse{Message: "hello, " + name}, nil
}

var helloModule = nexus.Module("hello",
	nexus.Provide(NewHelloService),
	nexus.AsRest("GET", "/hello", NewHello),
)
`

const tmplGitignoreTpl = `/bin/
/dist/
/vendor/
*.test
*.out
.DS_Store
.env
{{if .HasFrontend}}
# Vite frontend (web/). npm deps + build output are not committed, EXCEPT
# the web/dist/index.html stub so a fresh clone's first go build (before
# any npm run build) can satisfy //go:embed all:web/dist.
/web/node_modules/
/web/dist/*
!/web/dist/index.html
{{end}}`

const tmplEnvExampleTpl = `# Copy this file to .env and fill in real credentials.
{{if .HasDB}}
# Database
DB_HOST=localhost
{{- if eq .DB "postgres"}}
DB_PORT=5432
{{- else if eq .DB "mysql"}}
DB_PORT=3306
{{- end}}
DB_USER=postgres
DB_PASS=
DB_NAME={{.Name}}
{{end}}
{{- if .HasCache}}
# Cache (Redis with in-memory fallback)
APP_ENV=development
REDIS_HOST=localhost
REDIS_PORT=6379
REDIS_PASSWORD=
{{end}}`

// tmplDatabaseGoTpl mirrors portal_admin/resources/database.go but
// parameterized over the chosen driver.
const tmplDatabaseGoTpl = `package resources

import (
	"os"

	"github.com/paulmanoni/nexus/db"
	"github.com/paulmanoni/nexus/resource"
	"go.uber.org/zap"
)

type DB struct {
	*db.Manager
}

func NewDB(logger *zap.Logger) *DB {
	m := db.NewManager(db.Config{
{{- if eq .DB "postgres"}}
		Driver:   db.Postgres,
		Host:     os.Getenv("DB_HOST"),
		Port:     os.Getenv("DB_PORT"),
		User:     os.Getenv("DB_USER"),
		Password: os.Getenv("DB_PASS"),
		Database: os.Getenv("DB_NAME"),
		SSLMode:  "disable",
{{- else if eq .DB "mysql"}}
		Driver:   db.MySQL,
		Host:     os.Getenv("DB_HOST"),
		Port:     os.Getenv("DB_PORT"),
		User:     os.Getenv("DB_USER"),
		Password: os.Getenv("DB_PASS"),
		Database: os.Getenv("DB_NAME"),
{{- else if eq .DB "sqlite"}}
		Driver:   db.SQLite,
		Database: "{{.Name}}.db",
{{- end}}
	}, db.WithLogger(logger))
	m.Start()
	return &DB{m}
}

// NexusResources surfaces the connection on the dashboard's
// Resources panel with a live "connected?" indicator.
func (m *DB) NexusResources() []resource.Resource {
	driver := string(m.Driver())
	return []resource.Resource{
		resource.NewDatabase(
			"main", "GORM — "+driver,
			map[string]any{"engine": driver},
			m.IsConnected,
			resource.AsDefault(),
		),
	}
}
`

const tmplCacheGo = `package resources

import (
	"github.com/paulmanoni/nexus/extension/cache"
	"github.com/paulmanoni/nexus/resource"
	"go.uber.org/zap"
)

type CacheManager struct {
	*cache.Manager
}

// NewCacheManager reads APP_ENV / REDIS_HOST / REDIS_PORT /
// REDIS_PASSWORD from the environment. When APP_ENV is "production"
// the manager keeps trying Redis in the background; otherwise it
// stays on the in-memory store and never blocks startup on a
// missing Redis.
func NewCacheManager(logger *zap.Logger) *CacheManager {
	m := cache.NewManager(cache.NewConfig(), logger)
	m.Start()
	return &CacheManager{m}
}

func (c *CacheManager) NexusResources() []resource.Resource {
	return []resource.Resource{
		resource.NewCache(
			"session", "Redis with in-memory fallback",
			map[string]any{"ttl": "30m"},
			c.IsRedisConnected,
			resource.AsDefault(),
			resource.WithDetails(func() map[string]any {
				backend := "memory"
				if c.IsRedisConnected() {
					backend = "redis"
				}
				return map[string]any{"backend": backend, "ttl": "30m"}
			}),
		),
	}
}
`

// ── Vite frontend (web/) ────────────────────────────────────────────

const tmplViteIndexHTML = `<!DOCTYPE html>
<html lang="en">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>{{.Name}}</title>
  </head>
  <body>
    <div id="app"></div>
    <script type="module" src="/src/main.{{if .IsReact}}tsx{{else}}ts{{end}}"></script>
  </body>
</html>
`

const tmplViteTSConfig = `{
  "compilerOptions": {
    "target": "ES2022",
    "module": "ESNext",
    "moduleResolution": "bundler",
    "strict": true,
    "jsx": "preserve",
    "skipLibCheck": true,
    "esModuleInterop": true,
    "resolveJsonModule": true,
    "noEmit": true,
    "baseUrl": ".",
    "paths": { "@/*": ["./src/*"] },
    "lib": ["ES2022", "DOM", "DOM.Iterable"]
  },
  "include": ["src", "*.ts", "viteless-env.d.ts"]
}
`

// tmplViteDistStub is a minimal valid SPA shell committed at web/dist/index.html
// so the first ` + "`go build`" + ` (before any ` + "`vite build`" + `) compiles the
// //go:embed all:web/dist directive and the binary boots. Overwritten by the
// real build output on ` + "`nexus build`" + ` / ` + "`npm run build`" + `.
const tmplViteDistStub = `<!DOCTYPE html>
<html lang="en">
  <head><meta charset="UTF-8" /><title>{{.Name}}</title></head>
  <body>
    <div id="app"></div>
    <p style="font-family:system-ui;padding:2rem">Frontend not built yet — run
    <code>cd web &amp;&amp; npm install &amp;&amp; npm run build</code>
    (or <code>nexus build</code>).</p>
  </body>
</html>
`

// ── vue entry files (web/src) ───────────────────────────────────────

const tmplMainTS = `import { createApp } from 'vue'
import App from './App.vue'

createApp(App).mount('#app')
`

const tmplAppVueTpl = `<script setup lang="ts">
import { ref } from 'vue'
const count = ref(0)
</script>

<template>
  <main>
    <h1>{{.Name}}</h1>
    <p>Edit <code>web/src/App.vue</code> — <code>nexus dev</code> hot-reloads on save.</p>
    <button @click="count++">count is {{ "{{ count }}" }}</button>
  </main>
</template>

<style scoped>
main { font-family: system-ui, sans-serif; padding: 2rem; max-width: 40rem; }
button { padding: .5rem 1rem; border-radius: .25rem; cursor: pointer; }
</style>
`

// ── inertia entry files (web/src) ───────────────────────────────────

// tmplInertiaMainTS bootstraps the Inertia Vue adapter. Page components
// under src/Pages are resolved by the name the Go handler passes to
// inertia.Page (e.g. "Home" → src/Pages/Home.vue).
const tmplInertiaMainTS = `import { createInertiaApp } from '@inertiajs/vue3'
import { createApp, h } from 'vue'

createInertiaApp({
  resolve: (name) => {
    const pages = import.meta.glob('./Pages/**/*.vue', { eager: true })
    return pages['./Pages/' + name + '.vue']
  },
  setup({ el, App, props, plugin }) {
    createApp({ render: () => h(App, props) }).use(plugin).mount(el)
  },
})
`

// tmplInertiaHomeVue is the sample page component. Its props (message)
// are supplied by NewHome in pages.go — no client API call.
const tmplInertiaHomeVue = `<script setup lang="ts">
defineProps<{ message: string }>()
</script>

<template>
  <main>
    <h1>{{.Name}}</h1>
    <p>{{ "{{ message }}" }}</p>
    <p>Inertia page — edit <code>web/src/Pages/Home.vue</code>. Props come from
    <code>NewHome</code> in <code>pages.go</code>; ` + "`nexus dev`" + ` hot-reloads on save.</p>
  </main>
</template>

<style scoped>
main { font-family: system-ui, sans-serif; padding: 2rem; max-width: 40rem; }
</style>
`

// tmplPagesGo defines the Go side of the sample Inertia page: a reflective
// handler returning a typed props struct, mounted with inertia.Page.
const tmplPagesGo = `package main

import (
	"context"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/inertia"
)

// HomeProps is the prop bag the "Home" page component receives. Each
// exported field (honoring its json tag) becomes a prop on the client.
type HomeProps struct {
	Message string ` + "`json:\"message\"`" + `
}

// NewHome renders the "/" page. It's an ordinary nexus handler — returning
// props instead of a JSON body. inertia.Page wraps the return into the
// Inertia page protocol: a JSON page object for XHR visits, a full HTML
// document for the initial load.
func NewHome(ctx context.Context) (HomeProps, error) {
	return HomeProps{Message: "Welcome to {{.Name}} — this page is server-rendered via Inertia."}, nil
}

var pagesModule = nexus.Module("pages",
	inertia.Page("GET", "/", "Home", NewHome),
)
`

// tmplVitelessInertiaEnvDTS extends the base ambient types with the
// Inertia adapter + import.meta.glob so the editor resolves a zero-install
// Inertia project.
const tmplVitelessInertiaEnvDTS = `// Ambient declarations so TypeScript resolves a viteless Inertia project's
// imports with nothing installed. viteless reads viteless.config.ts itself.

declare module "viteless" {
  export function defineConfig<T>(config: T): T
}

declare module "*.vue" {
  import type { DefineComponent } from "vue"
  const component: DefineComponent<{}, {}, any>
  export default component
}

declare module "@inertiajs/vue3" {
  export const createInertiaApp: any
  export const Link: any
  export const router: any
  export function usePage<T = any>(): { props: T }
  export function useForm<T = any>(data?: T): any
}

interface ImportMeta {
  glob: (pattern: string, opts?: { eager?: boolean }) => Record<string, any>
}
`

// tmplViteInertiaPackageJSON is the npm-managed Inertia project manifest —
// Vue + the Inertia adapter.
const tmplViteInertiaPackageJSON = `{
  "name": "{{.Name}}-web",
  "private": true,
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "vite build",
    "preview": "vite preview"
  },
  "dependencies": {
    "@inertiajs/vue3": "^1.2.0",
    "vue": "^3.5.0"
  },
  "devDependencies": {
    "@vitejs/plugin-vue": "^5.2.0",
    "vite": "^5.4.0"
  }
}
`

// ── inertia SSR entry files (web/src) ───────────────────────────────

// tmplInertiaSSRMainTS is the SSR client entry. Unlike the plain Inertia
// entry it builds the app with createSSRApp and mounts it, which HYDRATES
// the server-rendered markup the Go shell placed in the root div (vs
// re-rendering from scratch) — the only difference from tmplInertiaMainTS.
const tmplInertiaSSRMainTS = `import { createInertiaApp } from '@inertiajs/vue3'
import { createSSRApp, h } from 'vue'

createInertiaApp({
  resolve: (name) => {
    const pages = import.meta.glob('./Pages/**/*.vue', { eager: true })
    return pages['./Pages/' + name + '.vue']
  },
  // createSSRApp (not createApp) so mount() hydrates the server-rendered
  // DOM instead of discarding and re-rendering it.
  setup({ el, App, props, plugin }) {
    createSSRApp({ render: () => h(App, props) }).use(plugin).mount(el)
  },
})
`

// tmplInertiaSSRTS is the Node SSR bundle entry. createServer (from
// @inertiajs/server) starts an HTTP server (default :13714) that, per page
// object POSTed to it, renders the Vue app to a string and returns the
// {head, body} the Go engine injects. Built with "vite build --ssr" into
// web/dist/ssr/ssr.js and run with "node web/dist/ssr/ssr.js".
const tmplInertiaSSRTS = `import { createInertiaApp } from '@inertiajs/vue3'
import createServer from '@inertiajs/server'
import { renderToString } from '@vue/server-renderer'
import { createSSRApp, h } from 'vue'

createServer((page) =>
  createInertiaApp({
    page,
    render: renderToString,
    resolve: (name) => {
      const pages = import.meta.glob('./Pages/**/*.vue', { eager: true })
      return pages['./Pages/' + name + '.vue']
    },
    setup({ App, props, plugin }) {
      return createSSRApp({ render: () => h(App, props) }).use(plugin)
    },
  }),
)
`

// tmplViteInertiaSSRPackageJSON is the SSR project manifest: the build
// produces BOTH the client bundle (vite build) and the SSR bundle
// (vite build --ssr → dist/ssr/ssr.js). @inertiajs/server provides
// createServer; @vue/server-renderer provides renderToString.
const tmplViteInertiaSSRPackageJSON = `{
  "name": "{{.Name}}-web",
  "private": true,
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "vite build && vite build --ssr src/ssr.ts --outDir dist/ssr",
    "preview": "vite preview",
    "ssr": "node dist/ssr/ssr.js"
  },
  "dependencies": {
    "@inertiajs/server": "^1.2.0",
    "@inertiajs/vue3": "^1.2.0",
    "@vue/server-renderer": "^3.5.0",
    "vue": "^3.5.0"
  },
  "devDependencies": {
    "@vitejs/plugin-vue": "^5.2.0",
    "vite": "^5.4.0"
  }
}
`

const tmplReadmeTpl = `# {{.Name}}

Generated with ` + "`nexus new`" + `.

## Run (single process)

` + "```" + `
go mod tidy
{{if .HasFrontend}}cd web && npm install && cd ..   # one-time: install frontend deps
{{end -}}
{{if .HasResources}}cp .env.example .env    # then fill in real credentials
{{end -}}
nexus dev
` + "```" + `

Then open http://localhost:8080/__nexus/ for the dashboard, and:

` + "```" + `
curl 'http://localhost:8080/hello?name=Paul'
` + "```" + `
{{if .HasFrontend}}
## Frontend

A standard **Vite** project under ` + "`web/`" + ` ({{if .IsReact}}React{{else}}Vue{{end}} + TypeScript).
Manage frontend deps with ` + "`npm`" + ` as usual — Tailwind, component
libraries, any Vite plugin all work. The build output (` + "`web/dist`" + `) is
embedded into the Go binary via ` + "`//go:embed`" + ` in main.go.

  - Install deps: ` + "`cd web && npm install`" + ` (add libs with ` + "`npm install <pkg>`" + `).
  - Edit ` + "`web/src/App.{{if .IsReact}}tsx{{else}}vue{{end}}`" + ` — ` + "`nexus dev`" + ` runs the Vite dev
    server with HMR on http://localhost:5173 and proxies ` + "`/__nexus`" + `,
    ` + "`/graphql`" + `, ` + "`/oauth`" + `, ` + "`/ws`" + ` to the Go app.
  - Production build is part of ` + "`nexus build`" + ` (it runs ` + "`npm run build`" + ` then
    embeds ` + "`web/dist`" + `):

` + "```" + `
nexus build
./bin/{{.Name}}
` + "```" + `
{{end}}
{{- if .HasDB}}
## Database

The ` + "`resources/database.go`" + ` module declares a {{.DB}} connection.
Wire it into a service by depending on ` + "`*resources.DB`" + ` in your
constructor — fx will inject it.
{{end}}
{{- if .HasCache}}
## Cache

` + "`resources/cache.go`" + ` provides a Redis-backed cache with an
in-memory fallback. The fallback engages automatically when Redis
is unreachable, so dev environments without Redis still boot.
{{end}}
## Build

` + "```" + `
nexus build -o ./bin/{{.Name}}
./bin/{{.Name}}
` + "```" + `

Runtime settings (server address, dashboard, introspection) live in
` + "`nexus.toml`" + ` — edit that file, not the code.
`

// validChoice checks a value against the allowed set; returned err
// includes the full set so users see the menu in the message.
func validChoice(value, label string, choices []string) error {
	if slices.Contains(choices, value) {
		return nil
	}
	return fmt.Errorf("%s %q is not one of: %s", label, value, strings.Join(choices, ", "))
}

// ── react entry files ───────────────────────────────────────────────

const tmplMainTSXTpl = `import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'

ReactDOM.createRoot(document.getElementById('app') as HTMLElement).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
)
`

const tmplAppTSXTpl = `import { useState } from 'react'

export default function App() {
  const [count, setCount] = useState(0)
  return (
    <main style={ { fontFamily: 'system-ui, sans-serif', padding: '2rem', maxWidth: '40rem' } }>
      <h1>{{.Name}}</h1>
      <p>Edit <code>src/App.tsx</code> — HMR replaces the module in &lt;100ms.</p>
      <button onClick={() => setCount((n) => n + 1)} style={ { padding: '.5rem 1rem' } }>
        count is {count}
      </button>
    </main>
  )
}
`

// ── auth scaffold ───────────────────────────────────────────────────

// tmplAuthGoTpl wires the framework's oauth2.Module with stubs for
// the parts that need user-specific code: an Authenticator (verify
// username + password against your user store) and an
// IdentityResolver (return token-claim metadata for an
// authenticated user). Both stubs return placeholder data so the
// app boots — replace them with real implementations as soon as
// you wire your user table.
const tmplAuthGoTpl = `// Package auth wires nexus's built-in oauth2 server. It exposes a
// /oauth/token endpoint that accepts grant_type=password and emits
// JWT access + refresh tokens.
//
// Replace StubAuthenticator with a real credential check against
// your user store before shipping. Until then the server accepts
// {username:"admin", password:"admin"} and returns user id "1" —
// fine for the first dashboard click-through, dangerous in any
// other context.
package auth

import (
	"context"

	"github.com/paulmanoni/nexus/extension/oauth2"
)

// Default dev client. Real apps swap NewStaticClientStore for
// oauth2.NewLoaderClientStore so client provisioning becomes a
// runtime operation instead of a code change.
const (
	defaultClientID     = "{{.Name}}-web"
	defaultClientSecret = "change-me-in-prod"
)

// Module wires the oauth2 server. nexus.Run picks it up via main.go.
// IdentityResolver is left at its default (echoes the userID from
// the password grant); add one to Config when you need richer JWT
// claims (roles, scopes, extra payload).
var Module = oauth2.Module(oauth2.Config{
	ClientStore: oauth2.NewStaticClientStore(
		oauth2.StaticClient{
			ID:     defaultClientID,
			Secret: defaultClientSecret,
			Domain: "*",
		},
	),
	Authenticator: StubAuthenticator,
})

// StubAuthenticator accepts admin/admin only. The clientID arg lets
// you scope credentials per OAuth2 client when you need it; the stub
// ignores it. Return oauth2.ErrInvalidCredentials (or any error) to
// fail the password grant.
func StubAuthenticator(ctx context.Context, clientID, username, password string) (string, error) {
	if username == "admin" && password == "admin" {
		return "1", nil
	}
	return "", oauth2.ErrInvalidCredentials
}
`

// ── nexus.toml (runtime config) ─────────────────────────────────────

// tmplDeployTOML is the starter config. main.go loads it via
// nexus.Boot(); operators edit settings here instead of in code.
// Fields absent from the TOML fall back to framework defaults.
const tmplDeployTOML = `# nexus.toml — runtime config for this app.
#
# nexus.Boot() in main.go loads this file automatically: the [runtime]
# table, any [extensions.*] blocks, and the nexus.Get value store. Edit
# settings here, not in code; absent fields fall back to framework
# defaults. Every runtime key lives UNDER [runtime] (or a [runtime.<sub>]
# table). Read any value in code with nexus.Get[T]("section.key").

[runtime]
environment = "development"

# Introspection opens the /__nexus dashboard + JSON APIs. It's OFF by
# default (the surface 404s) so a production binary is locked down out
# of the box; "true" here makes the dashboard reachable in dev. Before
# shipping, set this false and expose the dashboard to operators via an
# admin CIDR instead — introspection_networks = ["10.0.0.0/8"].
introspection = true

# sdk = true generates + serves the typed client SDK (REST + GraphQL +
# WebSocket) and, when a frontend dir is present, dumps the SDK files +
# wires tsconfig so the nexus-client import resolves with types — no other
# wiring needed. Active only under "nexus dev" OR when introspection is on,
# so a locked-down production binary never exposes the API surface from it.
# sdk = true

[runtime.server]
addr = ":8080"
{{if .IsInertia}}
# Inertia dev topology is AUTO-DETECTED (this app imports the inertia
# extension): "nexus dev" serves pages from the app port (the browser lives
# there) and points the app's HTML shell at the viteless dev server for HMR —
# the inverse of the SPA dev model. Uncomment to force it on/off (e.g. a hybrid
# SPA + Inertia app that wants the SPA dev model):
# [runtime.inertia]
# enabled = true
{{end}}
[runtime.dashboard]
enabled = true
name = "{{.Name}}"

# Web security. The three safe response headers (X-Frame-Options,
# X-Content-Type-Options, Referrer-Policy) are ON by default even without
# this block — it only tunes them or turns on the extras. See
# "nexus docs security".
[runtime.middleware.security]
# headers = false                 # turn the default security headers off
# csp     = "default-src 'self'"  # opt-in Content-Security-Policy
# hsts_max_age = 31536000         # opt-in HSTS (seconds) — set once you serve https
{{if .IsInertia}}
# CSRF is ON for this app: Inertia posts from cookie/session-backed forms,
# which is exactly what double-submit CSRF protects. The generated client
# already sends the csrftoken cookie back as X-CSRFToken.
csrf = true
{{else}}
# CSRF is OFF by default: a token-authenticated API (bearer / the typed
# client SDK) isn't CSRF-vulnerable. Turn it on if you serve cookie/session-
# authenticated, server-rendered HTML forms (a template engine).
# csrf = true
{{end}}
# Databases live at the TOP level (not under [runtime]); wire each with
# db.BindFromConfig[YourType]("name") in code.
# [databases.main]
# driver   = "postgres"
# host     = "localhost"
# port     = "5432"
# user     = "postgres"
# password = "${DB_PASSWORD}"   # ${ENV} is expanded at load
# name     = "{{.Name}}"
# sslmode  = "disable"

# Config server (optional) — read secrets/flags via nexus.Get[T]("key").
# [extensions.config]
# endpoint = "http://localhost:8078"
# identity = "{{.Name}}"
# profile  = "default"
`
