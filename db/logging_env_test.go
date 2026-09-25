package db

import "testing"

// NEXUS_ENVIRONMENT overrides a development nexus.toml for SQL logging too,
// as it does for App.Environment. RUNTIME_ENVIRONMENT stands in for the
// file's [runtime] environment (nexus.Get's env-override layer).
func TestDevMode_NexusEnvironmentOverrides(t *testing.T) {
	t.Setenv("NEXUS_DEV", "")
	t.Setenv("RUNTIME_ENVIRONMENT", "development")
	t.Setenv("NEXUS_ENVIRONMENT", "")
	if !devMode() {
		t.Fatal("precondition: environment = development should be dev mode")
	}
	t.Setenv("NEXUS_ENVIRONMENT", "production")
	if devMode() {
		t.Error("NEXUS_ENVIRONMENT=production must override a development nexus.toml")
	}
}
