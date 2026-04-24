package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestScaffoldAndBuild exercises the scaffolder end-to-end: we generate a
// fresh project into a temp dir, point it at the in-repo nexus via a replace
// directive, run `go mod tidy`, and `go build .` to prove the generated
// template compiles against the current framework. If this test breaks, it
// means the scaffold is drifting away from the framework's public API.
func TestScaffoldAndBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build test in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}

	// Find the repo root (three dirs up from cmd/nexus/new_test.go) so the
	// replace directive can point at a stable absolute path.
	_, here, _, _ := runtime.Caller(0)
	repoRoot, err := filepath.Abs(filepath.Join(filepath.Dir(here), "..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	// Sanity: the repo root should contain nexus.go.
	if _, err := os.Stat(filepath.Join(repoRoot, "nexus.go")); err != nil {
		t.Fatalf("expected nexus.go at %s: %v", repoRoot, err)
	}

	dir := filepath.Join(t.TempDir(), "myapp")
	var stdout, stderr bytes.Buffer
	if err := cmdNew([]string{dir}, &stdout, &stderr); err != nil {
		t.Fatalf("cmdNew: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Scaffolded") {
		t.Fatalf("expected Scaffolded message, got: %q", stdout.String())
	}

	// Expected files must all land.
	for _, name := range []string{"go.mod", "main.go", "module.go", ".gitignore", "README.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}

	// Point the scaffolded project at our in-repo nexus.
	addReplace := exec.Command("go", "mod", "edit",
		"-replace", "github.com/paulmanoni/nexus="+repoRoot,
		"-require", "github.com/paulmanoni/nexus@v0.0.0",
	)
	addReplace.Dir = dir
	if out, err := addReplace.CombinedOutput(); err != nil {
		t.Fatalf("go mod edit: %v\n%s", err, out)
	}

	// `go mod tidy` then `go build` — the real compile-time contract test.
	for _, step := range [][]string{
		{"go", "mod", "tidy"},
		{"go", "build", "."},
	} {
		cmd := exec.Command(step[0], step[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s failed: %v\n%s", strings.Join(step, " "), err, out)
		}
	}
}

func TestScaffold_RejectsNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err := cmdNew([]string{dir}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for non-empty dir, got nil")
	}
	if !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("expected 'not empty' in error, got: %v", err)
	}
}

func TestScaffold_InvalidModulePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app")
	var stdout, stderr bytes.Buffer
	err := cmdNew([]string{"-module", "has a space", dir}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for bad module path, got nil")
	}
}

func TestRun_Unknown(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{"whatever"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for unknown command")
	}
}

func TestRun_Version(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run([]string{"version"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "nexus") {
		t.Fatalf("version output missing brand: %q", stdout.String())
	}
}