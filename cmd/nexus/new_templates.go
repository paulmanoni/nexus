package main

import (
	"bytes"
	"fmt"
	"maps"
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
	DB         string // "none" | "postgres" | "mysql" | "sqlite"
	Cache      string // "none" | "redis"
	Auth       string // "none" | "oauth2"
	Inertia    bool   // Inertia.js server-driven pages (Vue) on top of the frontend
	SSR        bool   // Inertia server-side rendering (implies Inertia)
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

// NpmName is Name as a valid npm package name: lowercase, with anything
// outside [a-z0-9._-] replaced by "-" (npm refuses a package.json whose
// name has capitals or spaces).
func (o scaffoldOpts) NpmName() string {
	var b strings.Builder
	for _, r := range strings.ToLower(o.Name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.TrimLeft(b.String(), "._-")
	if name == "" {
		return "app"
	}
	return name
}

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
		web, err := renderFrontendOnly(opts)
		if err != nil {
			return nil, err
		}
		maps.Copy(out, web)
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
// scaffolder prints, in the order a fresh user runs them.
func nextStepsLines(opts scaffoldOpts) []string {
	lines := []string{
		"  cd " + opts.Dir,
		"  go mod tidy",
	}
	if opts.HasResources() {
		lines = append(lines,
			"  cp .env.example .env    # then fill in real credentials",
		)
	}
	if !opts.HasFrontend() {
		return append(lines, "  nexus dev               # rebuilds on save; dashboard at /__nexus/")
	}
	lines = append(lines,
		"  nexus dev               # first run installs web/ deps (npm, Node 20+); rebuilds on save",
		"                          # open the URL it prints — the app's own origin, not Vite's port",
		"  nexus build             # vite build → web/dist, embedded in one Go binary",
	)
	if opts.IsInertiaSSR() {
		lines = append(lines,
			"",
			"  # Server-side rendering: nexus build also writes web/dist/ssr/ssr.js;",
			"  # run it next to the app in production (Inertia's SSR server, :13714).",
			"  node web/dist/ssr/ssr.js",
			"  # Without it — and under nexus dev — pages render client-side.",
		)
	}
	return lines
}

// ── Vite frontend (web/) ────────────────────────────────────────────
//
// The frontend is an ordinary npm-managed Vite project. nexus-vite-plugin
// (web/sdk/nexus-vite-plugin.js, written at scaffold time and refreshed by
// nexus dev / nexus build before Vite starts) is the handshake with the Go
// app: under `vite dev` it writes dist/.vite/nexus-hot.json so the app's
// pages load modules from the dev server, and under `vite build` it forces
// the manifest the app reads. The browser always opens the Go app, so the
// config has no proxy block.

// tmplPackageJSON is web/package.json. The ranges are ones verified to
// install and build together (typescript stays on 6.0: vue-tsc 3.3 does
// not run on TypeScript 7).
const tmplPackageJSON = `{
  "name": "{{.NpmName}}-web",
  "private": true,
  "type": "module",
  "scripts": {
    "dev": "vite",
{{- if .IsInertiaSSR}}
    "build": "vite build && vite build --ssr src/ssr.ts --outDir dist/ssr",
    "ssr": "node dist/ssr/ssr.js",
{{- else}}
    "build": "vite build",
{{- end}}
    "typecheck": "{{if .IsReact}}tsc{{else}}vue-tsc{{end}} --noEmit"
  },
  "dependencies": {
{{- if .IsReact}}
    "react": "^19.3.0",
    "react-dom": "^19.3.0"
{{- else}}
{{- if .IsInertia}}
    "@inertiajs/vue3": "^2.3.0",
{{- end}}
    "vue": "^3.5.0"
{{- end}}
  },
  "devDependencies": {
{{- if .IsReact}}
    "@types/react": "^19.3.0",
    "@types/react-dom": "^19.3.0",
    "@vitejs/plugin-react": "^5.2.0",
    "typescript": "~6.0.3",
    "vite": "^6.4.3"
{{- else}}
    "@vitejs/plugin-vue": "^5.2.4",
    "typescript": "~6.0.3",
    "vite": "^6.4.3",
    "vue-tsc": "^3.3.11"
{{- end}}
  }
}
`

// tmplViteConfig is web/vite.config.ts.
const tmplViteConfig = `import { defineConfig } from 'vite'
{{if .IsReact}}import react from '@vitejs/plugin-react'{{else}}import vue from '@vitejs/plugin-vue'{{end}}
import nexus from './sdk/nexus-vite-plugin.js'

// nexus() connects Vite to the Go app: under "vite dev" it tells the app
// where the dev server is (dist/.vite/nexus-hot.json), under "vite build"
// it writes the manifest the app reads. Open the app's own URL, never
// Vite's — the app serves the pages, Vite only serves modules — so there
// is no proxy block. nexus dev / nexus build keep ./sdk up to date.
export default defineConfig({
  plugins: [{{if .IsReact}}react(){{else}}vue(){{end}}, nexus()],
  resolve: {
    alias: { '@': '/src' },
  },
{{- if .IsInertiaSSR}}
  // Bundle every dependency into dist/ssr/ssr.js, so the SSR server runs
  // with plain "node" wherever the binary is deployed — no node_modules.
  ssr: {
    noExternal: true,
  },
{{- end}}
})
`

// tmplViteTSConfig is web/tsconfig.json. No baseUrl (TypeScript 6
// deprecates it; paths resolve against this file). nexus dev adds the
// 'nexus-client' mapping and the SDK's client.d.ts when it first writes
// web/sdk — not seeded here, because those files don't exist until the
// app has run once and the scaffold imports nothing from them.
const tmplViteTSConfig = `{
  "compilerOptions": {
    "target": "ES2022",
    "module": "ESNext",
    "moduleResolution": "bundler",
    "lib": ["ES2022", "DOM", "DOM.Iterable"],
    "types": ["vite/client"],
    "jsx": "{{if .IsReact}}react-jsx{{else}}preserve{{end}}",
    "strict": true,
    "isolatedModules": true,
    "skipLibCheck": true,
    "resolveJsonModule": true,
    "noEmit": true,
    "paths": { "@/*": ["./src/*"] }
  },
  "include": ["src"]
}
`

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

// tmplViteDistStub is a minimal valid page committed at web/dist/index.html
// so the first `go build` (before any frontend build) compiles the
// //go:embed all:web/dist directive and the binary boots. `vite build`
// replaces it.
const tmplViteDistStub = `<!DOCTYPE html>
<html lang="en">
  <head><meta charset="UTF-8" /><title>{{.Name}}</title></head>
  <body>
    <div id="app"></div>
    <p style="font-family:system-ui;padding:2rem">Frontend not built yet — run
    <code>nexus build</code>, or <code>nexus dev</code> for the dev server.</p>
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
// inertia.Page (e.g. "Home" → src/Pages/Home.vue). With SSR the client
// hydrates the server-rendered markup (createSSRApp) and mounts fresh
// when there is none — under nexus dev, or with the SSR server down.
const tmplInertiaMainTS = `import { createInertiaApp } from '@inertiajs/vue3'
import { createApp, {{if .IsInertiaSSR}}createSSRApp, {{end}}h, type DefineComponent } from 'vue'

createInertiaApp({
  resolve: (name) => {
    const pages = import.meta.glob<DefineComponent>('./Pages/**/*.vue', { eager: true, import: 'default' })
    return pages[` + "`./Pages/${name}.vue`" + `]
  },
  setup({ el, App, props, plugin }) {
{{- if .IsInertiaSSR}}
    const create = el.hasChildNodes() ? createSSRApp : createApp
    create({ render: () => h(App, props) }).use(plugin).mount(el)
{{- else}}
    createApp({ render: () => h(App, props) }).use(plugin).mount(el)
{{- end}}
  },
})
`

// tmplInertiaSSRTS is the SSR bundle entry. createServer starts Inertia's
// SSR server (default :13714); for each page object the Go engine POSTs to
// it, it renders the app to a string and returns the {head, body} the
// engine puts into index.html. nexus build compiles it to
// web/dist/ssr/ssr.js (vite build --ssr).
const tmplInertiaSSRTS = `import { createInertiaApp } from '@inertiajs/vue3'
import createServer from '@inertiajs/vue3/server'
import { renderToString } from 'vue/server-renderer'
import { createSSRApp, h, type DefineComponent } from 'vue'

createServer((page) =>
  createInertiaApp({
    page,
    render: renderToString,
    resolve: (name) => {
      const pages = import.meta.glob<DefineComponent>('./Pages/**/*.vue', { eager: true, import: 'default' })
      return pages[` + "`./Pages/${name}.vue`" + `]
    },
    setup({ App, props, plugin }) {
      return createSSRApp({ render: () => h(App, props) }).use(plugin)
    },
  }),
)
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
    <code>NewHome</code> in <code>pages.go</code>; <code>nexus dev</code> hot-reloads on save.</p>
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

// ── react entry files (web/src) ─────────────────────────────────────

const tmplMainTSXTpl = `import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import App from './App'

createRoot(document.getElementById('app')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
`

const tmplAppTSXTpl = `import { useState } from 'react'

export default function App() {
  const [count, setCount] = useState(0)
  return (
    <main style={ { fontFamily: 'system-ui, sans-serif', padding: '2rem', maxWidth: '40rem' } }>
      <h1>{{.Name}}</h1>
      <p>Edit <code>web/src/App.tsx</code> — <code>nexus dev</code> hot-reloads on save.</p>
      <button onClick={() => setCount((n) => n + 1)} style={ { padding: '.5rem 1rem' } }>
        count is {count}
      </button>
    </main>
  )
}
`

// ── templates ───────────────────────────────────────────────────────

const tmplGoMod2 = `module {{.ModulePath}}

go 1.26
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
// webFS holds the built frontend (web/dist). nexus build runs vite build
// before go build, so this embed bakes the current bundle into the
// binary; a committed web/dist/index.html stub lets go build succeed
// before the first frontend build.
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
		// Inertia pages render into web/index.html — the bundle
		// ServeFrontend names (built in production, Vite's in dev). SSR
		// POSTs each first page load to the Node SSR server (nexus build
		// writes web/dist/ssr/ssr.js; run it with node, default :13714);
		// any renderer error falls back to client rendering, so a down SSR
		// server never takes a page down. Under nexus dev it isn't running.
		inertia.Module(inertia.Config{
			SSR: ssrhttp.New(""), // "" → http://127.0.0.1:13714
		}),
{{- else if .IsInertia}}
		// Inertia pages render into web/index.html — the bundle
		// ServeFrontend names (built in production, Vite's in dev).
		inertia.Module(inertia.Config{}),
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

// HelloService — typed wrapper around *nexus.Service so the DI
// container can route by type. Every handler that declares
// *HelloService as a dep grounds under the "hello" service on the
// dashboard's Architecture view.
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
# Frontend (web/). Dependencies and build output are not committed, except
# the web/dist/index.html stub: it lets a fresh clone's first go build
# satisfy //go:embed all:web/dist. web/sdk IS committed — vite.config.ts
# imports its nexus-vite-plugin.js, and pages import its generated types.
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
{{- if eq .DB "postgres"}}
	_ "github.com/paulmanoni/nexus/db/postgres" // links the pgx driver
{{- else if eq .DB "mysql"}}
	_ "github.com/paulmanoni/nexus/db/mysql" // links the MySQL driver
{{- else}}
	_ "github.com/paulmanoni/nexus/db/sqlite" // links the pure-Go SQLite engine
{{- end}}
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

const tmplReadmeTpl = `# {{.Name}}

Generated with ` + "`nexus new`" + `.

## Run

` + "```" + `
go mod tidy
{{if .HasResources}}cp .env.example .env    # then fill in real credentials
{{end -}}
nexus dev
` + "```" + `
{{if .HasFrontend}}
` + "`nexus dev`" + ` installs the frontend's dependencies on its first run (` + "`npm`" + ` —
Node.js 20 or later must be on PATH), starts Vite next to the Go app, and
prints the URL to open: the app's own origin, http://localhost:8080. Vite only
serves modules to that page; don't open its port.
{{end}}
The dashboard is at http://localhost:8080/__nexus/, and:

` + "```" + `
curl 'http://localhost:8080/hello?name=Paul'
` + "```" + `
{{if .HasFrontend}}
## Frontend

A {{if .IsReact}}React{{else}}Vue{{end}}{{if .IsInertia}} + Inertia{{end}} + TypeScript project under ` + "`web/`" + ` — an ordinary Vite
project (` + "`package.json`" + `, ` + "`vite.config.ts`" + `), so any Vite plugin or npm
library works: ` + "`cd web && npm install <pkg>`" + `.

  - ` + "`nexus()`" + ` in ` + "`vite.config.ts`" + ` connects Vite and the Go app. Under
    ` + "`vite dev`" + ` it tells the app where the dev server is, so pages on the app's
    origin load modules from it with HMR; under ` + "`vite build`" + ` it writes the
    manifest the app reads. ` + "`npm run dev`" + ` in ` + "`web/`" + ` plus ` + "`go run .`" + ` works as
    well as ` + "`nexus dev`" + `.
{{- if .IsInertia}}
  - Pages live in ` + "`web/src/Pages`" + `; ` + "`inertia.Page(\"GET\", \"/\", \"Home\", NewHome)`" + `
    in ` + "`pages.go`" + ` renders ` + "`Pages/Home.vue`" + ` with ` + "`NewHome`" + `'s return value as props,
    into ` + "`web/index.html`" + `. Once ` + "`nexus dev`" + ` has written ` + "`web/sdk`" + `, a page can take
    its props type from Go:
    ` + "`defineProps<NexusPageProps['Home']>()`" + ` with
    ` + "`import type { NexusPageProps } from 'nexus-client'`" + `.
{{- end}}
  - ` + "`web/sdk`" + ` holds ` + "`nexus-vite-plugin.js`" + ` (imported by ` + "`vite.config.ts`" + `) and
    the typed client SDK ` + "`nexus dev`" + ` generates. Commit it, and
    ` + "`web/package-lock.json`" + `, so a fresh checkout builds.
  - ` + "`npm run typecheck`" + ` in ` + "`web/`" + ` type-checks the frontend.
  - ` + "`nexus build`" + ` runs ` + "`vite build`" + `{{if .IsInertiaSSR}} (and the SSR build){{end}} into ` + "`web/dist`" + `, then ` + "`go build`" + `
    embeds it (` + "`//go:embed all:web/dist`" + ` in main.go). The binary needs no
    Node at run time{{if .IsInertiaSSR}} — except the SSR server below{{end}}.
{{if .IsInertiaSSR}}
### Server-side rendering

` + "`nexus build`" + ` also writes ` + "`web/dist/ssr/ssr.js`" + `, a self-contained Node server
(Inertia's, port 13714). Run it next to the app in production:

` + "```" + `
node web/dist/ssr/ssr.js
` + "```" + `

The app POSTs each first page load to it and falls back to client rendering
when it isn't running — which is what happens under ` + "`nexus dev`" + `.
{{end}}
{{- end}}
{{- if .HasDB}}
## Database

The ` + "`resources/database.go`" + ` module declares a {{.DB}} connection.
Wire it into a service by depending on ` + "`*resources.DB`" + ` in your
constructor — the DI container will inject it.
{{end}}
{{- if .HasCache}}
## Cache

` + "`resources/cache.go`" + ` provides a Redis-backed cache with an
in-memory fallback. The fallback engages automatically when Redis
is unreachable, so dev environments without Redis still boot.
{{end}}
## Build and deploy

` + "```" + `
nexus build -o ./bin/{{.Name}}
NEXUS_ENVIRONMENT=production ./bin/{{.Name}}
` + "```" + `

Runtime settings (server address, dashboard, introspection) live in
` + "`nexus.toml`" + ` — edit that file, not the code. It says
` + "`environment = \"development\"`" + `; set ` + "`NEXUS_ENVIRONMENT=production`" + ` wherever
the app is deployed, which overrides it.
`

// validChoice checks a value against the allowed set; returned err
// includes the full set so users see the menu in the message.
func validChoice(value, label string, choices []string) error {
	if slices.Contains(choices, value) {
		return nil
	}
	return fmt.Errorf("%s %q is not one of: %s", label, value, strings.Join(choices, ", "))
}

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
# "development" turns on dev-only behaviour for a plain "go run ." (nexus
# dev implies it): pages follow a running Vite dev server, and with sdk =
# true the typed client SDK is written into web/sdk. Deployments set
# NEXUS_ENVIRONMENT=production, which overrides this value — no edit here.
environment = "development"

# Introspection opens the /__nexus dashboard + JSON APIs. It's OFF by
# default (the surface 404s) so a production binary is locked down out
# of the box; "true" here makes the dashboard reachable in dev. Before
# shipping, set this false and expose the dashboard to operators via an
# admin CIDR instead — introspection_networks = ["10.0.0.0/8"].
introspection = true

# The typed client SDK (REST + GraphQL + WebSocket, import 'nexus-client')
# is generated into web/sdk under "nexus dev" with no setting. sdk = true
# also serves it from the binary at /__nexus/client/ — in production too,
# independent of introspection. See "nexus docs client".
# sdk = true

[runtime.server]
addr = ":8080"

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
