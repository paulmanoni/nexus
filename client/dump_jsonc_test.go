package client

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMergePathsConfig_JSONC verifies MergePathsConfig can read a tsconfig
// authored as JSONC — comments and trailing commas, which tsc accepts — and
// rewrites it as strict JSON with the SDK path mappings merged in.
func TestMergePathsConfig_JSONC(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "tsconfig.json")
	// Trailing commas (paths, compilerOptions, include) + a // comment +
	// a /* */ block comment — the exact shape that used to fail to parse.
	const jsonc = `{
  // project config
  "compilerOptions": {
    "baseUrl": ".",
    "paths": {
      "@/*": ["./src/*"], /* alias */
    },
    "strict": true,
  },
  "include": ["src/**/*.ts"],
}`
	if err := os.WriteFile(cfg, []byte(jsonc), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "sdk")

	if err := MergePathsConfig(cfg, outDir, io.Discard); err != nil {
		t.Fatalf("MergePathsConfig on JSONC: %v", err)
	}

	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Output must be strict JSON now (no comments/trailing commas).
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("rewritten tsconfig is not strict JSON: %v\n%s", err, body)
	}
	co, _ := doc["compilerOptions"].(map[string]any)
	paths, _ := co["paths"].(map[string]any)
	if paths["@/*"] == nil {
		t.Error("existing @/* alias should be preserved")
	}
	// The SDK URL mappings should have been merged in.
	if paths["/__nexus/client/client.js"] == nil {
		t.Errorf("SDK path mapping not merged; got paths=%v", paths)
	}
}

// TestStripJSONC_PreservesStrings ensures comment/comma markers inside string
// literals are left untouched.
func TestStripJSONC_PreservesStrings(t *testing.T) {
	in := `{"url": "http://x//y", "note": "a, }", "arr": [1, 2,]}`
	var doc map[string]any
	if err := json.Unmarshal(stripJSONC([]byte(in)), &doc); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if doc["url"] != "http://x//y" {
		t.Errorf("url mangled: %v", doc["url"])
	}
	if doc["note"] != "a, }" {
		t.Errorf("note mangled: %v", doc["note"])
	}
}

// Mapped paths are relative to an existing baseUrl, not to the config
// file: with baseUrl "src", web/sdk is "../sdk" from where TypeScript
// resolves them.
func TestMergePathsConfig_RelativeToBaseURL(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "web", "tsconfig.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{"compilerOptions":{"baseUrl":"src"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MergePathsConfig(cfg, filepath.Join(dir, "web", "sdk"), io.Discard); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfg)
	for _, want := range []string{`"baseUrl": "src"`, `"nexus-client": [`, `"../sdk/client.d.ts"`, `"../sdk/client.js"`, `"../sdk/vue.js"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in\n%s", want, b)
		}
	}
}

// An existing include gains the SDK's declaration files (once), so the
// inertia.d.ts augmentation is in the program; a config without include
// (TypeScript's default: everything) is left without one.
func TestMergePathsConfig_IncludesSDKDeclarations(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "web", "tsconfig.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`{"include":["src/**/*.ts","src/**/*.vue"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := MergePathsConfig(cfg, filepath.Join(dir, "web", "sdk"), io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	var doc struct{ Include []string }
	b, _ := os.ReadFile(cfg)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if want := []string{"src/**/*.ts", "src/**/*.vue", "sdk/client.d.ts"}; strings.Join(doc.Include, ",") != strings.Join(want, ",") {
		t.Errorf("include = %v, want %v", doc.Include, want)
	}

	bare := filepath.Join(dir, "bare", "tsconfig.json")
	if err := MergePathsConfig(bare, filepath.Join(dir, "bare", "sdk"), io.Discard); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(bare); strings.Contains(string(b), "include") {
		t.Errorf("a config without include got one:\n%s", b)
	}
}

// A solution-style root (create-vue / create-vite) compiles nothing: the
// mapping goes into the referenced config that covers src/, not the root
// and not the node config.
func TestMergePathsConfig_SolutionStyleRoot(t *testing.T) {
	dir := t.TempDir()
	web := filepath.Join(dir, "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	root := `{ "files": [], "references": [{ "path": "./tsconfig.node.json" }, { "path": "./tsconfig.app.json" }] }`
	app := `{ "compilerOptions": { "paths": { "@/*": ["./src/*"] } }, "include": ["env.d.ts", "src/**/*", "src/**/*.vue"] }`
	node := `{ "include": ["vite.config.*"] }`
	for name, body := range map[string]string{"tsconfig.json": root, "tsconfig.app.json": app, "tsconfig.node.json": node} {
		if err := os.WriteFile(filepath.Join(web, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := MergePathsConfig(filepath.Join(web, "tsconfig.json"), filepath.Join(web, "sdk"), io.Discard); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string { b, _ := os.ReadFile(filepath.Join(web, name)); return string(b) }
	if got := read("tsconfig.json"); got != root {
		t.Errorf("the solution root was rewritten:\n%s", got)
	}
	if got := read("tsconfig.node.json"); got != node {
		t.Errorf("the node config was rewritten:\n%s", got)
	}
	got := read("tsconfig.app.json")
	for _, want := range []string{`"nexus-client"`, `"./sdk/client.d.ts"`, `"@/*"`, `"sdk/client.d.ts"`} {
		if !strings.Contains(got, want) {
			t.Errorf("tsconfig.app.json missing %s:\n%s", want, got)
		}
	}
}

// A project's own 'nexus-client' mapping (a wrapper module) survives
// every merge; one naming a generated SDK file is updated.
func TestMergePathsConfig_KeepsUserNexusClient(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "tsconfig.json")
	for _, c := range []struct{ before, want string }{
		{`["./src/api/nexus.ts"]`, `"./src/api/nexus.ts"`},
		{`["./old/sdk/client.js"]`, `"./sdk/client.d.ts"`},
	} {
		if err := os.WriteFile(cfg, []byte(`{"compilerOptions":{"paths":{"nexus-client":`+c.before+`}}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := MergePathsConfig(cfg, filepath.Join(dir, "sdk"), io.Discard); err != nil {
			t.Fatal(err)
		}
		var doc struct {
			CompilerOptions struct{ Paths map[string][]string } `json:"compilerOptions"`
		}
		b, _ := os.ReadFile(cfg)
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		if got := doc.CompilerOptions.Paths["nexus-client"]; len(got) != 1 || `"`+got[0]+`"` != c.want {
			t.Errorf("before %s: nexus-client = %v, want [%s]", c.before, got, c.want)
		}
	}
}

// An absolute baseUrl is used as is, not joined onto the config dir.
func TestMergePathsConfig_AbsoluteBaseURL(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "web", "tsconfig.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"compilerOptions": map[string]any{"baseUrl": filepath.Join(dir, "web")}})
	if err := os.WriteFile(cfg, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MergePathsConfig(cfg, filepath.Join(dir, "web", "sdk"), io.Discard); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cfg); !strings.Contains(string(b), `"./sdk/client.d.ts"`) {
		t.Errorf("absolute baseUrl: want ./sdk/client.d.ts in\n%s", b)
	}
}
