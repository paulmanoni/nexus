package manifest

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadInputsYAML_FullSchema(t *testing.T) {
	yaml := []byte(`
environments:
  production:
    domain: app.example.com
    autoscale: { min: 2, max: 10 }
  staging:
    domain: staging.example.com
    ttl: 7d

secrets:
  JWT_SIGNING_KEY:
    description: HS256 signing key
    required: true
    env_scoped: true
    validation:
      length: { min: 32 }
  STRIPE_API_KEY:
    required: true
    env_scoped: true

files:
  tls_bundle:
    path: /etc/ssl/app/bundle.pem
    mode: 0400
    secret: true

hooks:
  build: [go build ./..., go test ./...]
  predeploy: [./bin/app migrate]

environment_overrides:
  production:
    env: { LOG_LEVEL: warn }
    services:
      main_db: { size: large, backup: hourly }
  staging:
    env: { LOG_LEVEL: debug }
`)
	m, err := LoadInputsYAML(yaml)
	if err != nil {
		t.Fatalf("LoadInputsYAML: %v", err)
	}

	// Environments sorted by name.
	if len(m.Environments) != 2 {
		t.Fatalf("Environments len: got %d, want 2", len(m.Environments))
	}
	if m.Environments[0].Name != "production" || m.Environments[1].Name != "staging" {
		t.Errorf("Environments order: %+v", m.Environments)
	}
	if m.Environments[0].Domain != "app.example.com" {
		t.Errorf("production.domain: got %q", m.Environments[0].Domain)
	}
	if a := m.Environments[0].Autoscale; a == nil || a.Min != 2 || a.Max != 10 {
		t.Errorf("autoscale: got %+v", a)
	}
	if m.Environments[1].TTL != "7d" {
		t.Errorf("staging.ttl: got %q", m.Environments[1].TTL)
	}

	// Secrets sorted by name, map key → Name field.
	if len(m.Secrets) != 2 {
		t.Fatalf("Secrets len: got %d", len(m.Secrets))
	}
	if m.Secrets[0].Name != "JWT_SIGNING_KEY" || m.Secrets[1].Name != "STRIPE_API_KEY" {
		t.Errorf("Secrets order: %+v", m.Secrets)
	}
	if !m.Secrets[0].Required || !m.Secrets[0].EnvScoped {
		t.Errorf("JWT_SIGNING_KEY flags: %+v", m.Secrets[0])
	}
	if m.Secrets[0].Validation == nil || m.Secrets[0].Validation.Length == nil || *m.Secrets[0].Validation.Length.Min != 32 {
		t.Errorf("JWT_SIGNING_KEY validation: %+v", m.Secrets[0].Validation)
	}

	// Files.
	if len(m.Files) != 1 || m.Files[0].Name != "tls_bundle" {
		t.Errorf("Files: %+v", m.Files)
	}
	if m.Files[0].Mode != 0400 {
		t.Errorf("tls_bundle.mode: got %o, want 0400", m.Files[0].Mode)
	}
	if !m.Files[0].Secret {
		t.Error("tls_bundle.secret should be true")
	}

	// Hooks.
	if m.Hooks == nil {
		t.Fatal("Hooks should not be nil")
	}
	if !reflect.DeepEqual(m.Hooks.Build, []string{"go build ./...", "go test ./..."}) {
		t.Errorf("Hooks.Build: %+v", m.Hooks.Build)
	}
	if !reflect.DeepEqual(m.Hooks.Predeploy, []string{"./bin/app migrate"}) {
		t.Errorf("Hooks.Predeploy: %+v", m.Hooks.Predeploy)
	}

	// Overrides: production has env + services patches.
	if len(m.Overrides) != 2 {
		t.Fatalf("Overrides len: got %d", len(m.Overrides))
	}
	prod := m.Overrides["production"]
	if prod.Env["LOG_LEVEL"] != "warn" {
		t.Errorf("production.env.LOG_LEVEL: got %q", prod.Env["LOG_LEVEL"])
	}
	svc, ok := prod.Services["main_db"]
	if !ok {
		t.Fatal("production.services.main_db missing")
	}
	if svc.Size == nil || *svc.Size != "large" {
		t.Errorf("main_db.size: got %v", svc.Size)
	}
	if svc.Backup == nil || *svc.Backup != "hourly" {
		t.Errorf("main_db.backup: got %v", svc.Backup)
	}

	// Staging.
	staging := m.Overrides["staging"]
	if staging.Env["LOG_LEVEL"] != "debug" {
		t.Errorf("staging.env.LOG_LEVEL: got %q", staging.Env["LOG_LEVEL"])
	}
}

func TestLoadInputsYAML_EmptyDocument(t *testing.T) {
	m, err := LoadInputsYAML([]byte(""))
	if err != nil {
		t.Fatalf("empty YAML should parse without error: %v", err)
	}
	if len(m.Environments) != 0 || len(m.Secrets) != 0 || len(m.Files) != 0 {
		t.Errorf("empty manifest should have empty slices, got %+v", m)
	}
	if m.Hooks != nil || m.Overrides != nil {
		t.Errorf("empty manifest should have nil Hooks/Overrides")
	}
}

func TestLoadInputsYAML_IgnoresUnknownTopLevelKeys(t *testing.T) {
	// Existing nexus.deploy.yaml files have `deployments:` and `peers:`
	// at top level. The inputs loader must ignore them, not error.
	yaml := []byte(`
deployments:
  monolith:
    port: 8080
peers:
  users-svc:
    timeout: 2s
environments:
  production: { domain: app.example.com }
`)
	m, err := LoadInputsYAML(yaml)
	if err != nil {
		t.Fatalf("LoadInputsYAML: %v", err)
	}
	if len(m.Environments) != 1 || m.Environments[0].Name != "production" {
		t.Errorf("environments not loaded alongside deployments/peers: %+v", m.Environments)
	}
}

func TestLoadInputsYAML_MalformedYAMLRejected(t *testing.T) {
	_, err := LoadInputsYAML([]byte("environments: {{ bad"))
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLoadInputsYAML_RoundTripsThroughLintAndMerge(t *testing.T) {
	yaml := []byte(`
environments:
  production: {}
  staging: {}
env:
secrets:
  API_KEY: { required: true }
environment_overrides:
  production:
    secret_specs:
      API_KEY: { env_scoped: true }
`)
	// Note: `env:` is empty (no entries) — should not crash. The
	// override's secret_specs uses snake_case (yaml tag).
	m, err := LoadInputsYAML(yaml)
	if err != nil {
		t.Fatalf("LoadInputsYAML: %v", err)
	}

	// Lint should report no issues (the manifest is structurally clean
	// — API_KEY is required and that's a warning ONLY if it lacks both
	// default and bound_to; secrets don't have those fields, so the
	// warning rule doesn't apply).
	issues := Lint(m)
	for _, i := range issues {
		if i.Severity == SeverityError {
			t.Errorf("unexpected error: %+v", i)
		}
	}

	// MergeOverrides should apply the secret spec diff cleanly.
	effective, err := MergeOverrides(m, "production")
	if err != nil {
		t.Fatalf("MergeOverrides: %v", err)
	}
	idx, ok := findSecretIndex(effective.Secrets, "API_KEY")
	if !ok {
		t.Fatal("API_KEY missing from effective manifest")
	}
	if !effective.Secrets[idx].EnvScoped {
		t.Error("API_KEY.EnvScoped should be true after override")
	}
}

func TestLoadInputsYAMLFile_ReadsFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus.deploy.yaml")
	contents := []byte(`
environments:
  production: { domain: app.example.com }
`)
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadInputsYAMLFile(path)
	if err != nil {
		t.Fatalf("LoadInputsYAMLFile: %v", err)
	}
	if len(m.Environments) != 1 {
		t.Fatalf("environments: %+v", m.Environments)
	}
}

func TestLoadInputsYAMLFile_MissingFile(t *testing.T) {
	_, err := LoadInputsYAMLFile("/does/not/exist.yaml")
	if err == nil {
		t.Fatal("expected error on missing file")
	}
}
