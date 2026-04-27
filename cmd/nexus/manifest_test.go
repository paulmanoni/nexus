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
    urls:
      - http://checkout.local:8080
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
	// Single-replica peers also use the urls slice — one entry.
	if got := m.Peers["checkout-svc"].URLs; len(got) != 1 || got[0] != "http://checkout.local:8080" {
		t.Errorf("checkout-svc URLs: want [http://checkout.local:8080], got %v", got)
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

// TestLoadManifest_Listeners verifies the new `listeners:` block
// round-trips from YAML into DeploymentSpec.Listeners — scope is
// required, addr is optional.
func TestLoadManifest_Listeners(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus.deploy.yaml")
	body := `deployments:
  monolith:
    port: 8080
    listeners:
      admin:
        scope: admin
      internal:
        scope: internal
        addr: 127.0.0.1:9000
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	mono := m.Deployments["monolith"]
	if got := mono.Listeners["admin"]; got.Scope != "admin" || got.Addr != "" {
		t.Errorf("admin: want {scope:admin addr:}, got %+v", got)
	}
	if got := mono.Listeners["internal"]; got.Scope != "internal" || got.Addr != "127.0.0.1:9000" {
		t.Errorf("internal: want {scope:internal addr:127.0.0.1:9000}, got %+v", got)
	}
}

// TestWriteDeployInitFile_ListenersEmitted verifies the codegen
// emits a Listeners map with the right scope constant per entry.
// Empty Addr in YAML stays empty in the emitted struct so the
// framework's fillListenerAddrs can derive from cfg.Addr at boot.
func TestWriteDeployInitFile_ListenersEmitted(t *testing.T) {
	manifest := &DeployManifest{
		Deployments: map[string]DeploymentSpec{
			"monolith": {
				Port: 8080,
				Listeners: map[string]ListenerSpec{
					"admin":    {Scope: "admin"},
					"internal": {Scope: "internal", Addr: "127.0.0.1:9000"},
				},
			},
		},
	}
	shadowDir := t.TempDir()
	out, err := writeDeployInitFile("monolith", manifest, shadowDir, ".", shadowDir, nil)
	if err != nil {
		t.Fatalf("writeDeployInitFile: %v", err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read generated file: %v", err)
	}
	src := string(body)

	mustContain(t, src, "Listeners: map[string]nexus.Listener{")
	mustContain(t, src, "Scope: nexus.ScopeAdmin,")
	mustContain(t, src, "Scope: nexus.ScopeInternal,")
	mustContain(t, src, `Addr:  "127.0.0.1:9000",`)
}

// TestWriteDeployInitFile_UnknownScopeRejected fails the build when
// a manifest declares a scope value the framework doesn't know.
// Catches typos at the CI gate instead of producing a binary that
// silently behaves wrong.
func TestWriteDeployInitFile_UnknownScopeRejected(t *testing.T) {
	manifest := &DeployManifest{
		Deployments: map[string]DeploymentSpec{
			"monolith": {
				Port: 8080,
				Listeners: map[string]ListenerSpec{
					"admin": {Scope: "addmin"}, // typo
				},
			},
		},
	}
	shadowDir := t.TempDir()
	_, err := writeDeployInitFile("monolith", manifest, shadowDir, ".", shadowDir, nil)
	if err == nil {
		t.Fatal("expected error for unknown scope")
	}
	if !strings.Contains(err.Error(), "addmin") {
		t.Errorf("error should name the bad scope; got: %v", err)
	}
}

// TestWriteDeployInitFile_NoUrlsFallback verifies that a peer with
// no `urls:` declared still emits a one-element URLs slice using
// envOr("<TAG>_URL", "http://localhost:<port>"). Keeps `nexus dev
// --split` working with manifests that don't bother declaring per-
// replica URLs.
func TestWriteDeployInitFile_NoUrlsFallback(t *testing.T) {
	manifest := &DeployManifest{
		Deployments: map[string]DeploymentSpec{
			"checkout-svc": {Owns: []string{"checkout"}, Port: 8080},
			"users-svc":    {Owns: []string{"users"}, Port: 8081},
		},
		Peers: map[string]PeerSpec{
			"users-svc": {}, // no urls declared
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
	mustContain(t, src, `URLs: []string{`)
	mustContain(t, src, `envOr("USERS_SVC_URL", "http://localhost:8081"),`)
}
