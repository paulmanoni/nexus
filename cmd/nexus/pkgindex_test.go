package main

import (
	"os"
	"path/filepath"
	"testing"
)

// tinyModule writes a module with a main package and one library package,
// and returns its root.
func tinyModule(t *testing.T, modulePath string) string {
	t.Helper()
	root := t.TempDir()
	mkpkg := func(dir, src string) {
		if dir != "" {
			if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", dir, err)
			}
		}
		if err := os.WriteFile(filepath.Join(root, dir, "x.go"), []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module "+modulePath+"\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	mkpkg("", "package main\n\nfunc main() {}\n")
	mkpkg("store", "package store\n")
	return root
}

func TestPkgIndexResolvesAndCaches(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go toolchain")
	}
	root := tinyModule(t, "example.test/app")
	idx := &pkgIndex{}

	main, err := idx.mainDir(root)
	if err != nil {
		t.Fatalf("mainDir: %v", err)
	}
	resolved, _ := filepath.EvalSymlinks(root)
	if main != root && main != resolved {
		t.Errorf("mainDir = %q, want %q", main, root)
	}

	got, err := idx.importPath(root, filepath.Join(root, "store"))
	if err != nil {
		t.Fatalf("importPath: %v", err)
	}
	if got != "example.test/app/store" {
		t.Errorf("importPath = %q, want example.test/app/store", got)
	}

	// The point of the index: repeated lookups must not shell out again.
	runs := idx.listRuns
	for i := 0; i < 5; i++ {
		if _, err := idx.mainDir(root); err != nil {
			t.Fatalf("mainDir repeat: %v", err)
		}
		if _, err := idx.importPath(root, filepath.Join(root, "store")); err != nil {
			t.Fatalf("importPath repeat: %v", err)
		}
	}
	if idx.listRuns != runs {
		t.Errorf("cache missed: %d extra `go list` runs", idx.listRuns-runs)
	}
}

func TestPkgIndexPicksUpNewPackage(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go toolchain")
	}
	root := tinyModule(t, "example.test/app")
	idx := &pkgIndex{}
	if _, err := idx.importPath(root, filepath.Join(root, "store")); err != nil {
		t.Fatalf("warm: %v", err)
	}

	// A package created mid-session is a cache miss, not an error.
	fresh := filepath.Join(root, "billing")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fresh, "x.go"), []byte("package billing\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := idx.importPath(root, fresh)
	if err != nil {
		t.Fatalf("importPath: %v", err)
	}
	if got != "example.test/app/billing" {
		t.Errorf("importPath = %q, want example.test/app/billing", got)
	}
}

func TestPkgIndexInvalidatesOnModuleChange(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go toolchain")
	}
	root := tinyModule(t, "example.test/app")
	idx := &pkgIndex{}
	if _, err := idx.importPath(root, filepath.Join(root, "store")); err != nil {
		t.Fatalf("warm: %v", err)
	}

	// Renaming the module must not leave stale import paths behind — the
	// generated aggregator would import packages that no longer exist.
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.test/renamed\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatalf("rewrite go.mod: %v", err)
	}
	got, err := idx.importPath(root, filepath.Join(root, "store"))
	if err != nil {
		t.Fatalf("importPath: %v", err)
	}
	if got != "example.test/renamed/store" {
		t.Errorf("importPath = %q, want example.test/renamed/store (stale cache)", got)
	}
}

func TestPkgIndexWithoutModule(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go toolchain")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	idx := &pkgIndex{}
	main, err := idx.mainDir(dir)
	if err != nil {
		t.Fatalf("mainDir: %v", err)
	}
	if main != "" {
		t.Errorf("mainDir = %q, want empty (no module → skip the aggregator)", main)
	}
}

func TestModuleStampChangesWithGoMod(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	gomod := filepath.Join(root, "go.mod")
	if err := os.WriteFile(gomod, []byte("module example.test/app\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Found by walking up from a nested package dir.
	first := moduleStamp(sub)
	if first == "" {
		t.Fatal("no stamp for a dir inside a module")
	}
	if got := moduleStamp(root); got != first {
		t.Errorf("stamp differs by scan depth: %q vs %q", got, first)
	}
	if err := os.WriteFile(gomod, []byte("module example.test/app\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if moduleStamp(sub) == first {
		t.Error("stamp unchanged after go.mod edit")
	}
}
