package nexus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/manifest"
)

// TestLoadDeployManifest_PopulatesDeclarations verifies that loading
// a TOML file pushes its inputs into the App's manifest store via
// the Declare* methods, then resolves into the effective manifest at
// boot. End-to-end smoke from "TOML on disk" → "boot validation
// fails per the declared inputs."
func TestLoadDeployManifest_PopulatesDeclarations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus.toml")
	contents := []byte(`
[environments.production]
domain = "app.example.com"

[secrets.API_KEY]
required   = true
env_scoped = true
`)
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}

	app := New(config.Runtime{
		Environment: "production",
		Server:      config.Server{Addr: "127.0.0.1:0"},
	})
	if err := app.LoadDeployManifest(path); err != nil {
		t.Fatalf("LoadDeployManifest: %v", err)
	}

	// API_KEY is required but unset; resolver should fail with that
	// name in the error message — proof the TOML's `required = true`
	// flowed through to the validator.
	err := app.resolveEffectiveManifest()
	if err == nil {
		t.Fatal("expected resolveEffectiveManifest to fail without API_KEY set")
	}
	if !strings.Contains(err.Error(), "API_KEY") {
		t.Errorf("error should name API_KEY: %v", err)
	}
}

func TestLoadDeployManifest_MissingFile(t *testing.T) {
	app := New(config.Runtime{
		Environment: "production",
		Server:      config.Server{Addr: "127.0.0.1:0"},
	})
	if err := app.LoadDeployManifest("/does/not/exist.toml"); err == nil {
		t.Fatal("expected error on missing file")
	}
}

func TestLoadDeployManifest_OverrideFlowsThroughToMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus.toml")
	contents := []byte(`
[environments.production]

[environment_overrides.production]
env = { LOG_LEVEL = "warn" }
`)
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}

	app := New(config.Runtime{
		Environment: "production",
		Server:      config.Server{Addr: "127.0.0.1:0"},
	})
	// Realistic flow: Go declares the schema, TOML supplies the per-
	// env values. The override should turn LOG_LEVEL's default from
	// "info" into "warn".
	app.DeclareEnv(manifest.EnvVar{Name: "LOG_LEVEL", Default: "info"})

	if err := app.LoadDeployManifest(path); err != nil {
		t.Fatalf("LoadDeployManifest: %v", err)
	}
	if err := app.resolveEffectiveManifest(); err != nil {
		t.Fatalf("resolveEffectiveManifest: %v", err)
	}
	eff := app.EffectiveManifest()
	if eff == nil {
		t.Fatal("EffectiveManifest nil after resolve")
	}
	for _, e := range eff.Env {
		if e.Name == "LOG_LEVEL" {
			if e.Default != "warn" {
				t.Errorf("LOG_LEVEL.Default: got %q, want warn", e.Default)
			}
			if e.Source != "override" {
				t.Errorf("LOG_LEVEL.Source: got %q, want override", e.Source)
			}
			return
		}
	}
	t.Fatal("LOG_LEVEL not in effective manifest")
}

func TestLoadDeployManifest_HooksAndSecretsLanded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nexus.toml")
	contents := []byte(`
[environments.production]

[secrets.JWT_SIGNING_KEY]
description = "HS256 key"
required    = true

[hooks]
build = ["go build ./..."]
`)
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("JWT_SIGNING_KEY", "x") // pass the required check
	app := New(config.Runtime{
		Environment: "production",
		Server:      config.Server{Addr: "127.0.0.1:0"},
	})
	if err := app.LoadDeployManifest(path); err != nil {
		t.Fatalf("LoadDeployManifest: %v", err)
	}
	if err := app.resolveEffectiveManifest(); err != nil {
		t.Fatalf("resolveEffectiveManifest: %v", err)
	}
	eff := app.EffectiveManifest()
	if eff == nil {
		t.Fatal("EffectiveManifest nil after resolve")
	}
	if len(eff.Secrets) != 1 || eff.Secrets[0].Name != "JWT_SIGNING_KEY" {
		t.Errorf("Secrets: %+v", eff.Secrets)
	}
	if eff.Hooks == nil {
		t.Fatal("Hooks nil")
	}
	if len(eff.Hooks.Build) != 1 || eff.Hooks.Build[0] != "go build ./..." {
		t.Errorf("Hooks.Build: %+v", eff.Hooks.Build)
	}
}

const tomlWithDatabases = `
[runtime.server]
addr = ":8080"

[databases.uaa]
driver     = "postgres"
key_prefix = "db.uaa"
sslmode    = "disable"
timezone   = "Africa/Dar_es_Salaam"
schema     = "main"

[databases.oats]
driver     = "postgres"
key_prefix = "db.oats"
default    = true

[databases.ajira]
driver     = "mysql"
key_prefix = "db.ajira_db"
`

// TestLoadConfig_ParsesDatabaseSpecs verifies the loader parses every
// [databases.*] block into the spec registry that db.BindFromConfig reads
// back via DatabaseSpecFor. The binders themselves moved to package db;
// the core's job is just to parse and store the specs.
func TestLoadConfig_ParsesDatabaseSpecs(t *testing.T) {
	if _, err := config.Load(writeTOML(t, tomlWithDatabases)); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	uaa, ok := config.DatabaseSpecFor("uaa")
	if !ok {
		t.Fatal("uaa spec not registered")
	}
	if uaa.Driver != "postgres" || uaa.KeyPrefix != "db.uaa" || uaa.SSLMode != "disable" ||
		uaa.TimeZone != "Africa/Dar_es_Salaam" || uaa.Schema != "main" {
		t.Errorf("uaa spec wrong: %+v", uaa)
	}
	if oats, _ := config.DatabaseSpecFor("oats"); !oats.Default {
		t.Error("oats should be default")
	}
	if ajira, _ := config.DatabaseSpecFor("ajira"); ajira.Driver != "mysql" {
		t.Errorf("ajira driver = %q", ajira.Driver)
	}
}
