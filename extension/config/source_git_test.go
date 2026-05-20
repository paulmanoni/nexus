package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestFromGit_CloneAndLoad drives the headline path: a local
// bare repo seeded with one app yaml, FromGit clones it, Load
// produces the expected appBody.
//
// Skips when git isn't on PATH — the framework documents this
// as a hard prereq; the test doesn't need to enforce it on
// CI hosts that don't have it (e.g. nano images).
func TestFromGit_CloneAndLoad(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}

	// Build a local bare repo + a working tree to push from.
	tmp := t.TempDir()
	bareRepo := filepath.Join(tmp, "bare.git")
	mustRunGit(t, tmp, "init", "--bare", bareRepo)

	workdir := filepath.Join(tmp, "src")
	mustRunGit(t, tmp, "clone", bareRepo, workdir)

	// Seed an app yaml + commit + push.
	yamlBody := `app: app1
profiles:
  default:
    api:
      timeout: 5s
`
	if err := os.WriteFile(filepath.Join(workdir, "app1.nexus.config.yaml"),
		[]byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, workdir, "config", "user.email", "test@example.com")
	mustRunGit(t, workdir, "config", "user.name", "test")
	mustRunGit(t, workdir, "add", ".")
	mustRunGit(t, workdir, "commit", "-m", "seed")
	mustRunGit(t, workdir, "branch", "-M", "main")
	mustRunGit(t, workdir, "push", "origin", "main")

	// Now point FromGit at the bare repo and Load.
	clonePath := filepath.Join(tmp, "configd-clone")
	src := FromGit(bareRepo,
		GitBranch("main"),
		GitClonePath(clonePath),
	)
	content, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	body, ok := content["app1"]
	if !ok {
		t.Fatalf("Load returned no app1 entry; got keys %v", keysOfMap(content))
	}
	got := body.Profiles["default"]["api"].(map[string]any)["timeout"]
	if got != "5s" {
		t.Errorf("api.timeout = %v, want 5s", got)
	}
}

// TestFromGit_FetchPicksUpUpstreamChanges proves the second
// Load fetches new upstream commits. Reuse the same clone
// directory across Loads; the working tree should match
// upstream HEAD on every call.
func TestFromGit_FetchPicksUpUpstreamChanges(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not on PATH")
	}

	tmp := t.TempDir()
	bareRepo := filepath.Join(tmp, "bare.git")
	mustRunGit(t, tmp, "init", "--bare", bareRepo)
	workdir := filepath.Join(tmp, "src")
	mustRunGit(t, tmp, "clone", bareRepo, workdir)
	mustRunGit(t, workdir, "config", "user.email", "test@example.com")
	mustRunGit(t, workdir, "config", "user.name", "test")

	writeAppYAML := func(timeout string) {
		body := `app: app1
profiles:
  default:
    api:
      timeout: ` + timeout + "\n"
		if err := os.WriteFile(filepath.Join(workdir, "app1.nexus.config.yaml"),
			[]byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg string) {
		mustRunGit(t, workdir, "add", ".")
		mustRunGit(t, workdir, "commit", "-m", msg)
		mustRunGit(t, workdir, "push", "origin", "HEAD:main")
	}
	writeAppYAML("5s")
	commit("v1")
	mustRunGit(t, workdir, "branch", "-M", "main")
	mustRunGit(t, workdir, "push", "-u", "origin", "main")

	src := FromGit(bareRepo,
		GitBranch("main"),
		GitClonePath(filepath.Join(tmp, "configd-clone")),
	)
	if _, err := src.Load(context.Background()); err != nil {
		t.Fatalf("first Load: %v", err)
	}

	// Upstream edit + push.
	writeAppYAML("30s")
	commit("v2")

	// Second Load should pick up v2.
	content, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	got := content["app1"].Profiles["default"]["api"].(map[string]any)["timeout"]
	if got != "30s" {
		t.Errorf("after upstream update, api.timeout = %v, want 30s", got)
	}
}

// mustRunGit is the test-only helper. Fails the test on git
// errors so a misconfigured fixture doesn't masquerade as a
// FromGit bug.
func mustRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\noutput: %s", args, err, out)
	}
}

func keysOfMap[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
