package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	if _, err := os.Stat(filepath.Join(repoRoot, "nexus.go")); err != nil {
		t.Fatalf("expected nexus.go at %s: %v", repoRoot, err)
	}

	dir := filepath.Join(t.TempDir(), "myapp")
	var stdout bytes.Buffer
	if err := scaffold(dir, "", &stdout); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	if !strings.Contains(stdout.String(), "Scaffolded") {
		t.Fatalf("expected Scaffolded message, got: %q", stdout.String())
	}
	for _, name := range []string{"go.mod", "main.go", "module.go", ".gitignore", "README.md", "nexus.deploy.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	// Sanity-check the manifest looks like a manifest, not an empty
	// stub — catches a future template that accidentally writes ""
	// past the test for file-existence.
	manifest, err := os.ReadFile(filepath.Join(dir, "nexus.deploy.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	for _, want := range []string{"deployments:", "monolith:", "port: 8080"} {
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
		"nexus.deploy.yaml",
		"resources/database.go",
		"resources/cache.go",
		"auth/auth.go",
		"web/package.json",
		"web/vite.config.ts",
		"web/index.html",
		"web/src/main.ts",
		"web/src/App.vue",
		"web/tsconfig.json",
		"web/dist/index.html",
		".env.example",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s, missing: %v", name, err)
		}
	}
	mainGo, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	for _, want := range []string{
		`"embed"`,
		"//go:embed all:web/dist",
		`"github.com/paulmanoni/nexus/client"`,
		"resources.NewDB",
		"resources.NewCacheManager",
		"auth.Module",
		"nexus.ServeFrontend(distFS",
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

// TestScaffoldWithOpts_ReactFrontend covers the react-specific
// branch: package.json carries react deps, src/main.tsx is the
// entry point, and vite.config.ts plugs in @vitejs/plugin-react.
func TestScaffoldWithOpts_ReactFrontend(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ra")
	if err := scaffoldWithOpts(scaffoldOpts{
		Dir: dir, Frontend: "react", DB: "none", Cache: "none", Auth: "none",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	for _, p := range []string{"web/src/main.tsx", "web/src/App.tsx", "web/package.json", "web/vite.config.ts"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	pkg, _ := os.ReadFile(filepath.Join(dir, "web/package.json"))
	for _, want := range []string{`"react":`, `"@vitejs/plugin-react":`} {
		if !strings.Contains(string(pkg), want) {
			t.Errorf("package.json missing %q\n--- body ---\n%s", want, pkg)
		}
	}
	vite, _ := os.ReadFile(filepath.Join(dir, "web/vite.config.ts"))
	if !strings.Contains(string(vite), "@vitejs/plugin-react") {
		t.Errorf("vite.config.ts missing react plugin import\n%s", vite)
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