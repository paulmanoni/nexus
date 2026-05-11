package main

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

	"github.com/paulmanoni/nexus/client"
)

// stubManifestJSON is the placeholder dropped at web/sdk/manifest.json
// during scaffold so vite.config.ts's `import './sdk/manifest.json'`
// resolves before the user has run `nexus dev` for the first time.
// nexus dev overwrites this with the real projection on its first
// boot — the file is in web/.gitignore so the stub never reaches
// version control.
const stubManifestJSON = `{
  "version": "client.v1",
  "basePath": "",
  "endpoints": [],
  "refs": {},
  "auth": {}
}
`

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
	// addRaw writes bytes verbatim — no template substitution. Used
	// for embedded JS payloads (the SDK runtime, the vite plugin)
	// where `{{` and `${` already mean something in the source and
	// we don't want text/template touching them.
	addRaw := func(path string, body []byte) {
		out[path] = string(body)
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
	if err := add("nexus.deploy.yaml", tmplDeployYaml); err != nil {
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
		// Files shared by every frontend choice — config, html shell,
		// gitignore, dist stub. The framework-specific source lives
		// under src/ and varies per choice (App.vue vs App.tsx).
		if err := add("web/vite.config.ts", tmplViteConfigTS); err != nil {
			return nil, err
		}
		if err := add("web/index.html", tmplIndexHTMLTpl); err != nil {
			return nil, err
		}
		if err := add("web/tsconfig.json", tmplTSConfig); err != nil {
			return nil, err
		}
		if err := add("web/.gitignore", tmplWebGitignore); err != nil {
			return nil, err
		}
		if err := add("web/dist/index.html", tmplStubDistHTMLTpl); err != nil {
			return nil, err
		}
		// SDK assets — vite.config.ts imports ./sdk/nexus-vite-plugin.js
		// and source files import ./sdk/client.js (or vue.js). Drop the
		// embedded copies here so a fresh checkout can run `vite dev`
		// before the user has ever invoked `nexus dev`. Once `nexus
		// dev` runs, it overwrites these with the live projection;
		// either way the consumer never has a missing-file error.
		// All four entries sit under web/sdk/ which the generated
		// .gitignore excludes — they're build artifacts, not source.
		addRaw("web/sdk/nexus-vite-plugin.js", client.VitePluginJS())
		addRaw("web/sdk/client.js", client.RuntimeJS())
		if opts.IsVue() {
			addRaw("web/sdk/vue.js", client.VueJS())
		}
		out["web/sdk/manifest.json"] = stubManifestJSON
		switch opts.Frontend {
		case "vue":
			if err := add("web/package.json", tmplPackageJSONVueTpl); err != nil {
				return nil, err
			}
			if err := add("web/src/main.ts", tmplMainTS); err != nil {
				return nil, err
			}
			if err := add("web/src/App.vue", tmplAppVueTpl); err != nil {
				return nil, err
			}
		case "react":
			if err := add("web/package.json", tmplPackageJSONReactTpl); err != nil {
				return nil, err
			}
			if err := add("web/src/main.tsx", tmplMainTSXTpl); err != nil {
				return nil, err
			}
			if err := add("web/src/App.tsx", tmplAppTSXTpl); err != nil {
				return nil, err
			}
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
	if opts.HasFrontend() {
		lines = append(lines,
			"  (cd web && npm install)",
		)
	}
	if opts.HasResources() {
		lines = append(lines,
			"  cp .env.example .env   # then fill in real credentials",
		)
	}
	lines = append(lines,
		"  nexus dev               # API on :8080, dashboard at /__nexus/",
	)
	if opts.HasFrontend() {
		lines = append(lines,
			"                          # SPA opens at vite's port (HMR)",
		)
	}
	return lines
}

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
{{- if .HasFrontend}}
	"github.com/paulmanoni/nexus/client"
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
//go:embed all:web/dist
var distFS embed.FS
{{end}}
func main() {
	nexus.Run(
		nexus.Config{
			Server:    nexus.ServerConfig{Addr: ":8080"},
			Dashboard: nexus.DashboardConfig{Enabled: true, Name: "{{.Name}}"},
{{- if .HasFrontend}}
			// Mount auto-detects ./web/{vite.config.ts,tsconfig.json}
			// and writes SDK files to ./web/sdk by default. Override
			// any field below to escape the convention.
			Client: client.Config{Enabled: true},
{{- end}}
		},
{{- if .HasFrontend}}
		nexus.ServeFrontend(distFS, "web/dist"),
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
# frontend artifacts
/web/node_modules/
/web/dist/
/web/sdk/
/web/auto-imports.d.ts
/web/components.d.ts
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

// ── frontend (vue) ──────────────────────────────────────────────────

// tmplPackageJSONVueTpl ships the minimum dep set vite needs to
// dev-server + build a vue 3 + ts project. Versions are loose
// (^range) so npm install picks the latest patch automatically.
const tmplPackageJSONVueTpl = `{
  "name": "{{.Name}}-web",
  "private": true,
  "version": "0.0.1",
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "vite build",
    "preview": "vite preview"
  },
  "dependencies": {
    "vue": "^3.4.0"
  },
  "devDependencies": {
    "@vitejs/plugin-vue": "^5.0.0",
    "typescript": "^5.0.0",
    "vite": "^5.0.0",
    "vue-tsc": "^2.0.0"
  }
}
`

const tmplPackageJSONReactTpl = `{
  "name": "{{.Name}}-web",
  "private": true,
  "version": "0.0.1",
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "vite build",
    "preview": "vite preview"
  },
  "dependencies": {
    "react": "^18.2.0",
    "react-dom": "^18.2.0"
  },
  "devDependencies": {
    "@types/react": "^18.2.0",
    "@types/react-dom": "^18.2.0",
    "@vitejs/plugin-react": "^4.2.0",
    "typescript": "^5.0.0",
    "vite": "^5.0.0"
  }
}
`

// tmplViteConfigTS is parameterized over .Frontend so the import +
// plugins entry switches between vue() and react() while the
// proxy / build sections stay shared. The nexus plugin is always
// wired so the auto-select rewriter + loop-guard run on every
// build; the manifest filter is opt-in via filter: 'usage'.
const tmplViteConfigTS = `import { defineConfig } from 'vite'
{{if .IsVue -}}
import vue from '@vitejs/plugin-vue'
{{- else if .IsReact -}}
import react from '@vitejs/plugin-react'
{{- end}}
import nexus from './sdk/nexus-vite-plugin.js'

// Proxy entries forward the framework's reserved paths back to Go
// during dev. nexus dev keeps these in sync automatically; they're
// pre-seeded here so a one-shot ` + "`npm run dev`" + ` works without nexus
// dev too.
export default defineConfig({
  plugins: [
    {{if .IsVue}}vue(),{{else if .IsReact}}react(),{{end}}
    // The nexus plugin bundles three behaviors:
    //   1. nexus-auto-select  — rewrites nx.query/mutate at build to
    //      fetch only the fields the surrounding code reads.
    //   2. nexus-manifest-filter — opt-in. Set filter: 'usage' to
    //      project the bundled SDK manifest down to the endpoints
    //      the app actually calls (production hardening — leaks
    //      less schema in the JS bundle). 'off' (default) ships
    //      the full manifest, which keeps autocomplete + ad-hoc
    //      experimentation friction-free during early development.
    //   3. nexus-loop-guard   — prevents unplugin-auto-import from
    //      thrashing rebuilds in nexus dev.
    nexus({
      filter: 'off',
      // filterMode: 'loose', // 'loose' (default) | 'strict'
    }),
  ],
  server: {
    proxy: {
      '/__nexus': { target: 'http://localhost:8080', changeOrigin: true },
      '/graphql': { target: 'http://localhost:8080', changeOrigin: true },
      '/oauth':   { target: 'http://localhost:8080', changeOrigin: true },
      '/ws':      { target: 'ws://localhost:8080', ws: true, changeOrigin: true },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
})
`

// tmplIndexHTMLTpl branches the entry-point script between
// main.ts (vue) and main.tsx (react) — vite cares about the
// extension to pick the right transform.
const tmplIndexHTMLTpl = `<!DOCTYPE html>
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
    <p>Edit <code>src/App.vue</code> — HMR replaces the module in &lt;100ms.</p>
    <button @click="count++">count is {{ "{{ count }}" }}</button>
  </main>
</template>

<style scoped>
main { font-family: system-ui, sans-serif; padding: 2rem; max-width: 40rem; }
button { padding: .5rem 1rem; border-radius: .25rem; cursor: pointer; }
</style>
`

const tmplTSConfig = `{
  "compilerOptions": {
    "target": "ES2020",
    "module": "ESNext",
    "moduleResolution": "bundler",
    "strict": true,
    "jsx": "preserve",
    "skipLibCheck": true,
    "isolatedModules": true,
    "esModuleInterop": true,
    "resolveJsonModule": true,
    "lib": ["ES2020", "DOM", "DOM.Iterable"]
  },
  "include": ["src/**/*", "src/**/*.vue", "env.d.ts"]
}
`

const tmplWebGitignore = `node_modules/
dist/
sdk/
auto-imports.d.ts
components.d.ts
`

// tmplStubDistHTMLTpl ships a placeholder index.html so
// nexus.ServeFrontend(distFS, "web/dist")'s boot-time existence
// check succeeds before the user has run ` + "`npm run build`" + ` once. The
// stub points the user at the correct setup step.
const tmplStubDistHTMLTpl = `<!DOCTYPE html>
<html lang="en">
  <head><meta charset="UTF-8"><title>{{.Name}} — not built yet</title></head>
  <body>
    <main style="font-family: system-ui; padding: 2rem; max-width: 40rem">
      <h1>Frontend not built yet</h1>
      <p>Run <code>cd web &amp;&amp; npm install &amp;&amp; npm run build</code>
         to produce the production bundle, or use <code>nexus dev</code>
         which serves the SPA via vite's dev server with HMR.</p>
    </main>
  </body>
</html>
`

const tmplReadmeTpl = `# {{.Name}}

Generated with ` + "`nexus new`" + `.

## Run (single process)

` + "```" + `
go mod tidy
{{if .HasFrontend}}(cd web && npm install)
{{end -}}
{{if .HasResources}}cp .env.example .env   # then fill in real credentials
{{end -}}
nexus dev
` + "```" + `

Then open http://localhost:8080/__nexus/ for the dashboard, and:

` + "```" + `
curl 'http://localhost:8080/hello?name=Paul'
` + "```" + `
{{if .HasFrontend}}
The SPA lives in ` + "`web/`" + `. ` + "`nexus dev`" + ` boots vite's dev server with HMR;
edits in ` + "`web/src/`" + ` show up in <100ms with component state preserved.
For a one-shot bundle (the embed.FS path):

` + "```" + `
cd web && npm run build
nexus build --deployment monolith
./bin/monolith
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
## Build a deployable binary

` + "```" + `
nexus build --deployment monolith
./bin/monolith
` + "```" + `

## Split into microservices

Edit ` + "`nexus.deploy.yaml`" + ` to declare additional deployments and tag
your modules with ` + "`nexus.DeployAs(\"...\")`" + `. The manifest comments
walk through each step. Then:

` + "```" + `
nexus dev --split           # all units in one terminal
nexus build --deployment orders-svc
` + "```" + `

Application code stays unchanged — the framework swaps cross-module
*Service struct bodies between the local impl and HTTP-stub shadows
at compile time, based on the active deployment.
`

// validChoice checks a value against the allowed set; returned err
// includes the full set so users see the menu in the message.
func validChoice(value, label string, choices []string) error {
	for _, c := range choices {
		if value == c {
			return nil
		}
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

// ── deployment manifest ─────────────────────────────────────────────

// tmplDeployYaml is the starter manifest. It declares a single
// monolith deployment and embeds a hand-walkthrough showing how to
// split modules into independent services. The user edits this file
// (not main.go) when topology changes.
const tmplDeployYaml = `# nexus.deploy.yaml — deployment topology for this app.
#
# 'nexus build --deployment NAME' reads this file to decide which
# modules compile locally and which become HTTP-stub shadows.
# 'nexus dev --split' reads it to launch one subprocess per split
# unit. Application code (main.go, modules) stays
# deployment-agnostic; everything per-environment lives here.
#
# ── Concepts ──────────────────────────────────────────────────────
#
# deployments:    map of unit name → { owns: [...], port: N }
#                 Empty 'owns' = "owns every module" (the monolith).
#                 Listed 'owns' = real split unit; modules NOT
#                 listed get replaced by HTTP-stub shadows in this
#                 unit's binary.
# peers:          map of DeployAs-tag → transport config (URL,
#                 timeout, retries, min_version, auth). Codegen bakes
#                 this into the binary as Config.Topology defaults.

deployments:
  # Monolith owns every module by default. Run with:
  #     nexus build --deployment monolith
  #     ./bin/monolith
  monolith:
    port: 8080

# ── How to split a module out ─────────────────────────────────────
#
# 1. Tag the module's declaration with DeployAs:
#
#        var Module = nexus.Module("orders",
#            nexus.DeployAs("orders-svc"),  // names the deployment unit
#            nexus.Provide(NewService),
#            nexus.AsRest("GET", "/orders/:id", NewGet),
#        )
#
# 2. Add a deployment for it here, and add it to monolith's owns
#    list (or leave monolith empty so it auto-includes everything):
#
#        deployments:
#          monolith:
#            port: 8080
#          orders-svc:
#            owns: [orders]
#            port: 8081
#
# 3. Add a peer entry so other services can reach it.
#
#        peers:
#          orders-svc:
#            timeout: 2s
#
# 4. Build (or run) per deployment:
#
#        nexus build --deployment orders-svc   # ./bin/orders-svc
#        nexus build --deployment monolith     # ./bin/monolith
#        nexus dev --split                     # all units, one terminal
`