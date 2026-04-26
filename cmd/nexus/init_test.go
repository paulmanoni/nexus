package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInit_MonolithFallback exercises `nexus init` against an empty
// directory: no DeployAs tags to discover, so the monolith-only
// template lands.
func TestInit_MonolithFallback(t *testing.T) {
	dir := t.TempDir()
	var stdout bytes.Buffer
	if err := runInit(dir, false, &stdout); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "nexus.deploy.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	for _, want := range []string{"deployments:", "monolith:", "port: 8080"} {
		if !strings.Contains(string(manifest), want) {
			t.Fatalf("manifest missing %q:\n%s", want, manifest)
		}
	}
	if !strings.Contains(stdout.String(), "No nexus.DeployAs") {
		t.Fatalf("expected fallback hint in output, got: %q", stdout.String())
	}
}

// TestInit_RefusesOverwrite confirms a second init without --force
// is a clean error rather than a silent clobber.
func TestInit_RefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nexus.deploy.yaml"), []byte("existing: yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err := runInit(dir, false, &stdout)
	if err == nil {
		t.Fatal("expected error for existing manifest, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected 'already exists' in error, got: %v", err)
	}
}

// TestInit_ForceOverwrites confirms --force replaces an existing
// manifest. Used by users who want to regenerate the starter
// template after rewriting their topology.
func TestInit_ForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nexus.deploy.yaml"), []byte("existing: yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := runInit(dir, true, &stdout); err != nil {
		t.Fatalf("runInit --force: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "nexus.deploy.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if strings.Contains(string(manifest), "existing: yes") {
		t.Fatalf("force should have replaced the manifest, but old content remained:\n%s", manifest)
	}
	if !strings.Contains(string(manifest), "monolith:") {
		t.Fatalf("expected new manifest content, got:\n%s", manifest)
	}
}

// TestInit_RejectsMissingDir verifies the cobra wrapper's primary
// safety: pointing at a non-existent path should error, not create
// the directory implicitly. `nexus init` is a config-add operation,
// not a project scaffolder.
func TestInit_RejectsMissingDir(t *testing.T) {
	var stdout bytes.Buffer
	err := runInit(filepath.Join(t.TempDir(), "does-not-exist"), false, &stdout)
	if err == nil {
		t.Fatal("expected error for missing directory")
	}
}

// TestInit_TagToEnvVar exercises the env-var name conversion used
// in pre-populated peer entries: lowercases + dashes-to-underscores
// applied uniformly so the manifest hint matches what the runtime
// reads.
func TestInit_TagToEnvVar(t *testing.T) {
	cases := []struct {
		tag, want string
	}{
		{"users-svc", "USERS_SVC"},
		{"orders", "ORDERS"},
		{"my-cool-svc", "MY_COOL_SVC"},
		{"", ""},
	}
	for _, c := range cases {
		got := tagToEnvVarName(c.tag)
		if got != c.want {
			t.Errorf("tagToEnvVarName(%q) = %q, want %q", c.tag, got, c.want)
		}
	}
}

// TestInit_ModuleNameFromTag exercises the heuristic for inferring a
// module name from a deploy tag — strips a "-svc" suffix when
// present, leaves anything else untouched.
func TestInit_ModuleNameFromTag(t *testing.T) {
	cases := []struct {
		tag, want string
	}{
		{"users-svc", "users"},
		{"orders", "orders"},
		{"a-b-svc", "a-b"},
		{"-svc", "-svc"}, // too short to strip safely
	}
	for _, c := range cases {
		got := moduleNameFromTag(c.tag)
		if got != c.want {
			t.Errorf("moduleNameFromTag(%q) = %q, want %q", c.tag, got, c.want)
		}
	}
}
