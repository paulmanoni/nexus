package nexus

import (
	"path/filepath"
	"testing"
)

// TestGet_ReadsNexusToml: LoadConfig seeds the nexus.Get base layer
// from the full document, so a key declared in nexus.toml resolves
// via nexus.Get even with no config extension wired. This is the
// headline of the "nexus.toml is fully automatic" change — the dotted
// key mirrors the TOML table path.
func TestGet_ReadsNexusToml(t *testing.T) {
	ClearConfigStoreForTest()
	t.Cleanup(ClearConfigStoreForTest)

	path := filepath.Join(t.TempDir(), "nexus.toml")
	mustWriteTOML(t, path, `
[runtime.storage]
dir = "media"
url = "/media"

[storage]
quota = 42
`)
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if got := Get[string]("runtime.storage.url"); got != "/media" {
		t.Errorf("Get(runtime.storage.url) = %q, want %q", got, "/media")
	}
	if got := Get[string]("runtime.storage.dir"); got != "media" {
		t.Errorf("Get(runtime.storage.dir) = %q, want %q", got, "media")
	}
	// Top-level table + int conversion through the snapshot path.
	if got := Get[int]("storage.quota"); got != 42 {
		t.Errorf("Get(storage.quota) = %d, want 42", got)
	}
	// Absent key still returns the supplied default.
	if got := Get[string]("storage.missing", "fallback"); got != "fallback" {
		t.Errorf("Get(storage.missing) = %q, want fallback", got)
	}
}

// TestGet_ExtensionStoreOverridesBase: when both the config-extension
// store and the nexus.toml base layer carry a key, the extension store
// wins (it's runtime-managed/hot-reloadable). A key only the base layer
// has still resolves — the extension store need not be exhaustive.
func TestGet_ExtensionStoreOverridesBase(t *testing.T) {
	ClearConfigStoreForTest()
	t.Cleanup(ClearConfigStoreForTest)

	path := filepath.Join(t.TempDir(), "nexus.toml")
	mustWriteTOML(t, path, `
[feature]
flag = "from-toml"
only_in_toml = "base"
`)
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// Extension store carries "feature.flag" but NOT "feature.only_in_toml".
	InstallConfigStore(map[string]any{
		"feature": map[string]any{"flag": "from-extension"},
	}, "ext")

	if got := Get[string]("feature.flag"); got != "from-extension" {
		t.Errorf("extension should override base: got %q", got)
	}
	if got := Get[string]("feature.only_in_toml"); got != "base" {
		t.Errorf("base layer should fill keys the extension store lacks: got %q", got)
	}
}

// TestGet_EnvOverridesToml: an ENV override outranks the nexus.toml
// base layer (storage.url → STORAGE_URL).
func TestGet_EnvOverridesToml(t *testing.T) {
	ClearConfigStoreForTest()
	t.Cleanup(ClearConfigStoreForTest)

	path := filepath.Join(t.TempDir(), "nexus.toml")
	mustWriteTOML(t, path, `
[storage]
url = "/from-toml"
`)
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	t.Setenv("STORAGE_URL", "/from-env")

	if got := Get[string]("storage.url"); got != "/from-env" {
		t.Errorf("ENV should override base: got %q", got)
	}
}

// TestAutoLoad_MissingFileTolerated: Boot's loader returns a zero
// Config and no extension options when nexus.toml is absent, so an app
// without one still boots rather than panicking.
func TestAutoLoad_MissingFileTolerated(t *testing.T) {
	ClearConfigStoreForTest()
	t.Cleanup(ClearConfigStoreForTest)

	cfg, extOpts := autoLoad(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if cfg.Server.Addr != "" || cfg.Environment != "" {
		t.Errorf("missing file should yield zero Config, got %+v", cfg)
	}
	if extOpts != nil {
		t.Errorf("missing file should yield no extension options, got %d", len(extOpts))
	}
}

// TestAutoLoad_ReadsRuntimeAndSeedsBase: the happy path used by Boot —
// autoLoad populates Config from [runtime] and seeds the base layer so
// nexus.Get works immediately afterward.
func TestAutoLoad_ReadsRuntimeAndSeedsBase(t *testing.T) {
	ClearConfigStoreForTest()
	t.Cleanup(ClearConfigStoreForTest)

	path := filepath.Join(t.TempDir(), "nexus.toml")
	mustWriteTOML(t, path, `
[runtime.server]
addr = ":9090"

[app]
name = "demo"
`)
	cfg, _ := autoLoad(path)
	if cfg.Server.Addr != ":9090" {
		t.Errorf("autoLoad Config.Server.Addr = %q, want :9090", cfg.Server.Addr)
	}
	if got := Get[string]("app.name"); got != "demo" {
		t.Errorf("autoLoad should seed base layer: Get(app.name) = %q", got)
	}
}
