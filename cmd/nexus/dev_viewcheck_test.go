package main

import (
	"os"
	"path/filepath"
	"testing"
)

// By default nexus dev compiles views in memory: nothing is written, and a
// change to the compiled output asks for a rebuild.
func TestViewsCheckGeneratorWritesNothing(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/app\n\ngo 1.26\n")
	home := filepath.Join(dir, "pages", "home.templ")
	writeFile(t, home, "package pages\n\ntempl Home() {\n\t<p>one</p>\n}\n")
	rebuilds := 0
	gens := devGenerators(dir, func() { rebuilds++ })
	if len(gens) != 1 || gens[0].name != "views" {
		t.Fatalf("gens = %+v", gens)
	}
	g := gens[0]
	if s, err := g.run(); err != nil || s == "" {
		t.Fatalf("first run = %q, %v", s, err)
	}
	if s, err := g.run(); err != nil || s != "" || rebuilds != 0 {
		t.Fatalf("unchanged run = %q, %v (rebuilds %d)", s, err, rebuilds)
	}
	writeFile(t, home, "package pages\n\ntempl Home() {\n\t<p>two</p>\n}\n")
	if s, err := g.run(); err != nil || s == "" || rebuilds != 1 {
		t.Fatalf("changed run = %q, %v (rebuilds %d)", s, err, rebuilds)
	}
	writeFile(t, home, "package pages\n\ntempl Home() {\n\t<p>{ </p>\n}\n")
	if _, err := g.run(); err == nil || rebuilds != 1 {
		t.Fatalf("a compile error must be reported, not rebuilt (err %v, rebuilds %d)", err, rebuilds)
	}
	if _, err := os.Stat(filepath.Join(dir, "pages", "home_templ.go")); err == nil {
		t.Fatal("generated Go was written to disk")
	}
}
