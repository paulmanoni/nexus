package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInspectFrontend(t *testing.T) {
	vite := t.TempDir()
	writeTestFile(t, filepath.Join(vite, "package.json"), "{}")
	if p := inspectFrontend(vite); !p.PackageJSON || p.Legacy != "" {
		t.Errorf("vite project: %+v", p)
	}
	legacy := t.TempDir()
	writeTestFile(t, filepath.Join(legacy, "viteless.config.ts"), "")
	p := inspectFrontend(legacy)
	if p.PackageJSON || p.Legacy != "viteless.config.ts" || !strings.Contains(p.legacyHint(), "package.json") {
		t.Errorf("viteless project: %+v %q", p, p.legacyHint())
	}
	if p := inspectFrontend(t.TempDir()); p.PackageJSON || p.Legacy != "" {
		t.Errorf("empty dir: %+v", p)
	}

	// A viteless project that had a package.json is still one while it
	// has a viteless config and no Vite config.
	withPkg := t.TempDir()
	writeTestFile(t, filepath.Join(withPkg, "package.json"), `{"dependencies":{"vue":"^3.5.0"}}`)
	writeTestFile(t, filepath.Join(withPkg, "viteless.config.ts"), "")
	p = inspectFrontend(withPkg)
	if !p.PackageJSON || p.Legacy != "viteless.config.ts" || !strings.Contains(p.legacyHint(), "no vite.config") || !strings.Contains(p.legacyHint(), ".orig") {
		t.Errorf("viteless project with package.json: %+v %q", p, p.legacyHint())
	}
	writeTestFile(t, filepath.Join(withPkg, "vite.config.mts"), "")
	if p := inspectFrontend(withPkg); p.Legacy != "" {
		t.Errorf("a Vite config makes it a Vite project, leftover viteless config or not: %+v", p)
	}
	stale := t.TempDir()
	writeTestFile(t, filepath.Join(stale, "package.json"), "{}")
	writeTestFile(t, filepath.Join(stale, "viteless-env.d.ts"), "")
	if p := inspectFrontend(stale); p.Legacy != "" {
		t.Errorf("stale viteless-env.d.ts beside a package.json is not a viteless project: %+v", p)
	}
}

func TestFrontendEnv(t *testing.T) {
	dir := t.TempDir()
	toml := filepath.Join(dir, "nexus.toml")
	writeTestFile(t, toml, "[env.client]\nid = \"web\"\n[env]\nflag = \"on\"\n")
	env, err := frontendEnv(toml, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var payload string
	for _, kv := range env {
		if strings.HasPrefix(kv, frontendEnvVar+"=") {
			payload = strings.TrimPrefix(kv, frontendEnvVar+"=")
		}
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("payload %q: %v", payload, err)
	}
	if got["client.id"] != "web" || got["flag"] != "on" {
		t.Errorf("payload = %v", got)
	}
	env, err = frontendEnv(filepath.Join(dir, "missing.toml"), io.Discard)
	if err != nil || !containsString(env, frontendEnvVar+"={}") {
		t.Errorf("no nexus.toml: err %v, want %s={}", err, frontendEnvVar)
	}
}

func TestWriteSDKPlugin(t *testing.T) {
	web := t.TempDir()
	if err := writeSDKPlugin(web, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"nexus-vite-plugin.js", "nexus-vite-plugin.d.ts"} {
		if fi, err := os.Stat(filepath.Join(web, "sdk", f)); err != nil || fi.Size() == 0 {
			t.Errorf("%s not written: %v", f, err)
		}
	}
}

func TestEnsureNodeModules_InstalledIsLeftAlone(t *testing.T) {
	web := t.TempDir()
	name := "vite"
	if runtime.GOOS == "windows" {
		name = "vite.cmd"
	}
	writeTestFile(t, filepath.Join(web, "node_modules", ".bin", name), "")
	t.Setenv("PATH", "") // npm would not be found: proves nothing ran
	if err := ensureNodeModules(context.Background(), web, io.Discard, io.Discard); err != nil {
		t.Fatalf("installed project: %v", err)
	}
	empty := t.TempDir()
	if err := ensureNodeModules(context.Background(), empty, io.Discard, io.Discard); err != errNoNpm {
		t.Errorf("missing deps without npm: got %v, want errNoNpm", err)
	}
}
