package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackageJSON_LoadMissingReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	pj, err := loadPackageJSON(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(pj.Dependencies) != 0 {
		t.Errorf("Dependencies = %v, want empty", pj.Dependencies)
	}
}

func TestPackageJSON_AddDepCaretPrefixes(t *testing.T) {
	pj, _ := loadPackageJSON("/nonexistent/package.json")
	pj.addDep("vue", "3.4.21")
	if got := pj.Dependencies["vue"]; got != "^3.4.21" {
		t.Errorf("addDep('vue', '3.4.21') = %q, want '^3.4.21'", got)
	}
	// An explicit range prefix is preserved as-is.
	pj.addDep("react", "~18.0.0")
	if got := pj.Dependencies["react"]; got != "~18.0.0" {
		t.Errorf("addDep('react', '~18.0.0') = %q, want '~18.0.0'", got)
	}
}

func TestPackageJSON_AddDepIdempotent(t *testing.T) {
	pj, _ := loadPackageJSON("/nonexistent/package.json")
	pj.addDep("vue", "3.4.0")
	pj.addDep("vue", "3.5.0")
	if got := pj.Dependencies["vue"]; got != "^3.5.0" {
		t.Errorf("second addDep didn't overwrite: got %q", got)
	}
}

func TestPackageJSON_RemoveDep(t *testing.T) {
	pj, _ := loadPackageJSON("/nonexistent/package.json")
	pj.addDep("vue", "3.4.0")
	if !pj.removeDep("vue") {
		t.Error("removeDep returned false on present dep")
	}
	if pj.removeDep("vue") {
		t.Error("removeDep returned true on absent dep")
	}
}

func TestPackageJSON_PreservesExtras(t *testing.T) {
	// Stuff a real-ish package.json with fields nexus doesn't model.
	// After load + save they should still be present byte-for-byte.
	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")
	input := `{
  "name": "test-app",
  "type": "module",
  "private": true,
  "dependencies": {
    "vue": "^3.4.0"
  },
  "scripts": {
    "lint": "eslint ."
  },
  "engines": {
    "node": ">=20"
  }
}
`
	if err := os.WriteFile(path, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}
	pj, err := loadPackageJSON(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	pj.addDep("react", "18.2.0")
	if err := pj.save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, _ := os.ReadFile(path)
	body := string(out)
	if !strings.Contains(body, `"scripts"`) || !strings.Contains(body, `"lint": "eslint ."`) {
		t.Errorf("extras 'scripts' lost on round-trip:\n%s", body)
	}
	if !strings.Contains(body, `"engines"`) || !strings.Contains(body, `"node": ">=20"`) {
		t.Errorf("extras 'engines' lost on round-trip:\n%s", body)
	}
	if !strings.Contains(body, `"react": "^18.2.0"`) {
		t.Errorf("new dep not present:\n%s", body)
	}
	if !strings.Contains(body, `"vue": "^3.4.0"`) {
		t.Errorf("original dep lost:\n%s", body)
	}
}

func TestPackageJSON_DeterministicOutput(t *testing.T) {
	// Two saves of the same logical state must produce identical
	// bytes — required so commits don't churn on each `nexus add`.
	pj, _ := loadPackageJSON("/nonexistent/package.json")
	pj.Name = "test-app"
	pj.Type = "module"
	pj.Private = true
	// Add deps in a deliberately scrambled order — should still
	// emit alphabetized.
	pj.addDep("zod", "3.22.0")
	pj.addDep("vue", "3.4.0")
	pj.addDep("axios", "1.0.0")

	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.json")
	pathB := filepath.Join(dir, "b.json")
	if err := pj.save(pathA); err != nil {
		t.Fatal(err)
	}
	if err := pj.save(pathB); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(pathA)
	b, _ := os.ReadFile(pathB)
	if string(a) != string(b) {
		t.Errorf("saves differ:\nA:\n%s\nB:\n%s", a, b)
	}
	// Sanity: deps appear alphabetized.
	body := string(a)
	axiosIdx := strings.Index(body, `"axios"`)
	vueIdx := strings.Index(body, `"vue"`)
	zodIdx := strings.Index(body, `"zod"`)
	if !(axiosIdx < vueIdx && vueIdx < zodIdx) {
		t.Errorf("deps not alphabetized:\n%s", body)
	}
}
