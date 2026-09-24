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
	if want := []string{"src/**/*.ts", "src/**/*.vue", "sdk/*.d.ts"}; strings.Join(doc.Include, ",") != strings.Join(want, ",") {
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
