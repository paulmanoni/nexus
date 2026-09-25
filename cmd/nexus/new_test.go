package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/client"
)

// TestScaffoldAndBuild exercises the scaffolder end-to-end: we generate
// a fresh project into a temp dir, point it at the in-repo nexus via a
// replace directive, run `go mod tidy`, and `go build .` to prove the
// generated template compiles against the current framework. If this
// test breaks, the scaffold is drifting from the public API.
func TestScaffoldAndBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build test in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	_, here, _, _ := runtime.Caller(0)
	repoRoot, err := filepath.Abs(filepath.Join(filepath.Dir(here), "..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		t.Fatalf("expected go.mod at %s: %v", repoRoot, err)
	}

	dir := filepath.Join(t.TempDir(), "myapp")
	var stdout bytes.Buffer
	if err := scaffold(dir, "", &stdout); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	if !strings.Contains(stdout.String(), "Scaffolded") {
		t.Fatalf("expected Scaffolded message, got: %q", stdout.String())
	}
	for _, name := range []string{"go.mod", "main.go", "module.go", ".gitignore", "README.md", "nexus.toml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	// Sanity-check the manifest looks like a manifest, not an empty
	// stub — catches a future template that accidentally writes ""
	// past the test for file-existence.
	manifest, err := os.ReadFile(filepath.Join(dir, "nexus.toml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	for _, want := range []string{"[runtime]", "[runtime.server]", "[runtime.dashboard]", "introspection = true"} {
		if !strings.Contains(string(manifest), want) {
			t.Fatalf("manifest missing %q:\n%s", want, manifest)
		}
	}

	addReplace := exec.Command("go", "mod", "edit",
		"-replace", "github.com/paulmanoni/nexus="+repoRoot,
		"-require", "github.com/paulmanoni/nexus@v0.0.0",
	)
	addReplace.Dir = dir
	if out, err := addReplace.CombinedOutput(); err != nil {
		t.Fatalf("go mod edit: %v\n%s", err, out)
	}
	for _, step := range [][]string{
		{"go", "mod", "tidy"},
		{"go", "build", "."},
	} {
		cmd := exec.Command(step[0], step[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s failed: %v\n%s", strings.Join(step, " "), err, out)
		}
	}
}

// TestScaffold_Inertia_Builds scaffolds an Inertia app and compiles it
// against the in-repo nexus (which carries extension/inertia). Proves the
// generated Go wiring — inertia.Module, pagesModule, the page handler — is
// API-correct, and asserts the Inertia-specific file layout.
func TestScaffold_Inertia_Builds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build test in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	_, here, _, _ := runtime.Caller(0)
	repoRoot, _ := filepath.Abs(filepath.Join(filepath.Dir(here), "..", ".."))

	dir := filepath.Join(t.TempDir(), "inertiaapp")
	var stdout bytes.Buffer
	if err := scaffoldWithOpts(scaffoldOpts{
		Dir:      dir,
		Frontend: "vue",
		Inertia:  true,
		DB:       "none",
		Cache:    "none",
		Auth:     "none",
	}, &stdout); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	// Inertia-specific layout: page entry + Pages component + Go page
	// module; no App.vue (Inertia replaces the SPA root).
	for _, name := range []string{"pages.go", "web/src/main.ts", "web/src/Pages/Home.vue"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "web/src/App.vue")); err == nil {
		t.Fatalf("Inertia scaffold should not emit App.vue")
	}

	mainGo, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	for _, want := range []string{`extension/inertia`, "inertia.Module(", "pagesModule"} {
		if !strings.Contains(string(mainGo), want) {
			t.Fatalf("main.go missing %q:\n%s", want, mainGo)
		}
	}
	// The engine finds the bundle through ServeFrontend; naming it again in
	// inertia.Config would only be needed for a different source.
	if !strings.Contains(string(mainGo), "inertia.Module(inertia.Config{})") {
		t.Fatalf("main.go should pass an empty inertia.Config:\n%s", mainGo)
	}
	// The dev topology no longer depends on Inertia, so there is no
	// [runtime.inertia] override to advertise.
	toml, _ := os.ReadFile(filepath.Join(dir, "nexus.toml"))
	if strings.Contains(string(toml), "runtime.inertia") || strings.Contains(string(toml), "viteless") {
		t.Fatalf("nexus.toml still documents the old Inertia dev override:\n%s", toml)
	}
	mainTS, _ := os.ReadFile(filepath.Join(dir, "web/src/main.ts"))
	if !strings.Contains(string(mainTS), "import.meta.glob<DefineComponent>('./Pages/**/*.vue'") {
		t.Fatalf("main.ts should resolve pages through a typed glob:\n%s", mainTS)
	}

	addReplace := exec.Command("go", "mod", "edit",
		"-replace", "github.com/paulmanoni/nexus="+repoRoot,
		"-require", "github.com/paulmanoni/nexus@v0.0.0",
	)
	addReplace.Dir = dir
	if out, err := addReplace.CombinedOutput(); err != nil {
		t.Fatalf("go mod edit: %v\n%s", err, out)
	}
	for _, step := range [][]string{{"go", "mod", "tidy"}, {"go", "build", "."}} {
		cmd := exec.Command(step[0], step[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s failed: %v\n%s", strings.Join(step, " "), err, out)
		}
	}
}

// TestScaffold_InertiaSSR_Builds scaffolds an Inertia SSR app and compiles it
// against the in-repo nexus (which carries extension/inertia + ssrhttp). Proves
// the generated Go wiring (inertia.Module with SSR: ssrhttp.New) is API-correct
// and asserts the SSR-specific file layout (ssr.ts entry, hydrating main.ts, and
// the two-bundle build script).
func TestScaffold_InertiaSSR_Builds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build test in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	_, here, _, _ := runtime.Caller(0)
	repoRoot, _ := filepath.Abs(filepath.Join(filepath.Dir(here), "..", ".."))

	dir := filepath.Join(t.TempDir(), "ssrapp")
	var stdout bytes.Buffer
	if err := scaffoldWithOpts(scaffoldOpts{
		Dir:      dir,
		Frontend: "vue",
		Inertia:  true,
		SSR:      true,
		DB:       "none",
		Cache:    "none",
		Auth:     "none",
	}, &stdout); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	// SSR-specific layout: the Node SSR bundle entry plus the shared page
	// files. main.ts hydrates server-rendered markup (createSSRApp) and
	// mounts fresh when there is none.
	for _, name := range []string{"web/src/ssr.ts", "web/src/main.ts", "web/src/Pages/Home.vue", "web/package.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	mainTS, _ := os.ReadFile(filepath.Join(dir, "web/src/main.ts"))
	if !strings.Contains(string(mainTS), "createSSRApp") {
		t.Fatalf("SSR client entry must hydrate via createSSRApp:\n%s", mainTS)
	}
	ssrTS, _ := os.ReadFile(filepath.Join(dir, "web/src/ssr.ts"))
	for _, want := range []string{"'@inertiajs/vue3/server'", "createServer", "renderToString"} {
		if !strings.Contains(string(ssrTS), want) {
			t.Fatalf("ssr.ts missing %q:\n%s", want, ssrTS)
		}
	}
	pkg, _ := os.ReadFile(filepath.Join(dir, "web/package.json"))
	if !strings.Contains(string(pkg), "vite build --ssr src/ssr.ts --outDir dist/ssr") {
		t.Fatalf("package.json build script should run the SSR build:\n%s", pkg)
	}
	// @inertiajs/server is a dead package (its createServer now ships in
	// @inertiajs/vue3/server); vue/server-renderer comes with vue.
	for _, bad := range []string{"@inertiajs/server", "@vue/server-renderer"} {
		if strings.Contains(string(pkg), bad) {
			t.Fatalf("package.json should not depend on %s:\n%s", bad, pkg)
		}
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, "web/vite.config.ts"))
	if !strings.Contains(string(cfg), "noExternal: true") {
		t.Fatalf("vite.config.ts should bundle deps into the SSR build:\n%s", cfg)
	}

	mainGo, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	for _, want := range []string{"extension/inertia/ssrhttp", "SSR:", "ssrhttp.New("} {
		if !strings.Contains(string(mainGo), want) {
			t.Fatalf("main.go missing %q:\n%s", want, mainGo)
		}
	}

	addReplace := exec.Command("go", "mod", "edit",
		"-replace", "github.com/paulmanoni/nexus="+repoRoot,
		"-require", "github.com/paulmanoni/nexus@v0.0.0",
	)
	addReplace.Dir = dir
	if out, err := addReplace.CombinedOutput(); err != nil {
		t.Fatalf("go mod edit: %v\n%s", err, out)
	}
	for _, step := range [][]string{{"go", "mod", "tidy"}, {"go", "build", "."}} {
		cmd := exec.Command(step[0], step[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s failed: %v\n%s", strings.Join(step, " "), err, out)
		}
	}
}

// TestScaffold_SSR_Validation covers the --ssr guardrail: SSR is an
// Inertia feature. (The cobra command turns --inertia on for --ssr; the
// scaffolder itself refuses the inconsistent options.)
func TestScaffold_SSR_Validation(t *testing.T) {
	var out bytes.Buffer
	err := scaffoldWithOpts(scaffoldOpts{
		Dir: filepath.Join(t.TempDir(), "b"), Frontend: "vue",
		SSR: true, DB: "none", Cache: "none", Auth: "none",
	}, &out)
	if err == nil || !strings.Contains(err.Error(), "inertia") {
		t.Fatalf("--ssr without --inertia should be rejected; got %v", err)
	}
}

func TestScaffold_RejectsNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err := scaffold(dir, "", &stdout)
	if err == nil {
		t.Fatal("expected error for non-empty dir, got nil")
	}
	if !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("expected 'not empty' in error, got: %v", err)
	}
}

func TestScaffold_InvalidModulePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app")
	var stdout bytes.Buffer
	err := scaffold(dir, "has a space", &stdout)
	if err == nil {
		t.Fatal("expected error for bad module path, got nil")
	}
}

// TestScaffoldWithOpts_FullStack covers the maximum-options path:
// vue + postgres + redis + oauth2. We assert each axis dropped its
// expected files, the generated main.go imports the right
// packages, and the .env.example includes credentials for every
// chosen resource.
func TestScaffoldWithOpts_FullStack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "myapp")
	var stdout bytes.Buffer
	err := scaffoldWithOpts(scaffoldOpts{
		Dir:      dir,
		Frontend: "vue",
		DB:       "postgres",
		Cache:    "redis",
		Auth:     "oauth2",
	}, &stdout)
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	for _, name := range []string{
		"go.mod",
		"main.go",
		"module.go",
		"nexus.toml",
		"resources/database.go",
		"resources/cache.go",
		"auth/auth.go",
		"web/package.json",
		"web/vite.config.ts",
		"web/sdk/nexus-vite-plugin.js",
		"web/sdk/nexus-vite-plugin.d.ts",
		"web/tsconfig.json",
		"web/index.html",
		"web/src/main.ts",
		"web/src/App.vue",
		"web/dist/index.html",
		".env.example",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s, missing: %v", name, err)
		}
	}
	// And these should NOT exist anymore (islands- and viteless-era files):
	for _, name := range []string{
		"islands.src/main.ts",
		"islands/index.html",
		"nexus-shims.d.ts",
		"web/viteless.config.ts",
		"web/viteless-env.d.ts",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("legacy artifact %s still scaffolded — should be gone", name)
		}
	}
	mainGo, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	for _, want := range []string{
		`"embed"`,
		"//go:embed all:web/dist",
		"resources.NewDB",
		"resources.NewCacheManager",
		"auth.Module",
		"nexus.ServeFrontend(webFS",
	} {
		if !strings.Contains(string(mainGo), want) {
			t.Errorf("main.go missing %q\n--- body ---\n%s", want, mainGo)
		}
	}
	envExample, _ := os.ReadFile(filepath.Join(dir, ".env.example"))
	for _, want := range []string{"DB_HOST", "DB_PORT=5432", "REDIS_HOST"} {
		if !strings.Contains(string(envExample), want) {
			t.Errorf(".env.example missing %q\n--- body ---\n%s", want, envExample)
		}
	}
}

// TestScaffoldFullStack_Builds catches API drift between the
// auth/db/cache templates and the framework packages they import.
// The cheaper TestScaffoldWithOpts_FullStack only checks file
// contents — it would have missed the recent oauth2 signature
// change that broke `nexus new --auth=oauth2`. This one runs the
// full mod-tidy + go-build dance against an in-repo replace, so
// any template that references a renamed/removed symbol fails
// loudly here instead of in user inboxes.
func TestScaffoldFullStack_Builds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build test in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	_, here, _, _ := runtime.Caller(0)
	repoRoot, err := filepath.Abs(filepath.Join(filepath.Dir(here), "..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "fullstack")
	if err := scaffoldWithOpts(scaffoldOpts{
		Dir: dir, Frontend: "vue", DB: "postgres", Cache: "redis", Auth: "oauth2",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	addReplace := exec.Command("go", "mod", "edit",
		"-replace", "github.com/paulmanoni/nexus="+repoRoot,
		"-require", "github.com/paulmanoni/nexus@v0.0.0",
	)
	addReplace.Dir = dir
	if out, err := addReplace.CombinedOutput(); err != nil {
		t.Fatalf("go mod edit: %v\n%s", err, out)
	}
	for _, step := range [][]string{
		{"go", "mod", "tidy"},
		{"go", "build", "./..."},
	} {
		cmd := exec.Command(step[0], step[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s failed: %v\n%s", strings.Join(step, " "), err, out)
		}
	}
}

// TestScaffoldWithOpts_VueLayout asserts the Vite project shape for
// --frontend=vue: package.json with the verified version ranges, a
// vite.config.ts that loads nexus-vite-plugin from ./sdk (written at
// scaffold time, byte-identical to the embedded plugin) with no proxy
// block, and a tsconfig with neither baseUrl nor viteless types.
func TestScaffoldWithOpts_VueLayout(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wired")
	if err := scaffoldWithOpts(scaffoldOpts{
		Dir: dir, Frontend: "vue", DB: "none", Cache: "none", Auth: "none",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	for _, p := range []string{
		"web/package.json",
		"web/vite.config.ts",
		"web/tsconfig.json",
		"web/index.html",
		"web/src/main.ts",
		"web/src/App.vue",
		"web/sdk/nexus-vite-plugin.js",
		"web/sdk/nexus-vite-plugin.d.ts",
		"web/dist/index.html",
	} {
		info, err := os.Stat(filepath.Join(dir, p))
		if err != nil {
			t.Errorf("missing %s: %v", p, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("%s exists but is empty", p)
		}
	}
	html := readScaffoldFile(t, dir, "web/index.html")
	if !strings.Contains(html, `src="/src/main.ts"`) {
		t.Errorf("web/index.html should reference /src/main.ts\n--- body ---\n%s", html)
	}
	cfg := readScaffoldFile(t, dir, "web/vite.config.ts")
	for _, want := range []string{
		"import vue from '@vitejs/plugin-vue'",
		"import nexus from './sdk/nexus-vite-plugin.js'",
		"plugins: [vue(), nexus()]",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("vite.config.ts missing %q\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "proxy:") || strings.Contains(cfg, "localhost:8080") {
		t.Errorf("vite.config.ts must not proxy to the app — the browser is on the app's origin\n%s", cfg)
	}
	if got := readScaffoldFile(t, dir, "web/sdk/nexus-vite-plugin.js"); got != string(client.VitePluginJS()) {
		t.Error("web/sdk/nexus-vite-plugin.js differs from the embedded plugin")
	}
	if got := readScaffoldFile(t, dir, "web/sdk/nexus-vite-plugin.d.ts"); got != string(client.VitePluginDTS()) {
		t.Error("web/sdk/nexus-vite-plugin.d.ts differs from the embedded plugin types")
	}
	pkg := readPackageJSON(t, dir)
	for name, want := range map[string]string{
		"vite": "^6.4.3", "@vitejs/plugin-vue": "^5.2.4",
		"typescript": "~6.0.3", "vue-tsc": "^3.3.11",
	} {
		if got := pkg.DevDependencies[name]; got != want {
			t.Errorf("devDependencies[%s] = %q, want %q", name, got, want)
		}
	}
	if got := pkg.Dependencies["vue"]; got != "^3.5.0" {
		t.Errorf("dependencies[vue] = %q, want ^3.5.0", got)
	}
	if pkg.Scripts["build"] != "vite build" || pkg.Scripts["typecheck"] != "vue-tsc --noEmit" {
		t.Errorf("scripts = %v", pkg.Scripts)
	}
	ts := readScaffoldFile(t, dir, "web/tsconfig.json")
	for _, bad := range []string{"baseUrl", "viteless", "nexus-client"} {
		if strings.Contains(ts, bad) {
			t.Errorf("tsconfig.json should not mention %q\n%s", bad, ts)
		}
	}
	var tsDoc struct {
		CompilerOptions map[string]any `json:"compilerOptions"`
		Include         []string       `json:"include"`
	}
	if err := json.Unmarshal([]byte(ts), &tsDoc); err != nil {
		t.Fatalf("tsconfig.json is not JSON: %v", err)
	}
	if tsDoc.CompilerOptions["strict"] != true || !slices.Equal(tsDoc.Include, []string{"src"}) {
		t.Errorf("tsconfig should be strict and include src only: %s", ts)
	}
	mainTS := readScaffoldFile(t, dir, "web/src/main.ts")
	for _, want := range []string{"createApp", "App", "mount"} {
		if !strings.Contains(mainTS, want) {
			t.Errorf("web/src/main.ts missing %q", want)
		}
	}
}

// TestScaffoldWithOpts_ReactFrontend covers the react variant: .tsx
// sources, @vitejs/plugin-react, the React 19 types, tsc as the checker.
func TestScaffoldWithOpts_ReactFrontend(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ra")
	if err := scaffoldWithOpts(scaffoldOpts{
		Dir: dir, Frontend: "react", DB: "none", Cache: "none", Auth: "none",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	for _, p := range []string{
		"web/src/main.tsx",
		"web/src/App.tsx",
		"web/index.html",
		"web/package.json",
		"web/vite.config.ts",
		"web/sdk/nexus-vite-plugin.js",
	} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	if main := readScaffoldFile(t, dir, "web/src/main.tsx"); !strings.Contains(main, "from 'react-dom/client'") {
		t.Errorf("main.tsx should mount with react-dom/client\n%s", main)
	}
	if html := readScaffoldFile(t, dir, "web/index.html"); !strings.Contains(html, `src="/src/main.tsx"`) {
		t.Errorf("index.html should load /src/main.tsx\n%s", html)
	}
	cfg := readScaffoldFile(t, dir, "web/vite.config.ts")
	if !strings.Contains(cfg, "import react from '@vitejs/plugin-react'") || !strings.Contains(cfg, "plugins: [react(), nexus()]") {
		t.Errorf("vite.config.ts should use plugin-react and nexus()\n%s", cfg)
	}
	if ts := readScaffoldFile(t, dir, "web/tsconfig.json"); !strings.Contains(ts, `"jsx": "react-jsx"`) {
		t.Errorf("tsconfig should use the automatic JSX runtime\n%s", ts)
	}
	pkg := readPackageJSON(t, dir)
	for name, want := range map[string]string{
		"vite": "^6.4.3", "@vitejs/plugin-react": "^5.2.0", "typescript": "~6.0.3",
		"@types/react": "^19.3.0", "@types/react-dom": "^19.3.0",
	} {
		if got := pkg.DevDependencies[name]; got != want {
			t.Errorf("devDependencies[%s] = %q, want %q", name, got, want)
		}
	}
	if pkg.Dependencies["react"] != "^19.3.0" || pkg.Dependencies["react-dom"] != "^19.3.0" {
		t.Errorf("dependencies = %v", pkg.Dependencies)
	}
	if _, ok := pkg.DevDependencies["vue-tsc"]; ok {
		t.Error("react project should not depend on vue-tsc")
	}
	if pkg.Scripts["typecheck"] != "tsc --noEmit" {
		t.Errorf("scripts = %v", pkg.Scripts)
	}
}

// TestScaffold_FrontendVariants_ValidProject checks what every variant
// shares: package.json parses, the .gitignore keeps web/sdk (its plugin
// is what vite.config.ts imports on a fresh checkout), nexus.toml tells
// deployments about NEXUS_ENVIRONMENT, and no viteless text survives.
func TestScaffold_FrontendVariants_ValidProject(t *testing.T) {
	variants := map[string]scaffoldOpts{
		"vue":     {Frontend: "vue"},
		"react":   {Frontend: "react"},
		"inertia": {Frontend: "vue", Inertia: true},
		"ssr":     {Frontend: "vue", Inertia: true, SSR: true},
	}
	for name, opts := range variants {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "My App")
			opts.Dir, opts.ModulePath = dir, "example.com/myapp"
			opts.DB, opts.Cache, opts.Auth = "none", "none", "none"
			var out bytes.Buffer
			if err := scaffoldWithOpts(opts, &out); err != nil {
				t.Fatalf("scaffold: %v", err)
			}
			pkg := readPackageJSON(t, dir)
			if pkg.Name != "my-app-web" || !pkg.Private || pkg.Type != "module" {
				t.Errorf("package.json name/private/type = %q/%v/%q", pkg.Name, pkg.Private, pkg.Type)
			}
			if pkg.Scripts["dev"] != "vite" {
				t.Errorf("scripts.dev = %q", pkg.Scripts["dev"])
			}
			gi := readScaffoldFile(t, dir, ".gitignore")
			for _, want := range []string{"/web/node_modules/", "/web/dist/*", "!/web/dist/index.html"} {
				if !strings.Contains(gi, want) {
					t.Errorf(".gitignore missing %q\n%s", want, gi)
				}
			}
			if strings.Contains(gi, "/web/sdk") {
				t.Errorf(".gitignore must not ignore web/sdk\n%s", gi)
			}
			toml := readScaffoldFile(t, dir, "nexus.toml")
			if !strings.Contains(toml, `environment = "development"`) || !strings.Contains(toml, "NEXUS_ENVIRONMENT=production") {
				t.Errorf("nexus.toml should keep development and point deployments at NEXUS_ENVIRONMENT\n%s", toml)
			}
			next := out.String()
			if !strings.Contains(next, "nexus dev") || strings.Contains(next, "5173") || strings.Contains(next, "npm install") {
				t.Errorf("next steps should say nexus dev installs deps and not send users to Vite's port:\n%s", next)
			}
			files, err := buildFiles(opts)
			if err != nil {
				t.Fatal(err)
			}
			for path, body := range files {
				if strings.HasPrefix(path, "web/sdk/") {
					continue // the plugin's own text is not the scaffold's
				}
				if strings.Contains(strings.ToLower(body), "viteless") {
					t.Errorf("%s still mentions viteless", path)
				}
			}
		})
	}
}

// TestNpmName covers the package.json name derived from the directory.
func TestNpmName(t *testing.T) {
	for in, want := range map[string]string{
		"myapp": "myapp", "My App": "my-app", "Shop_2": "shop_2", "_x": "x", "...": "app", "café": "caf-",
	} {
		if got := (scaffoldOpts{Name: in}).NpmName(); got != want {
			t.Errorf("NpmName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestScaffoldWithOpts_RejectsBadAxis catches typos / casing
// mismatches early — better than letting `go build` fail with a
// confusing template-rendered import.
func TestScaffoldWithOpts_RejectsBadAxis(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x")
	err := scaffoldWithOpts(scaffoldOpts{
		Dir: dir, Frontend: "Vue", DB: "none", Cache: "none", Auth: "none",
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error for non-canonical frontend value, got nil")
	}
}

// TestPromptMissing_TakesNumericChoices simulates a user picking
// "2) postgres" via the prompt. Confirms numeric input maps to
// the right axis value.
func TestPromptMissing_TakesNumericChoices(t *testing.T) {
	stdin := bytes.NewBufferString("\n2\n2\n2\n") // frontend default, db=postgres, cache=redis, auth=oauth2
	var stdout bytes.Buffer
	opts := scaffoldOpts{}
	if err := promptMissing(&opts, stdin, &stdout); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if opts.Frontend != "none" {
		t.Errorf("frontend: got %q want none", opts.Frontend)
	}
	if opts.DB != "postgres" {
		t.Errorf("db: got %q want postgres", opts.DB)
	}
	if opts.Cache != "redis" {
		t.Errorf("cache: got %q want redis", opts.Cache)
	}
	if opts.Auth != "oauth2" {
		t.Errorf("auth: got %q want oauth2", opts.Auth)
	}
}

// TestPromptMissing_TakesNamedChoices verifies users can type
// "vue" / "sqlite" / "redis" / "oauth2" instead of the index.
func TestPromptMissing_TakesNamedChoices(t *testing.T) {
	stdin := bytes.NewBufferString("vue\nsqlite\nredis\noauth2\n")
	var stdout bytes.Buffer
	opts := scaffoldOpts{}
	if err := promptMissing(&opts, stdin, &stdout); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if opts.Frontend != "vue" || opts.DB != "sqlite" || opts.Cache != "redis" || opts.Auth != "oauth2" {
		t.Errorf("got %+v", opts)
	}
}

// TestCobra_VersionCommand asserts the cobra wiring routes the
// `version` subcommand to its handler — guards against accidental
// reorganization of the command tree dropping the brand line.
func TestCobra_VersionCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := newRootCmd(&stdout, &stderr)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v (stderr=%s)", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "nexus") {
		t.Fatalf("version output missing brand: %q", stdout.String())
	}
}

// TestCobra_UnknownCommand confirms cobra surfaces an error for typos.
// This covers the same contract the old TestRun_Unknown test did.
func TestCobra_UnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := newRootCmd(&stdout, &stderr)
	root.SetArgs([]string{"whatever"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for unknown command")
	}
}

// TestNewCmd_ToolingDeprecated keeps old scripts working: --tooling is
// accepted, reported as deprecated, and ignored — the scaffold is the
// Vite project whatever value it carries.
func TestNewCmd_ToolingDeprecated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vt")
	var stdout, stderr bytes.Buffer
	root := newRootCmd(&stdout, &stderr)
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"new", dir, "--frontend", "vue", "--tooling", "viteless", "--yes"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v (stderr=%s)", err, stderr.String())
	}
	if !strings.Contains(stdout.String()+stderr.String(), "deprecated") {
		t.Errorf("--tooling should be reported as deprecated; out=%q err=%q", stdout.String(), stderr.String())
	}
	for _, p := range []string{"web/vite.config.ts", "web/package.json"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "web/viteless.config.ts")); err == nil {
		t.Error("--tooling viteless must not bring back viteless.config.ts")
	}
}

type scaffoldPackage struct {
	Name            string            `json:"name"`
	Private         bool              `json:"private"`
	Type            string            `json:"type"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

func readPackageJSON(t *testing.T, dir string) scaffoldPackage {
	t.Helper()
	var pkg scaffoldPackage
	if err := json.Unmarshal([]byte(readScaffoldFile(t, dir, "web/package.json")), &pkg); err != nil {
		t.Fatalf("web/package.json is not valid JSON: %v", err)
	}
	return pkg
}

func readScaffoldFile(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}
