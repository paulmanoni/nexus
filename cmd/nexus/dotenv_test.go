package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestLoadViteEnv_FullStack: drops the four standard .env files
// into a temp project + verifies precedence (later overrides
// earlier) and the VITE_ prefix filter.
func TestLoadViteEnv_FullStack(t *testing.T) {
	tmp := t.TempDir()
	// Base — gets overridden.
	mustEnvFile(t, filepath.Join(tmp, ".env"), `
VITE_API=https://base.example.com
VITE_KEEP=base-only
SECRET_DB_PASSWORD=should-not-leak
`)
	// Local overrides VITE_API.
	mustEnvFile(t, filepath.Join(tmp, ".env.local"), `
VITE_API=https://local.example.com
`)
	// Mode-specific — wins over both above.
	mustEnvFile(t, filepath.Join(tmp, ".env.development"), `
VITE_API=https://dev.example.com
VITE_MODE_ONLY=dev-special
`)

	var stdout bytes.Buffer
	env, err := loadViteEnv(tmp, "development", &stdout)
	if err != nil {
		t.Fatalf("loadViteEnv: %v", err)
	}
	// VITE_API: .env.development wins.
	if env["VITE_API"] != "https://dev.example.com" {
		t.Errorf("VITE_API = %q, want dev (most-specific should win)", env["VITE_API"])
	}
	// VITE_KEEP: only in .env, but should still be present.
	if env["VITE_KEEP"] != "base-only" {
		t.Errorf("VITE_KEEP = %q, want base-only", env["VITE_KEEP"])
	}
	// VITE_MODE_ONLY: only in .env.development.
	if env["VITE_MODE_ONLY"] != "dev-special" {
		t.Errorf("VITE_MODE_ONLY = %q, want dev-special", env["VITE_MODE_ONLY"])
	}
	// SECRET_DB_PASSWORD: must NOT be in the result — only
	// VITE_-prefixed vars are exposed.
	if _, ok := env["SECRET_DB_PASSWORD"]; ok {
		t.Error("non-VITE_ vars must NOT leak to the bundle env — security regression")
	}
}

// TestLoadViteEnv_NoFiles: zero .env files present → empty
// result, no error. Operators may have no env config at all.
func TestLoadViteEnv_NoFiles(t *testing.T) {
	tmp := t.TempDir()
	var stdout bytes.Buffer
	env, err := loadViteEnv(tmp, "development", &stdout)
	if err != nil {
		t.Fatalf("expected no error for missing .env files, got: %v", err)
	}
	if len(env) != 0 {
		t.Errorf("expected empty map, got %v", env)
	}
}

// TestParseDotEnvFile_Shapes: the parser handles the common
// shapes Vite supports — quoted values, comments, blank lines,
// `export` prefix, surrounding quotes both single + double.
func TestParseDotEnvFile_Shapes(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, ".env")
	mustEnvFile(t, path, `
# Comment line
VITE_PLAIN=plain-value
VITE_DOUBLE_QUOTED="has spaces"
VITE_SINGLE_QUOTED='also spaces'
export VITE_EXPORTED=shell-style
VITE_URL=https://api.example.com/path?x=1

# Blank line above is fine
malformed-line-no-equals
VITE_TRAILING_SPACES   =   trimmed
`)
	got, err := parseDotEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"VITE_PLAIN":            "plain-value",
		"VITE_DOUBLE_QUOTED":    "has spaces",
		"VITE_SINGLE_QUOTED":    "also spaces",
		"VITE_EXPORTED":         "shell-style",
		"VITE_URL":              "https://api.example.com/path?x=1",
		"VITE_TRAILING_SPACES":  "trimmed",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
	// Malformed lines silently dropped.
	if _, ok := got["malformed-line-no-equals"]; ok {
		t.Error("malformed lines should be dropped silently")
	}
}

func mustEnvFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
