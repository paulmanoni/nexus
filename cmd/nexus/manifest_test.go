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
			"checkout-svc": {Owns: &[]string{"checkout"}, Port: 8080},
			"users-svc":    {Owns: &[]string{"users"}, Port: 8081},
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
			"checkout-svc": {Owns: &[]string{"checkout"}, Port: 8080},
			"users-svc":    {Owns: &[]string{"users"}, Port: 8081},
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
			"checkout-svc": {Owns: &[]string{"checkout"}, Port: 8080},
			"users-svc":    {Owns: &[]string{"users"}, Port: 8081},
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

// TestOwns_OmittedVsExplicitEmpty verifies the three-shape
// semantic for the `owns:` key. yaml.v3 preserves the
// distinction between an absent key (nil slice) and an
// explicit empty list ([]string{}) — Owns() consumes that
// distinction so a frontend-only deployment (`owns: []`) is
// genuinely owns-nothing while a monolith (no owns key) is
// owns-everything.
func TestOwns_OmittedVsExplicitEmpty(t *testing.T) {
	yaml := `
deployments:
  monolith:
    port: 8080
  web-svc:
    owns: []
    port: 9000
  uaa-svc:
    owns: [uaa]
    port: 9001
`
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus.deploy.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	cases := []struct {
		dep, mod string
		want     bool
	}{
		// Monolith owns everything (omitted owns key).
		{"monolith", "uaa", true},
		{"monolith", "interview", true},
		{"monolith", "anything-at-all", true},
		// web-svc owns nothing (explicit empty list).
		{"web-svc", "uaa", false},
		{"web-svc", "interview", false},
		{"web-svc", "anything-at-all", false},
		// uaa-svc owns the listed module only.
		{"uaa-svc", "uaa", true},
		{"uaa-svc", "interview", false},
		// Unknown deployment never owns anything.
		{"missing-svc", "uaa", false},
	}
	for _, tc := range cases {
		got := m.Owns(tc.dep, tc.mod)
		if got != tc.want {
			t.Errorf("Owns(%q, %q) = %v; want %v", tc.dep, tc.mod, got, tc.want)
		}
	}
}

// ── Overrides ─────────────────────────────────────────────────────

func ptrBool(b bool) *bool       { return &b }
func ptrString(s string) *string { return &s }

func TestApplyOverrides_ServiceExposeAsRename(t *testing.T) {
	// The classic OATS scenario: code declares DB_HOSTNAME but the
	// app actually reads POSTGRES_HOST. Operator override remaps the
	// env name without touching source.
	dm := &DeployManifest{
		Services: map[string]ServiceDeclSpec{
			"main": {
				Kind: "postgres",
				ExposeAs: map[string]string{
					"host":     "DB_HOSTNAME",
					"port":     "DB_PORT",
					"user":     "DB_USERNAME",
					"password": "DB_PASSWORD",
					"database": "DB_NAME",
				},
			},
		},
		Overrides: Overrides{
			Services: map[string]ServiceOverride{
				"main": {
					ExposeAs: map[string]string{
						"host":     "POSTGRES_HOST",
						"port":     "POSTGRES_PORT",
						"user":     "POSTGRES_USER",
						"password": "POSTGRES_PASSWORD",
						"database": "POSTGRES_DB",
					},
				},
			},
		},
	}
	applied, stale := dm.ApplyOverrides()
	if applied != 5 {
		t.Errorf("applied count: got %d, want 5", applied)
	}
	if len(stale) != 0 {
		t.Errorf("stale: got %v, want none", stale)
	}
	if got := dm.Services["main"].ExposeAs["host"]; got != "POSTGRES_HOST" {
		t.Errorf("host override: got %q", got)
	}
	if got := dm.Services["main"].ExposeAs["database"]; got != "POSTGRES_DB" {
		t.Errorf("database override: got %q", got)
	}
}

func TestApplyOverrides_ServicePartialMerge(t *testing.T) {
	// Override only one ExposeAs key; the others must stay
	// code-derived.
	dm := &DeployManifest{
		Services: map[string]ServiceDeclSpec{
			"main": {
				Kind:     "postgres",
				ExposeAs: map[string]string{"host": "DB_HOSTNAME", "port": "DB_PORT"},
			},
		},
		Overrides: Overrides{
			Services: map[string]ServiceOverride{
				"main": {ExposeAs: map[string]string{"host": "PG_HOST"}},
			},
		},
	}
	applied, _ := dm.ApplyOverrides()
	if applied != 1 {
		t.Errorf("applied: got %d, want 1", applied)
	}
	if dm.Services["main"].ExposeAs["host"] != "PG_HOST" {
		t.Errorf("host not overridden: %v", dm.Services["main"].ExposeAs)
	}
	if dm.Services["main"].ExposeAs["port"] != "DB_PORT" {
		t.Errorf("port should be untouched: %v", dm.Services["main"].ExposeAs)
	}
}

func TestApplyOverrides_KindSwap(t *testing.T) {
	dm := &DeployManifest{
		Services: map[string]ServiceDeclSpec{
			"events": {Kind: "rabbitmq"},
		},
		Overrides: Overrides{
			Services: map[string]ServiceOverride{
				"events": {Kind: "kafka"},
			},
		},
	}
	applied, _ := dm.ApplyOverrides()
	if applied != 1 || dm.Services["events"].Kind != "kafka" {
		t.Errorf("kind swap: applied=%d kind=%q", applied, dm.Services["events"].Kind)
	}
}

func TestApplyOverrides_EnvFlipRequiredFalse(t *testing.T) {
	// Pointer-based override: nil = no override, &false = explicit
	// "set to false". The bool zero-value being meaningful is why
	// EnvOverride uses pointer fields.
	dm := &DeployManifest{
		Env: map[string]EnvDeclSpec{
			"DB_NAME": {Required: true, BoundTo: "main.database"},
		},
		Overrides: Overrides{
			Env: map[string]EnvOverride{
				"DB_NAME": {Required: ptrBool(false), Default: ptrString("oats_db")},
			},
		},
	}
	applied, _ := dm.ApplyOverrides()
	if applied != 2 {
		t.Errorf("applied: got %d, want 2", applied)
	}
	if dm.Env["DB_NAME"].Required != false {
		t.Errorf("required: should be false, got true")
	}
	if dm.Env["DB_NAME"].Default != "oats_db" {
		t.Errorf("default: got %q, want %q", dm.Env["DB_NAME"].Default, "oats_db")
	}
	if dm.Env["DB_NAME"].BoundTo != "main.database" {
		t.Errorf("boundTo should be untouched: got %q", dm.Env["DB_NAME"].BoundTo)
	}
}

func TestApplyOverrides_StaleTargetReported(t *testing.T) {
	// Override targets a service that no longer exists in code (e.g.
	// the source-side declaration was renamed). Reconcile shouldn't
	// fail, but should surface the mismatch so the operator can
	// clean up.
	dm := &DeployManifest{
		Services: map[string]ServiceDeclSpec{
			"main": {Kind: "postgres"},
		},
		Overrides: Overrides{
			Services: map[string]ServiceOverride{
				"old-name": {Kind: "mysql"},
			},
			Env: map[string]EnvOverride{
				"GHOST_VAR": {Required: ptrBool(false)},
			},
		},
	}
	applied, stale := dm.ApplyOverrides()
	if applied != 0 {
		t.Errorf("applied: got %d, want 0", applied)
	}
	if len(stale) != 2 {
		t.Errorf("stale count: got %d (%v), want 2", len(stale), stale)
	}
}

func TestApplyOverrides_NoOpWhenValueAlreadyMatches(t *testing.T) {
	// If override matches code-derived value, the count of "applied"
	// stays 0 — useful so operators see signal only when overrides
	// actually do something. Lets a stale override that no longer
	// differs from code stay in the YAML without inflating the count.
	dm := &DeployManifest{
		Services: map[string]ServiceDeclSpec{
			"main": {Kind: "postgres", ExposeAs: map[string]string{"host": "DB_HOSTNAME"}},
		},
		Overrides: Overrides{
			Services: map[string]ServiceOverride{
				"main": {Kind: "postgres", ExposeAs: map[string]string{"host": "DB_HOSTNAME"}},
			},
		},
	}
	applied, _ := dm.ApplyOverrides()
	if applied != 0 {
		t.Errorf("applied: got %d, want 0 (override matched code)", applied)
	}
}
