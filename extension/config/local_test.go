package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/paulmanoni/nexus"
)

// resetStore wraps nexus.ClearConfigStoreForTest so each test
// starts from a clean slate (and tests don't observe the
// "second installer panics" guard from a prior test's setup).
func resetStore(t *testing.T) {
	t.Helper()
	nexus.ClearConfigStoreForTest()
	t.Cleanup(nexus.ClearConfigStoreForTest)
}

// TestLocal_ReadsPlaintextAndPopulatesGet drives the headline
// path: a plaintext TOML on disk → values reachable via
// nexus.Get. config.Local leaves the file alone — operators
// expect their TOML to stay readable + editable.
func TestLocal_ReadsPlaintextAndPopulatesGet(t *testing.T) {
	resetStore(t)
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "nexus.config.toml")
	plaintext := `[profiles.default]
app_name = "test-app"
port     = 8080
enabled  = true

[profiles.default.api]
timeout = "5s"
`
	if err := os.WriteFile(tomlPath, []byte(plaintext), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := initLocal(localConfig{path: tomlPath, profile: "default"}); err != nil {
		t.Fatal(err)
	}
	// Plaintext file MUST still be on disk in its original form
	// — config.Local doesn't seal, doesn't move, doesn't touch.
	body, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatalf("toml file disappeared after initLocal: %v", err)
	}
	if string(body) != plaintext {
		t.Errorf("toml file was modified by initLocal:\n  want: %q\n   got: %q", plaintext, string(body))
	}

	// And nexus.Get works across types.
	if got := nexus.Get[string]("app_name"); got != "test-app" {
		t.Errorf("Get[string](app_name) = %q, want test-app", got)
	}
	if got := nexus.Get[int]("port"); got != 8080 {
		t.Errorf("Get[int](port) = %d, want 8080", got)
	}
	if got := nexus.Get[bool]("enabled"); !got {
		t.Errorf("Get[bool](enabled) = false, want true")
	}
}

// TestLocal_ProfileOverlay proves the profile-merge inside
// Local: a `prod` request gets default + prod overlaid, with
// prod's leaf values winning where both sides set a key.
func TestLocal_ProfileOverlay(t *testing.T) {
	resetStore(t)
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "nexus.config.toml")
	plaintext := `[profiles.default]
timeout = "5s"
[profiles.default.features]
new_path = false

[profiles.prod.features]
new_path = true
`
	if err := os.WriteFile(tomlPath, []byte(plaintext), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := initLocal(localConfig{path: tomlPath, profile: "prod"}); err != nil {
		t.Fatal(err)
	}
	// default value passes through unchanged
	if got := nexus.Get[string]("timeout"); got != "5s" {
		t.Errorf("timeout = %q, want 5s (from default)", got)
	}
	// prod overlay wins
	if got := nexus.Get[bool]("features.new_path"); !got {
		t.Errorf("features.new_path = false, want true (prod overlay)")
	}
}

// TestLocal_MissingFile proves the error path: a config.Local
// pointed at a nonexistent path fails boot loudly. Catches the
// canonical "I forgot to copy the file" deploy mistake at
// startup rather than at first nexus.Get call site.
func TestLocal_MissingFile(t *testing.T) {
	resetStore(t)
	err := initLocal(localConfig{path: "/nonexistent/nexus.config.toml", profile: "default"})
	if err == nil {
		t.Fatal("initLocal should error on missing file")
	}
}

// TestGet_WithDefault proves the optional-default contract:
// missing key + default = default; missing key + no default =
// zero value. Same semantics regardless of whether the missing-
// ness was from the store or from a typing conversion failure.
func TestGet_WithDefault(t *testing.T) {
	resetStore(t)
	nexus.InstallConfigStore(map[string]any{"present": "value"}, "test")

	// Missing key, with default
	if got := nexus.Get[int]("missing", 99); got != 99 {
		t.Errorf("missing with default = %d, want 99", got)
	}
	// Missing key, no default → zero
	if got := nexus.Get[int]("missing"); got != 0 {
		t.Errorf("missing no default = %d, want 0", got)
	}
	// Present key, default ignored
	if got := nexus.Get[string]("present", "fallback"); got != "value" {
		t.Errorf("present with default = %q, want value (default ignored)", got)
	}
}

// TestGet_EnvOverride proves the ENV-wins priority. An ENV
// var matching the dotted key (UPPERCASE_WITH_UNDERSCORES)
// overrides the snapshot.
func TestGet_EnvOverride(t *testing.T) {
	resetStore(t)
	nexus.InstallConfigStore(map[string]any{
		"api": map[string]any{"timeout": "5s"},
	}, "test")
	t.Setenv("API_TIMEOUT", "30s")
	if got := nexus.Get[string]("api.timeout"); got != "30s" {
		t.Errorf("with ENV override = %q, want 30s", got)
	}
}

// TestMustGet_PanicsOnMissing proves MustGet's strict contract.
// Use for keys whose absence is a boot bug, not a runtime
// condition; the panic is desired (loud, traceable).
func TestMustGet_PanicsOnMissing(t *testing.T) {
	resetStore(t)
	nexus.InstallConfigStore(map[string]any{}, "test")

	defer func() {
		if r := recover(); r == nil {
			t.Error("MustGet on missing key should panic")
		}
	}()
	_ = nexus.MustGet[string]("never_set")
}

// TestBindConfig_PopulatesStruct proves the typed struct-bind
// path: a subtree → typed Go struct via JSON. Equivalent to
// Spring's @ConfigurationProperties.
func TestBindConfig_PopulatesStruct(t *testing.T) {
	resetStore(t)
	nexus.InstallConfigStore(map[string]any{
		"payment": map[string]any{
			"provider":    "stripe",
			"max_retries": 3,
		},
	}, "test")
	type PaymentConfig struct {
		Provider   string `json:"provider"`
		MaxRetries int    `json:"max_retries"`
	}
	var pc PaymentConfig
	if err := nexus.BindConfig("payment", &pc); err != nil {
		t.Fatal(err)
	}
	if pc.Provider != "stripe" || pc.MaxRetries != 3 {
		t.Errorf("BindConfig = %+v, want {stripe, 3}", pc)
	}
}

// TestOnConfigChange_FiresOnChange proves the hot-reload
// callback path. Subscribe; trigger a snapshot update via
// UpdateConfigStore; assert the callback fired with the new
// value.
func TestOnConfigChange_FiresOnChange(t *testing.T) {
	resetStore(t)
	nexus.InstallConfigStore(map[string]any{"flag": false}, "v1")

	var got any
	called := make(chan struct{}, 1)
	nexus.OnConfigChange("flag", func(v any) {
		got = v
		called <- struct{}{}
	})

	nexus.UpdateConfigStore(map[string]any{"flag": true}, "v2")

	<-called
	if b, ok := got.(bool); !ok || !b {
		t.Errorf("OnConfigChange got %v, want true", got)
	}
}

// TestLocal_ExpandsEnvVarPlaceholders verifies that
// ${VAR}/${VAR:default} placeholders inside a config.toml file
// resolve the same way they do inside nexus.toml. Keeps the
// two TOML surfaces consistent — operators learn one rule.
func TestLocal_ExpandsEnvVarPlaceholders(t *testing.T) {
	resetStore(t)
	t.Setenv("CFG_DSN_HOST", "db.prod.example.com")

	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "nexus.config.toml")
	plaintext := `[profiles.default.db]
dsn      = "postgres://${CFG_DSN_HOST}:5432/app"
timeout  = "${CFG_DSN_TIMEOUT:5s}"
`
	if err := os.WriteFile(tomlPath, []byte(plaintext), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := initLocal(localConfig{path: tomlPath, profile: "default"}); err != nil {
		t.Fatal(err)
	}
	if got := nexus.Get[string]("db.dsn"); got != "postgres://db.prod.example.com:5432/app" {
		t.Errorf("db.dsn: got %q", got)
	}
	if got := nexus.Get[string]("db.timeout"); got != "5s" {
		t.Errorf("db.timeout default: got %q", got)
	}
}

// TestLocal_RejectsUndefinedEnvVar matches the nexus.toml
// strict-mode behavior: an undefined ${VAR} without a default
// fails the load loud rather than silently producing an empty
// value that breaks downstream Get() calls.
func TestLocal_RejectsUndefinedEnvVar(t *testing.T) {
	resetStore(t)
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "nexus.config.toml")
	plaintext := `[profiles.default]
api_key = "${CFG_UNDEFINED_KEY}"
`
	if err := os.WriteFile(tomlPath, []byte(plaintext), 0o644); err != nil {
		t.Fatal(err)
	}
	err := initLocal(localConfig{path: tomlPath, profile: "default"})
	if err == nil {
		t.Fatal("expected error on undefined env var without default")
	}
}
