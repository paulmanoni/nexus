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
		// Vite project under web/. Standard Node toolchain: npm-managed deps
		// (package.json + package-lock), vite dev/build, real node_modules.
		// `nexus dev` runs `npm run dev` (Vite + HMR) and injects the proxy;
		// `nexus build` runs `npm run build` → web/dist, embedded via
		// //go:embed all:web/dist in main.go. A committed web/dist/index.html
		// stub makes the first `go build` (before any vite build) compile.
		if err := add("web/index.html", tmplViteIndexHTML); err != nil {
			return nil, err
		}
		if err := add("web/vite.config.ts", tmplViteConfig); err != nil {
			return nil, err
		}
		if err := add("web/tsconfig.json", tmplViteTSConfig); err != nil {
			return nil, err
		}
		if err := add("web/dist/index.html", tmplViteDistStub); err != nil {
			return nil, err
		}
		switch opts.Frontend {
		case "vue":
			if err := add("web/package.json", tmplViteVuePackageJSON); err != nil {
				return nil, err
			}
			if err := add("web/src/main.ts", tmplMainTS); err != nil {
				return nil, err
			}
			if err := add("web/src/App.vue", tmplAppVueTpl); err != nil {
				return nil, err
			}
		case "react":
			if err := add("web/package.json", tmplViteReactPackageJSON); err != nil {
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
		// Vite project under web/ — install JS deps with npm (one-time).
		lines = append(lines,
			"  cd web && npm install   # install frontend deps (vite + framework)",
			"  cd ..",
		)
	}
	if opts.HasResources() {
		lines = append(lines,
			"  cp .env.example .env    # then fill in real credentials",
		)
	}
	lines = append(lines,
		"  nexus dev               # go run + Vite dev server (HMR); dashboard at /__nexus/",
	)
	if opts.HasFrontend() {
		lines = append(lines,
			"                          # SPA on http://localhost:5173 ; nexus build embeds web/dist",
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
	// Runtime config (server addr, dashboard, introspection, environment, …)
	// is loaded from nexus.toml — edit that file to change settings without
	// touching code. Fields absent from the TOML fall back to framework
	// defaults. MustLoadExtensions picks up any [extensions.*] blocks.
	cfg := nexus.MustLoadConfig()
	opts := nexus.MustLoadExtensions()

	opts = append(opts,
{{- if .HasFrontend}}
		nexus.ServeFrontend(webFS, "web/dist"),
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

	nexus.Run(cfg, opts...)
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

// ── frontend (vue) ──────────────────────────────────────────────────

// tmplPackageJSONNexusVue is the minimal package.json the nexus
// pipeline emits for Vue projects. NO scripts (no npm runner), NO
// devDependencies (no vite/tsc) — every JS build step is handled
// by the Go-side bundler reading nexus.lock. The file exists
// because:
//   - IDEs (VS Code, JetBrains) detect "this is a JS project" via
//     package.json and only then load tsconfig.json / .d.ts shims
//   - Dependabot / Renovate scan package.json for outdated deps
//   - Humans want a one-glance answer to "what does this depend on?"
//     that doesn't require reading the machine-formatted nexus.lock
//
// The `dependencies` field is the source of truth for what nexus
// should install. `nexus add <pkg>` mirrors new pins here; `nexus
// remove <pkg>` drops them. `nexus install` reads this list and
// reconciles against nexus.lock.
// ── Vite frontend (web/) ────────────────────────────────────────────

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
    "vue": "^3.4.0"
  },
  "devDependencies": {
    "@vitejs/plugin-vue": "^5.1.0",
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

// tmplViteConfig — base '/' (the SPA is served at the app root). `nexus dev`
// injects a managed proxy block into server.proxy (between @nexus:proxy
// markers) so /__nexus,/graphql,/oauth,/ws reach the Go app from :5173.
const tmplViteConfig = `import { defineConfig } from 'vite'
{{if .IsReact}}import react from '@vitejs/plugin-react'{{else}}import vue from '@vitejs/plugin-vue'{{end}}

export default defineConfig({
  plugins: [{{if .IsReact}}react(){{else}}vue(){{end}}],
  server: {
    // ` + "`nexus dev`" + ` injects the /__nexus,/graphql,/oauth,/ws proxy here.
    proxy: {},
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
})
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
    "lib": ["ES2022", "DOM", "DOM.Iterable"]
  },
  "include": ["src"]
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

const tmplPackageJSONNexusVue = `{
  "name": "{{.Name}}",
  "type": "module",
  "private": true,
  "dependencies": {
    "vue": "^3.4.0"
  }
}
`

// tmplPackageJSONNexusReact is the React counterpart. Same layout
// rationale as tmplPackageJSONNexusVue; only the dep set differs.
const tmplPackageJSONNexusReact = `{
  "name": "{{.Name}}",
  "type": "module",
  "private": true,
  "dependencies": {
    "react": "^18.2.0",
    "react-dom": "^18.2.0"
  }
}
`

// tmplIndexHTMLTpl is the SPA shell that ships at
// islands/index.html. Stable filenames from nexus build mean
// the script reference doesn't need post-build patching — main.js
// is always there, alongside the user-edited islands.src/main.ts.
//
// The user is free to add <link rel="stylesheet" href="/main.css">
// after their first import of a .css file from main.ts (esbuild
// emits a sidecar CSS bundle then).
const tmplIndexHTMLTpl = `<!DOCTYPE html>
<html lang="en">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>{{.Name}}</title>
  </head>
  <body>
    <div id="app"></div>
    <script type="module" src="/main.js"></script>
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
    <p>Edit <code>islands.src/App.vue</code> — <code>nexus dev</code> rebuilds on save.</p>
    <button @click="count++">count is {{ "{{ count }}" }}</button>
  </main>
</template>

<style scoped>
main { font-family: system-ui, sans-serif; padding: 2rem; max-width: 40rem; }
button { padding: .5rem 1rem; border-radius: .25rem; cursor: pointer; }
</style>
`

// tmplTSConfigForIDE is the IDE-only tsconfig the scaffold emits
// for nexus-managed frontend projects. esbuild bypasses it (our
// bundler infers loaders + targets independently); this exists
// purely so the TypeScript server in VS Code / cursor / etc.
// understands the project layout and stops emitting TS2307.
//
// skipLibCheck=true matters: nexus-shims.d.ts ships ambient
// `declare module 'foo'` lines that don't match the real
// package's type signatures. Without skipLibCheck the IDE would
// emit "duplicate identifier" errors when real .d.ts files
// eventually land alongside (the follow-up commit).
//
// jsx=preserve so .tsx files stay as JSX in the AST; esbuild's
// pipeline handles the actual JSX-to-JS lowering.
const tmplTSConfigForIDE = `{
  "compilerOptions": {
    "target": "ES2020",
    "module": "ESNext",
    "moduleResolution": "bundler",
    "strict": true,
    "jsx": "preserve",
    "skipLibCheck": true,
    "isolatedModules": true,
    "esModuleInterop": true,
    "allowSyntheticDefaultImports": true,
    "resolveJsonModule": true,
    "noEmit": true,
    "lib": ["ES2020", "DOM", "DOM.Iterable"]
  },
  "include": ["islands.src/**/*", "nexus-shims.d.ts"]
}
`

// tmplShimsDTS supplies ambient module declarations so the IDE
// can resolve every bare import in islands.src/ without a real
// node_modules tree.
//
// What's covered:
//   - Asset imports (.vue, .css, .svg, .png, .jpg, .webp, .woff,
//     .woff2) so things like 'import './style.css'' stop erroring
//   - The npm package set the scaffold expects (vue, react,
//     @vue-flow/core, lucide-vue-next, etc.) — ` + "`nexus add`" + ` appends
//     to this file as new deps are pinned
//
// These ambient `declare module 'foo';` lines (no signatures) just
// stop the IDE complaining; they don't give autocomplete on
// createApp / ref / etc. For REAL types, run `nexus types`: it
// fetches each dep's .d.ts from esm.sh into a types-only
// node_modules/ that the editor resolves against (the bundler still
// ignores it). These shims are the no-network fallback for before
// that runs.
//
// The file is meant to be committed; it's part of the project's
// IDE-only build chain. Re-run ` + "`nexus install`" + ` to regenerate from
// the lockfile if it gets out of sync.
const tmplShimsDTS = `// nexus-shims.d.ts — IDE-only ambient declarations.
// Generated by nexus; safe to commit and re-generate via ` + "`nexus install`" + `.
//
// Stops TypeScript from complaining about bare imports while
// nexus.lock + ~/.nexus/cache do the actual resolution at build
// time. For real types (full autocomplete), run ` + "`nexus types`" + ` —
// it mirrors each dep's .d.ts from esm.sh into a types-only
// node_modules/ the editor resolves against.

// Asset imports — esbuild handles these at build time via the
// resolver's content-type dispatch (CSS → injected, fonts →
// LoaderFile, etc.). The IDE just needs to know they're modules.
declare module "*.vue" {
  import type { DefineComponent } from "vue";
  const component: DefineComponent<{}, {}, any>;
  export default component;
}
declare module "*.css";
declare module "*.scss";
declare module "*.svg";
declare module "*.png";
declare module "*.jpg";
declare module "*.jpeg";
declare module "*.gif";
declare module "*.webp";
declare module "*.avif";
declare module "*.woff";
declare module "*.woff2";
declare module "*.ttf";
declare module "*.otf";

// Per-package bare-spec shims for the dependencies the scaffold
// pulls. ` + "`nexus add <pkg>`" + ` appends new entries here when the
// project grows.
{{- if .IsVue}}
declare module "vue";
{{- end}}
{{- if .IsReact}}
declare module "react";
declare module "react-dom";
declare module "react-dom/client";
{{- end}}
`

const tmplReadmeTpl = `# {{.Name}}

Generated with ` + "`nexus new`" + `.

## Run (single process)

` + "```" + `
go mod tidy
{{if .HasFrontend}}{{if .IsVue}}nexus add vue           # one-time: pulls vue into ~/.nexus/cache
{{else}}nexus add react react-dom
{{end -}}
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
// nexus.MustLoadConfig(); operators edit settings here instead of in
// code. Fields absent from the TOML fall back to framework defaults.
const tmplDeployTOML = `# nexus.toml — runtime config for this app.
#
# main.go loads the [runtime] table via nexus.MustLoadConfig() and any
# [extensions.*] blocks via nexus.MustLoadExtensions(). Edit settings here,
# not in code; absent fields fall back to framework defaults. Every runtime
# key lives UNDER [runtime] (or a [runtime.<sub>] table).

[runtime]
environment = "development"

# Introspection opens the /__nexus dashboard + JSON APIs. It's OFF by
# default (the surface 404s) so a production binary is locked down out
# of the box; "true" here makes the dashboard reachable in dev. Before
# shipping, set this false and expose the dashboard to operators via an
# admin CIDR instead — introspection_networks = ["10.0.0.0/8"].
introspection = true

[runtime.server]
addr = ":8080"

[runtime.dashboard]
enabled = true
name = "{{.Name}}"

# Databases live at the TOP level (not under [runtime]); wire each with
# nexus.DatabaseFromConfig[YourType]("name") in code.
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