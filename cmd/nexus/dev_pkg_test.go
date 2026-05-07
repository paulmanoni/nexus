package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureDevBuildScript_IntoExistingScripts pins the common case:
// a package.json that already has a "scripts" block gets the new
// dev:build entry prepended without disturbing the rest.
func TestEnsureDevBuildScript_IntoExistingScripts(t *testing.T) {
	dir := t.TempDir()
	original := `{
  "name": "web",
  "scripts": {
    "dev": "vite",
    "build": "vite build"
  },
  "dependencies": {
    "vue": "^3.0.0"
  }
}
`
	pkg := filepath.Join(dir, "package.json")
	if err := os.WriteFile(pkg, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureDevBuildScript(dir, io.Discard); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	got := readPkg(t, pkg)
	if !strings.Contains(got, `"dev:build": "vite build --watch --emptyOutDir false"`) {
		t.Errorf("dev:build entry missing\n--- body ---\n%s", got)
	}
	for _, want := range []string{`"dev": "vite"`, `"build": "vite build"`, `"vue": "^3.0.0"`} {
		if !strings.Contains(got, want) {
			t.Errorf("clobbered existing field %q\n--- body ---\n%s", want, got)
		}
	}
	mustParseJSON(t, got)
}

// TestEnsureDevBuildScript_Idempotent verifies the literal-substring
// guard: re-running once the entry exists is a no-op, even when
// surrounding fields shift around.
func TestEnsureDevBuildScript_Idempotent(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "package.json")
	original := `{
  "scripts": {
    "dev:build": "vite build --watch --emptyOutDir false"
  }
}
`
	if err := os.WriteFile(pkg, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureDevBuildScript(dir, io.Discard); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	got := readPkg(t, pkg)
	if got != original {
		t.Errorf("idempotent run modified the file\n--- got ---\n%s\n--- want ---\n%s", got, original)
	}
	if strings.Count(got, `"dev:build"`) != 1 {
		t.Errorf("dev:build entry duplicated: %s", got)
	}
}

// TestEnsureDevBuildScript_NoScriptsObject covers the case where the
// project's package.json has no scripts block — the helper has to
// invent one without breaking the surrounding JSON.
func TestEnsureDevBuildScript_NoScriptsObject(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "package.json")
	original := `{
  "name": "web",
  "version": "0.0.1"
}
`
	if err := os.WriteFile(pkg, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureDevBuildScript(dir, io.Discard); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	got := readPkg(t, pkg)
	if !strings.Contains(got, `"scripts"`) || !strings.Contains(got, `"dev:build"`) {
		t.Errorf("scripts block not added\n--- body ---\n%s", got)
	}
	if !strings.Contains(got, `"name": "web"`) {
		t.Errorf("clobbered name field\n--- body ---\n%s", got)
	}
	mustParseJSON(t, got)
}

// TestEnsureDevBuildScript_EmptyScriptsBlock confirms the helper
// emits a properly-terminated entry (no dangling comma) when the
// scripts block is empty.
func TestEnsureDevBuildScript_EmptyScriptsBlock(t *testing.T) {
	dir := t.TempDir()
	pkg := filepath.Join(dir, "package.json")
	original := `{
  "scripts": {}
}
`
	if err := os.WriteFile(pkg, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureDevBuildScript(dir, io.Discard); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	got := readPkg(t, pkg)
	if !strings.Contains(got, `"dev:build"`) {
		t.Errorf("entry missing\n--- body ---\n%s", got)
	}
	mustParseJSON(t, got)
}

// TestEnsureDevBuildScript_MissingFile silences when there's no
// package.json at all — the user might be using a non-npm toolchain.
func TestEnsureDevBuildScript_MissingFile(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	if err := ensureDevBuildScript(dir, &buf); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !strings.Contains(buf.String(), "skip script injection") {
		t.Errorf("expected skip notice, got %q", buf.String())
	}
}

func readPkg(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustParseJSON(t *testing.T, body string) {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("output is not valid JSON: %v\n--- body ---\n%s", err, body)
	}
}