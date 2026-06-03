package nexus

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTOML is defined in database_toml_test.go (same package).

func TestEnvVars_FlattensDottedNames(t *testing.T) {
	p := writeTOML(t, `
[runtime]
environment = "development"

[env.client]
id     = "ajira_portal-web"
secret = "change-me-in-prod"

[env]
region = "eu"
`)
	got, err := EnvVars(p)
	if err != nil {
		t.Fatalf("EnvVars: %v", err)
	}
	want := map[string]string{
		"client.id":     "ajira_portal-web",
		"client.secret": "change-me-in-prod",
		"region":        "eu",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%q = %q, want %q", k, got[k], v)
		}
	}
}

func TestEnvVars_ExpandsPlaceholders(t *testing.T) {
	t.Setenv("REAL_SECRET", "s3cr3t")
	p := writeTOML(t, `
[env.client]
id     = "web"
secret = "${REAL_SECRET}"
`)
	got, err := EnvVars(p)
	if err != nil {
		t.Fatalf("EnvVars: %v", err)
	}
	if got["client.secret"] != "s3cr3t" {
		t.Errorf("client.secret = %q, want expanded s3cr3t", got["client.secret"])
	}
}

func TestLoadConfig_PublishesEnvVars(t *testing.T) {
	p := writeTOML(t, `
[runtime]
environment = "development"

[env.client]
id     = "ajira_portal-web"
secret = "change-me-in-prod"
`)
	// Sanity: not set before.
	_ = os.Unsetenv("client.id")
	if _, err := LoadConfig(p); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := os.Getenv("client.id"); got != "ajira_portal-web" {
		t.Errorf("os.Getenv(client.id) = %q, want ajira_portal-web", got)
	}
	if got := os.Getenv("client.secret"); got != "change-me-in-prod" {
		t.Errorf("os.Getenv(client.secret) = %q, want change-me-in-prod", got)
	}
	// Cleanup so the dotted vars don't leak into other tests.
	_ = os.Unsetenv("client.id")
	_ = os.Unsetenv("client.secret")
}

func TestEnvVars_MissingFileIsNil(t *testing.T) {
	got, err := EnvVars(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil || got != nil {
		t.Errorf("missing file: got (%v, %v), want (nil, nil)", got, err)
	}
}
