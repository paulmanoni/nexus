package client

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
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
