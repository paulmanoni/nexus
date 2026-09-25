package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Fake Vite executables for frontendBuild. Each records its arguments and
// the NEXUS_FRONTEND_ENV it saw (cwd is the web dir), then behaves like
// the Vite step it stands in for.
const (
	fakeViteOK = `#!/bin/sh
echo "$*" >> calls.log
printf '%s' "$NEXUS_FRONTEND_ENV" > env.json
if [ "$2" = "--ssr" ]; then
  mkdir -p dist/ssr && echo 'export const render = 1' > dist/ssr/ssr.js
  exit 0
fi
rm -rf dist && mkdir -p dist/.vite && echo '{}' > dist/.vite/manifest.json
echo "vite: built"
`
	fakeViteFail = `#!/bin/sh
echo "$*" >> calls.log
echo "[vite]: Rollup failed to resolve import ./missing from src/main.ts" >&2
exit 1
`
	fakeViteNoOutput = `#!/bin/sh
echo "$*" >> calls.log
rm -rf dist
`
)

// fakeViteProject lays out <root>/web as a Vite project whose
// node_modules/.bin/vite is script.
func fakeViteProject(t *testing.T, script string) (root, web string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake vite is a shell script")
	}
	root = t.TempDir()
	web = filepath.Join(root, "web")
	writeTestFile(t, filepath.Join(web, "package.json"), `{"private":true}`)
	bin := filepath.Join(web, "node_modules", ".bin", "vite")
	writeTestFile(t, bin, script)
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, web
}

func viteCalls(t *testing.T, web string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(web, "calls.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestFrontendBuild_ViteProject(t *testing.T) {
	t.Setenv("NEXUS_FRONTEND_DIR", "")
	root, web := fakeViteProject(t, fakeViteOK)
	writeTestFile(t, filepath.Join(root, "nexus.toml"), "[env.client]\nid = \"web\"\n")

	var out, errOut bytes.Buffer
	if err := frontendBuild(context.Background(), root, &out, &errOut); err != nil {
		t.Fatalf("frontendBuild: %v\nstdout: %s\nstderr: %s", err, out.String(), errOut.String())
	}
	if got := viteCalls(t, web); len(got) != 1 || got[0] != "build" {
		t.Errorf("vite calls = %q, want one plain build (no src/ssr.ts)", got)
	}
	if !strings.Contains(out.String(), "vite: built") {
		t.Errorf("vite output not streamed: %q", out.String())
	}
	raw, err := os.ReadFile(filepath.Join(web, "env.json"))
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	if err := json.Unmarshal(raw, &env); err != nil || env["client.id"] != "web" {
		t.Errorf("NEXUS_FRONTEND_ENV = %s (%v), want client.id=web", raw, err)
	}
	for _, f := range []string{"nexus-vite-plugin.js", "nexus-vite-plugin.d.ts"} {
		if !fileExists(filepath.Join(web, "sdk", f)) {
			t.Errorf("sdk/%s not written before vite ran", f)
		}
	}
}

// A secret the app reads at boot is not the frontend build's business: an
// unset ${DB_PASSWORD} outside [env] must not fail the build, and an unset
// variable inside [env] drops that one key with a warning.
func TestFrontendBuild_UnsetSecretsDoNotFailTheBuild(t *testing.T) {
	t.Setenv("NEXUS_FRONTEND_DIR", "")
	t.Setenv("NEXUS_TEST_UNSET_DB_PASSWORD", "")
	os.Unsetenv("NEXUS_TEST_UNSET_DB_PASSWORD")
	t.Setenv("NEXUS_TEST_UNSET_CLIENT_SECRET", "")
	os.Unsetenv("NEXUS_TEST_UNSET_CLIENT_SECRET")
	root, web := fakeViteProject(t, fakeViteOK)
	writeTestFile(t, filepath.Join(root, "nexus.toml"), `[databases.main]
password = "${NEXUS_TEST_UNSET_DB_PASSWORD}"

[env.client]
id = "web"
secret = "${NEXUS_TEST_UNSET_CLIENT_SECRET}"
`)
	var out, errOut bytes.Buffer
	if err := frontendBuild(context.Background(), root, &out, &errOut); err != nil {
		t.Fatalf("frontendBuild: %v\nstderr: %s", err, errOut.String())
	}
	raw, err := os.ReadFile(filepath.Join(web, "env.json"))
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]string
	if err := json.Unmarshal(raw, &env); err != nil || env["client.id"] != "web" || len(env) != 1 {
		t.Errorf("NEXUS_FRONTEND_ENV = %s (%v), want only client.id=web", raw, err)
	}
	if w := errOut.String(); strings.Count(w, "left out of the frontend") != 1 ||
		!strings.Contains(w, "client.secret (nexus.toml:6)") || !strings.Contains(w, "${NEXUS_TEST_UNSET_CLIENT_SECRET}") {
		t.Errorf("want one warning naming client.secret, got %q", w)
	}
}

func TestFrontendBuild_SSR(t *testing.T) {
	t.Setenv("NEXUS_FRONTEND_DIR", "")
	root, web := fakeViteProject(t, fakeViteOK)
	writeTestFile(t, filepath.Join(web, "src", "ssr.ts"), "export {}")

	var out bytes.Buffer
	if err := frontendBuild(context.Background(), root, &out, &out); err != nil {
		t.Fatalf("frontendBuild: %v\n%s", err, out.String())
	}
	want := []string{"build", "build --ssr src/ssr.ts --outDir dist/ssr --emptyOutDir=false"}
	if got := viteCalls(t, web); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("vite calls = %q, want %q (client first, so its emptyOutDir can't remove dist/ssr)", got, want)
	}
	for _, f := range []string{"dist/.vite/manifest.json", "dist/ssr/ssr.js"} {
		if !fileExists(filepath.Join(web, f)) {
			t.Errorf("%s missing after the build", f)
		}
	}
}

func TestFrontendBuild_ViteFailureStops(t *testing.T) {
	t.Setenv("NEXUS_FRONTEND_DIR", "")
	root, web := fakeViteProject(t, fakeViteFail)
	writeTestFile(t, filepath.Join(web, "src", "ssr.ts"), "export {}")

	var out, errOut bytes.Buffer
	err := frontendBuild(context.Background(), root, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "vite build failed") {
		t.Fatalf("err = %v, want a vite build failure", err)
	}
	if !strings.Contains(errOut.String(), "failed to resolve import ./missing") {
		t.Errorf("Vite's error not shown: %q", errOut.String())
	}
	if got := viteCalls(t, web); len(got) != 1 {
		t.Errorf("vite calls = %q, want the SSR build skipped after a failure", got)
	}
}

func TestFrontendBuild_NoOutputIsAnError(t *testing.T) {
	t.Setenv("NEXUS_FRONTEND_DIR", "")
	root, _ := fakeViteProject(t, fakeViteNoOutput)
	err := frontendBuild(context.Background(), root, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), ".vite/manifest.json nor index.html") {
		t.Fatalf("err = %v, want the missing-output error", err)
	}
}

func TestFrontendBuild_FrontendDirOverride(t *testing.T) {
	root, web := fakeViteProject(t, fakeViteOK)
	client := filepath.Join(root, "client")
	if err := os.Rename(web, client); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NEXUS_FRONTEND_DIR", "client")
	if err := frontendBuild(context.Background(), root, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatalf("frontendBuild: %v", err)
	}
	if got := viteCalls(t, client); len(got) != 1 {
		t.Errorf("vite calls in client/ = %q, want one build", got)
	}
}

func TestFrontendBuild_Skips(t *testing.T) {
	t.Setenv("NEXUS_FRONTEND_DIR", "")
	// A pure-Go app: no web/ at all.
	if err := frontendBuild(context.Background(), t.TempDir(), &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Errorf("no frontend dir: %v", err)
	}
	// A static, hand-written dist with no package.json is embedded as is.
	root := t.TempDir()
	index := filepath.Join(root, "web", "dist", "index.html")
	writeTestFile(t, index, "<p>static</p>")
	writeTestFile(t, filepath.Join(root, "web", "src", "__nexus", "api.ts"), "")
	var out bytes.Buffer
	if err := frontendBuild(context.Background(), root, &out, &out); err != nil {
		t.Errorf("static dist: %v", err)
	}
	if b, _ := os.ReadFile(index); string(b) != "<p>static</p>" || out.Len() != 0 {
		t.Errorf("static dist touched: %q, output %q", b, out.String())
	}
}

func TestFrontendBuild_LegacyVitelessDirIsAnError(t *testing.T) {
	t.Setenv("NEXUS_FRONTEND_DIR", "")
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "web", "viteless.config.ts"), "export default {}")
	writeTestFile(t, filepath.Join(root, "web", "src", "main.ts"), "")
	err := frontendBuild(context.Background(), root, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "viteless.config.ts but no package.json") {
		t.Fatalf("err = %v, want the legacy hint", err)
	}
}

func TestFrontendBuild_NoNpm(t *testing.T) {
	t.Setenv("NEXUS_FRONTEND_DIR", "")
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "web", "package.json"), "{}")
	t.Setenv("PATH", t.TempDir())
	err := frontendBuild(context.Background(), root, &bytes.Buffer{}, &bytes.Buffer{})
	if !errors.Is(err, errNoNpm) {
		t.Fatalf("err = %v, want errNoNpm", err)
	}
}
