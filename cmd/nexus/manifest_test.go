package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadManifest_PeerURLs verifies the new `urls:` field round-trips
// from YAML into PeerSpec.URLs as an ordered slice. Mixed literal +
// env-interpolation entries are kept verbatim — codegen handles the
// expansion later.
func TestLoadManifest_PeerURLs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus.deploy.yaml")
	body := `deployments:
  monolith:
    port: 8080

peers:
  users-svc:
    urls:
      - http://users-1.local:8080
      - http://users-2.local:8080
      - ${USERS_SVC_REPLICA_3_URL}
    timeout: 2s
  checkout-svc:
    url: http://checkout.local:8080
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	users := m.Peers["users-svc"]
	want := []string{
		"http://users-1.local:8080",
		"http://users-2.local:8080",
		"${USERS_SVC_REPLICA_3_URL}",
	}
	if len(users.URLs) != len(want) {
		t.Fatalf("URLs len: want %d, got %d (%v)", len(want), len(users.URLs), users.URLs)
	}
	for i, w := range want {
		if users.URLs[i] != w {
			t.Errorf("URLs[%d]: want %q, got %q", i, w, users.URLs[i])
		}
	}
	// Sugar form still parses into the singular URL field.
	if got := m.Peers["checkout-svc"].URL; got != "http://checkout.local:8080" {
		t.Errorf("checkout-svc URL: want %q, got %q", "http://checkout.local:8080", got)
	}
}

// TestWriteDeployInitFile_URLsEmitted verifies that the codegen
// emits a Peer.URLs slice with each entry properly expressed
// (literal vs os.Getenv). The generated source isn't compiled here
// (no main package) — the test just confirms the textual shape.
func TestWriteDeployInitFile_URLsEmitted(t *testing.T) {
	manifest := &DeployManifest{
		Deployments: map[string]DeploymentSpec{
			"checkout-svc": {Owns: []string{"checkout"}, Port: 8080},
			"users-svc":    {Owns: []string{"users"}, Port: 8081},
		},
		Peers: map[string]PeerSpec{
			"users-svc": {
				URLs: []string{
					"http://users-1.local:8080",
					"${USERS_SVC_REPLICA_2_URL}",
				},
			},
		},
	}
	shadowDir := t.TempDir()
	out, err := writeDeployInitFile("checkout-svc", manifest, shadowDir, ".", shadowDir, nil)
	if err != nil {
		t.Fatalf("writeDeployInitFile: %v", err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read generated file: %v", err)
	}
	src := string(body)

	// Slice form, in order.
	mustContain(t, src, `URLs: []string{`)
	mustContain(t, src, `"http://users-1.local:8080",`)
	mustContain(t, src, `os.Getenv("USERS_SVC_REPLICA_2_URL"),`)
	// The singular URL field should NOT be emitted when URLs is set.
	if strings.Contains(src, "URL: ") {
		// "URL: " (with space) is the singular form; "URLs:" uses
		// no space after the colon-equivalent opener.
		t.Errorf("singular URL should not be emitted when URLs is set; got:\n%s", src)
	}
}

// TestWriteDeployInitFile_URLsEnvDefault verifies the ${VAR:-fallback}
// shell-style default lifts to envOr("VAR", "fallback") in the
// generated source. This is what makes a urls-enabled manifest run
// with `nexus dev --split` out of the box: each replica falls back
// to the local dev port when no per-replica env var is set.
func TestWriteDeployInitFile_URLsEnvDefault(t *testing.T) {
	manifest := &DeployManifest{
		Deployments: map[string]DeploymentSpec{
			"checkout-svc": {Owns: []string{"checkout"}, Port: 8080},
			"users-svc":    {Owns: []string{"users"}, Port: 8081},
		},
		Peers: map[string]PeerSpec{
			"users-svc": {
				URLs: []string{
					"${USERS_SVC_REPLICA_1_URL:-http://localhost:8081}",
					"${USERS_SVC_REPLICA_2_URL:-http://localhost:8081}",
				},
			},
		},
	}
	shadowDir := t.TempDir()
	out, err := writeDeployInitFile("checkout-svc", manifest, shadowDir, ".", shadowDir, nil)
	if err != nil {
		t.Fatalf("writeDeployInitFile: %v", err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read generated file: %v", err)
	}
	src := string(body)

	mustContain(t, src, `envOr("USERS_SVC_REPLICA_1_URL", "http://localhost:8081"),`)
	mustContain(t, src, `envOr("USERS_SVC_REPLICA_2_URL", "http://localhost:8081"),`)
}

// TestWriteDeployInitFile_SingularURLBackCompat verifies the existing
// singular `url:` path still emits Peer.URL — back-compat for every
// manifest that hasn't moved to URLs.
func TestWriteDeployInitFile_SingularURLBackCompat(t *testing.T) {
	manifest := &DeployManifest{
		Deployments: map[string]DeploymentSpec{
			"checkout-svc": {Owns: []string{"checkout"}, Port: 8080},
			"users-svc":    {Owns: []string{"users"}, Port: 8081},
		},
		Peers: map[string]PeerSpec{
			"users-svc": {URL: "http://users.local:8081"},
		},
	}
	shadowDir := t.TempDir()
	out, err := writeDeployInitFile("checkout-svc", manifest, shadowDir, ".", shadowDir, nil)
	if err != nil {
		t.Fatalf("writeDeployInitFile: %v", err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read generated file: %v", err)
	}
	src := string(body)
	mustContain(t, src, `URL: "http://users.local:8081"`)
	if strings.Contains(src, "URLs: []string{") {
		t.Errorf("URLs slice should not be emitted when only URL is set; got:\n%s", src)
	}
}
