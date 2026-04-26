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

// TestScaffoldAndBuild exercises the scaffolder end-to-end: we generate
// a fresh project into a temp dir, point it at the in-repo nexus via a
// replace directive, run `go mod tidy`, and `go build .` to prove the
// generated template compiles against the current framework. If this
// test breaks, the scaffold is drifting from the public API.
func TestScaffoldAndBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build test in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	_, here, _, _ := runtime.Caller(0)
	repoRoot, err := filepath.Abs(filepath.Join(filepath.Dir(here), "..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "nexus.go")); err != nil {
		t.Fatalf("expected nexus.go at %s: %v", repoRoot, err)
	}

	dir := filepath.Join(t.TempDir(), "myapp")
	var stdout bytes.Buffer
	if err := scaffold(dir, "", &stdout); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	if !strings.Contains(stdout.String(), "Scaffolded") {
		t.Fatalf("expected Scaffolded message, got: %q", stdout.String())
	}
	for _, name := range []string{"go.mod", "main.go", "module.go", ".gitignore", "README.md", "nexus.deploy.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	// Sanity-check the manifest looks like a manifest, not an empty
	// stub — catches a future template that accidentally writes ""
	// past the test for file-existence.
	manifest, err := os.ReadFile(filepath.Join(dir, "nexus.deploy.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	for _, want := range []string{"deployments:", "monolith:", "port: 8080"} {
		if !strings.Contains(string(manifest), want) {
			t.Fatalf("manifest missing %q:\n%s", want, manifest)
		}
	}

	addReplace := exec.Command("go", "mod", "edit",
		"-replace", "github.com/paulmanoni/nexus="+repoRoot,
		"-require", "github.com/paulmanoni/nexus@v0.0.0",
	)
	addReplace.Dir = dir
	if out, err := addReplace.CombinedOutput(); err != nil {
		t.Fatalf("go mod edit: %v\n%s", err, out)
	}
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
	var stdout bytes.Buffer
	err := scaffold(dir, "", &stdout)
	if err == nil {
		t.Fatal("expected error for non-empty dir, got nil")
	}
	if !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("expected 'not empty' in error, got: %v", err)
	}
}

func TestScaffold_InvalidModulePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "app")
	var stdout bytes.Buffer
	err := scaffold(dir, "has a space", &stdout)
	if err == nil {
		t.Fatal("expected error for bad module path, got nil")
	}
}

// TestCobra_VersionCommand asserts the cobra wiring routes the
// `version` subcommand to its handler — guards against accidental
// reorganization of the command tree dropping the brand line.
func TestCobra_VersionCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := newRootCmd(&stdout, &stderr)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v (stderr=%s)", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "nexus") {
		t.Fatalf("version output missing brand: %q", stdout.String())
	}
}

// TestCobra_UnknownCommand confirms cobra surfaces an error for typos.
// This covers the same contract the old TestRun_Unknown test did.
func TestCobra_UnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := newRootCmd(&stdout, &stderr)
	root.SetArgs([]string{"whatever"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for unknown command")
	}
}