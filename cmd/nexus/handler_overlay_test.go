package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile is a tiny helper for the resolver tests.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestImportsOfFileBlankAndDot(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.go")
	writeFile(t, f, `package x

import (
	_ "github.com/acme/inertia"
	. "fmt"
	al "github.com/acme/aliased"
	"github.com/acme/plain"
)
`)
	imps, err := importsOfFile(f)
	if err != nil {
		t.Fatal(err)
	}
	// Blank import recorded under its path tail as a PLAIN import.
	if got := imps["inertia"]; got != `"github.com/acme/inertia"` {
		t.Errorf("blank import: got %q", got)
	}
	// Dot import dropped (no usable selector).
	if _, ok := imps["fmt"]; ok {
		t.Errorf("dot import should be skipped, got %q", imps["fmt"])
	}
	// Alias preserved.
	if got := imps["al"]; got != `al "github.com/acme/aliased"` {
		t.Errorf("alias: got %q", got)
	}
	// Plain import keyed by path tail.
	if got := imps["plain"]; got != `"github.com/acme/plain"` {
		t.Errorf("plain: got %q", got)
	}
}

func TestImportLineFor(t *testing.T) {
	if got := importLineFor("inertia", "github.com/x/inertia"); got != `"github.com/x/inertia"` {
		t.Errorf("tail==sel should be plain, got %q", got)
	}
	if got := importLineFor("inertia", "github.com/x/inertiapkg"); got != `inertia "github.com/x/inertiapkg"` {
		t.Errorf("tail!=sel should alias, got %q", got)
	}
}

func TestResolveLayer1FileImport(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "h.go")
	writeFile(t, f, "package h\n\nimport \"github.com/acme/inertia\"\n")
	r := newSelectorResolver(dir)
	got, err := r.resolve(f, "inertia")
	if err != nil {
		t.Fatal(err)
	}
	if got != `"github.com/acme/inertia"` {
		t.Errorf("layer1: got %q", got)
	}
}

func TestResolveLayer2SiblingImport(t *testing.T) {
	dir := t.TempDir()
	// The annotated file does NOT import inertia...
	annotated := filepath.Join(dir, "academic_level.go")
	writeFile(t, annotated, "package settings\n")
	// ...but a sibling file in the same package does.
	writeFile(t, filepath.Join(dir, "module.go"), "package settings\n\nimport \"github.com/acme/inertia\"\n")
	r := newSelectorResolver(dir)
	got, err := r.resolve(annotated, "inertia")
	if err != nil {
		t.Fatal(err)
	}
	if got != `"github.com/acme/inertia"` {
		t.Errorf("layer2: got %q", got)
	}
}

func TestResolveLayer4TomlHint(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "h.go")
	writeFile(t, f, "package h\n")
	writeFile(t, filepath.Join(dir, "nexus.toml"), "[decorators.imports]\ninertia = \"github.com/custom/inertia\"\n")
	r := newSelectorResolver(dir)
	got, err := r.resolve(f, "inertia")
	if err != nil {
		t.Fatal(err)
	}
	if got != `"github.com/custom/inertia"` {
		t.Errorf("layer4: got %q", got)
	}
}

func TestResolveLayer3ModuleGraph(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/m\n\ngo 1.21\n")
	writeFile(t, filepath.Join(dir, "foo", "foo.go"), "package foo\n")
	annotated := filepath.Join(dir, "pages", "p.go")
	writeFile(t, annotated, "package pages\n")
	r := newSelectorResolver(dir)
	got, err := r.resolve(annotated, "foo")
	if err != nil {
		t.Fatalf("layer3: %v", err)
	}
	if got != `"example.com/m/foo"` {
		t.Errorf("layer3: got %q", got)
	}
}

func TestResolveLayer3Ambiguous(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/m\n\ngo 1.21\n")
	writeFile(t, filepath.Join(dir, "a", "a.go"), "package foo\n")
	writeFile(t, filepath.Join(dir, "b", "b.go"), "package foo\n")
	annotated := filepath.Join(dir, "pages", "p.go")
	writeFile(t, annotated, "package pages\n")
	r := newSelectorResolver(dir)
	_, err := r.resolve(annotated, "foo")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/m\n\ngo 1.21\n")
	annotated := filepath.Join(dir, "pages", "p.go")
	writeFile(t, annotated, "package pages\n")
	r := newSelectorResolver(dir)
	_, err := r.resolve(annotated, "nope")
	if err == nil || !strings.Contains(err.Error(), "not a dependency") {
		t.Fatalf("expected not-a-dependency error, got %v", err)
	}
}
