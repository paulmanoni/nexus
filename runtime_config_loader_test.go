package nexus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadConfig_FullSchema: the headline integration. Every
// commonly-set field round-trips from TOML → Config without
// loss. If this fails, operators copying the schema sample
// from the loader's godoc will be confused.
func TestLoadConfig_FullSchema(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "nexus.toml")
	mustWriteTOML(t, path, `
[runtime]
environment = "production"
version = "1.2.3"
introspection = true
introspection_networks = ["127.0.0.0/8", "10.0.0.0/8"]
trace_capacity = 500

[runtime.server]
addr = ":8080"
route_prefix = "/api"

[runtime.server.listeners.public]
addr = ":8080"
scope = "public"

[runtime.server.listeners.admin]
addr = "127.0.0.1:7000"
scope = "admin"

[runtime.dashboard]
enabled = true
name = "Demo App"

[runtime.graphql]
path = "/graphql"
pretty = true
debug = false
disable_playground = false
document_cache_size = 2048

[runtime.middleware.cors]
allow_origins = ["https://app.example.com"]
allow_methods = ["GET", "POST", "DELETE"]
allow_credentials = true
max_age = "12h"

[runtime.middleware.ratelimit]
rpm = 600
burst = 50
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	// Spot-check scalars.
	if cfg.Environment != "production" {
		t.Errorf("Environment = %q, want production", cfg.Environment)
	}
	if cfg.Version != "1.2.3" {
		t.Errorf("Version = %q, want 1.2.3", cfg.Version)
	}
	if !cfg.Introspection {
		t.Errorf("Introspection should be true")
	}
	if cfg.TraceCapacity != 500 {
		t.Errorf("TraceCapacity = %d, want 500", cfg.TraceCapacity)
	}

	// Server.
	if cfg.Server.Addr != ":8080" {
		t.Errorf("Server.Addr = %q", cfg.Server.Addr)
	}
	if cfg.Server.RoutePrefix != "/api" {
		t.Errorf("Server.RoutePrefix = %q", cfg.Server.RoutePrefix)
	}
	if len(cfg.Server.Listeners) != 2 {
		t.Fatalf("Server.Listeners count = %d, want 2", len(cfg.Server.Listeners))
	}
	if cfg.Server.Listeners["admin"].Scope != ScopeAdmin {
		t.Errorf("admin listener scope = %v, want ScopeAdmin", cfg.Server.Listeners["admin"].Scope)
	}
	if cfg.Server.Listeners["public"].Addr != ":8080" {
		t.Errorf("public listener addr = %q", cfg.Server.Listeners["public"].Addr)
	}

	// Dashboard.
	if !cfg.Dashboard.Enabled {
		t.Errorf("Dashboard.Enabled should be true")
	}
	if cfg.Dashboard.Name != "Demo App" {
		t.Errorf("Dashboard.Name = %q", cfg.Dashboard.Name)
	}

	// GraphQL.
	if cfg.GraphQL.Path != "/graphql" {
		t.Errorf("GraphQL.Path = %q", cfg.GraphQL.Path)
	}
	if !cfg.GraphQL.Pretty {
		t.Errorf("GraphQL.Pretty should be true")
	}
	if cfg.GraphQL.DocumentCacheSize != 2048 {
		t.Errorf("GraphQL.DocumentCacheSize = %d", cfg.GraphQL.DocumentCacheSize)
	}

	// CORS.
	if cfg.Middleware.CORS == nil {
		t.Fatal("Middleware.CORS should be set")
	}
	if !cfg.Middleware.CORS.AllowCredentials {
		t.Errorf("CORS.AllowCredentials should be true")
	}
	if cfg.Middleware.CORS.MaxAge != 12*time.Hour {
		t.Errorf("CORS.MaxAge = %v", cfg.Middleware.CORS.MaxAge)
	}
	if got := cfg.Middleware.CORS.AllowOrigins; len(got) != 1 || got[0] != "https://app.example.com" {
		t.Errorf("CORS.AllowOrigins = %v", got)
	}

	// Rate limit.
	if cfg.Middleware.RateLimit.RPM != 600 {
		t.Errorf("RateLimit.RPM = %d, want 600", cfg.Middleware.RateLimit.RPM)
	}
	if cfg.Middleware.RateLimit.Burst != 50 {
		t.Errorf("RateLimit.Burst = %d", cfg.Middleware.RateLimit.Burst)
	}
}

// TestLoadConfig_MinimalDocument: a near-empty TOML returns
// a near-zero Config. Operators who want defaults everywhere
// should be able to ship a stub nexus.toml without per-field
// gymnastics.
func TestLoadConfig_MinimalDocument(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "nexus.toml")
	mustWriteTOML(t, path, `
[runtime]
environment = "development"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Environment != "development" {
		t.Errorf("Environment = %q", cfg.Environment)
	}
	// Server.Addr left empty so framework default applies at Run.
	if cfg.Server.Addr != "" {
		t.Errorf("Server.Addr should be unset, got %q", cfg.Server.Addr)
	}
	if cfg.Dashboard.Enabled {
		t.Errorf("Dashboard.Enabled should default to false")
	}
}

// TestLoadConfig_EnvVarExpansion: ${VAR} placeholders in
// string values get expanded the same way the deploy manifest
// already supports. Keeps the schema consistent + lets
// operators externalize secrets / per-env hostnames.
func TestLoadConfig_EnvVarExpansion(t *testing.T) {
	t.Setenv("ADMIN_PORT", "9000")
	t.Setenv("APP_NAME", "Expanded App")

	tmp := t.TempDir()
	path := filepath.Join(tmp, "nexus.toml")
	mustWriteTOML(t, path, `
[runtime]
[runtime.server]
addr = ":${ADMIN_PORT}"
[runtime.dashboard]
name = "${APP_NAME}"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server.Addr != ":9000" {
		t.Errorf("Server.Addr = %q, want :9000", cfg.Server.Addr)
	}
	if cfg.Dashboard.Name != "Expanded App" {
		t.Errorf("Dashboard.Name = %q", cfg.Dashboard.Name)
	}
}

// TestLoadConfig_MissingFile: a clean os-style ErrNotExist so
// operators can decide whether to fall back to defaults or
// fail loudly with their own message.
func TestLoadConfig_MissingFile(t *testing.T) {
	tmp := t.TempDir()
	_, err := LoadConfig(filepath.Join(tmp, "does-not-exist.toml"))
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist, got: %v", err)
	}
}

// TestLoadConfig_InvalidScope: an unknown listener scope value
// surfaces a clear error citing the offending listener name,
// not a generic "scope invalid" we'd have to grep for.
func TestLoadConfig_InvalidScope(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "nexus.toml")
	mustWriteTOML(t, path, `
[runtime.server.listeners.weird]
addr = ":1234"
scope = "private"
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid scope")
	}
	if !strings.Contains(err.Error(), "weird") {
		t.Errorf("error should name the offending listener, got: %v", err)
	}
}

// TestLoadConfig_InvalidMaxAge: bad duration string surfaces a
// clean error pointing at the field, not a low-level parse error.
func TestLoadConfig_InvalidMaxAge(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "nexus.toml")
	mustWriteTOML(t, path, `
[runtime.middleware.cors]
max_age = "not-a-duration"
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected duration parse error")
	}
	if !strings.Contains(err.Error(), "max_age") {
		t.Errorf("error should name max_age field, got: %v", err)
	}
}

// TestLoadConfig_GoOverrideAfterLoad: operators MUST be able
// to mutate the returned Config (e.g. Version from build
// ldflags). The doc promises this; lock it in.
func TestLoadConfig_GoOverrideAfterLoad(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "nexus.toml")
	mustWriteTOML(t, path, `
[runtime]
version = "from-toml"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Version = "from-ldflags"
	if cfg.Version != "from-ldflags" {
		t.Errorf("Go-side override didn't stick: %q", cfg.Version)
	}
}

func mustWriteTOML(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestUnknownConfigKeys_TypoInOwnedTable: the headline bug. A one-character
// typo under [runtime.server] used to be dropped without a word, leaving the
// app on the default port. It must now be named, located, and corrected.
func TestUnknownConfigKeys_TypoInOwnedTable(t *testing.T) {
	keys := unknownConfigKeys([]byte(`
[runtime.server]
adress = ":8099"
`))
	if len(keys) != 1 {
		t.Fatalf("want 1 unknown key, got %d: %+v", len(keys), keys)
	}
	k := keys[0]
	if got := k.Key(); got != "runtime.server.adress" {
		t.Errorf("Key() = %q, want runtime.server.adress", got)
	}
	if k.Line != 3 {
		t.Errorf("Line = %d, want 3", k.Line)
	}
	// `adress` is four edits from `addr`, too far to claim as a spelling
	// match — so the hint falls back to naming what the table does accept,
	// which puts `addr` in front of the operator either way.
	if !strings.Contains(k.Hint, "addr") {
		t.Errorf("Hint = %q, should point at addr", k.Hint)
	}
}

// TestUnknownConfigKeys_MisNestedTopLevelKey: the higher-value half — a real
// setting written at the wrong nesting level. The value reads correctly, so
// nothing but this check can tell the operator it does nothing.
func TestUnknownConfigKeys_MisNestedTopLevelKey(t *testing.T) {
	keys := unknownConfigKeys([]byte(`
addr = ":9001"

[runtime]
environment = "development"
`))
	if len(keys) != 1 {
		t.Fatalf("want 1 unknown key, got %d: %+v", len(keys), keys)
	}
	k := keys[0]
	if got := k.Key(); got != "addr" {
		t.Errorf("Key() = %q, want addr", got)
	}
	if want := "did you mean [runtime.server] addr?"; k.Hint != want {
		t.Errorf("Hint = %q, want %q", k.Hint, want)
	}
	if !strings.Contains(k.describe(), "top level") {
		t.Errorf("describe() should call out the nesting level, got %q", k.describe())
	}
}

// TestUnknownConfigKeys_CleanConfigSilent: every correctly-nested key across
// the runtime schema — including a named listener under the map-typed
// [runtime.server.listeners.*] table — must produce nothing. A check that
// cries wolf on a valid file is worse than no check.
func TestUnknownConfigKeys_CleanConfigSilent(t *testing.T) {
	keys := unknownConfigKeys([]byte(`
[runtime]
environment = "production"
introspection = true
trace_capacity = 500
sdk = true

[runtime.server]
addr = ":8080"
route_prefix = "/api"
max_body_bytes = 33554432

[runtime.server.listeners.admin]
addr = "127.0.0.1:7000"
scope = "admin"

[runtime.websocket]
allowed_origins = ["https://app.example.com"]

[runtime.dashboard]
enabled = true
name = "Demo"

[runtime.graphql]
path = "/graphql"

[runtime.middleware.cors]
allow_origins = ["*"]

[runtime.middleware.ratelimit]
rpm = 600
burst = 50

[runtime.middleware.security]
csrf = true
`))
	if len(keys) != 0 {
		t.Errorf("clean config must be silent, got %+v", keys)
	}
}

// TestUnknownConfigKeys_UnownedSectionsSilent: the noise floor. Tables owned
// by another loader (the deploy-manifest surface, sibling BindFromConfig
// blocks, extension blocks this binary may not even link) and an app's own
// nexus.Get sections — at the top level or under [runtime] — are all
// legitimate and must stay quiet.
func TestUnknownConfigKeys_UnownedSectionsSilent(t *testing.T) {
	keys := unknownConfigKeys([]byte(`
[runtime.server]
addr = ":8080"

# Another loader's schema — unknown keys inside are not ours to judge.
[databases.main]
driver = "postgres"
pool_size = 10

[extensions.config]
endpoint = "http://localhost:8078"

[env.client]
id = "myapp-web"

[deployments.production]
region = "af-south-1"

[peers.mesh]
addr = "10.0.0.1:7777"

[environments.staging]
domain = "staging.example.com"

# App-owned sections, readable via nexus.Get — a table is never "unknown".
[app]
name = "demo"

[runtime.logging]
format = "pretty"
requests = false
`))
	if len(keys) != 0 {
		t.Errorf("unowned + app-owned sections must be silent, got %+v", keys)
	}
}

// TestUnknownConfigKeys_BothHalvesOfTheReproduction: the empirical repro —
// a typo AND a mis-nested key in one file — reported together, in source
// order, each with its own diagnosis.
func TestUnknownConfigKeys_BothHalvesOfTheReproduction(t *testing.T) {
	keys := unknownConfigKeys([]byte(`
addr = ":9001"

[runtime.server]
adress = ":8099"
`))
	if len(keys) != 2 {
		t.Fatalf("want 2 unknown keys, got %d: %+v", len(keys), keys)
	}
	if keys[0].Key() != "addr" || keys[1].Key() != "runtime.server.adress" {
		t.Errorf("want source order [addr, runtime.server.adress], got [%s, %s]",
			keys[0].Key(), keys[1].Key())
	}
}

// TestLoadConfig_UnknownKeyIsNotFatal: severity contract. An unrecognized key
// must never break boot — existing apps carry blocks this binary has no
// decoder for — and the keys the loader DOES understand still load.
func TestLoadConfig_UnknownKeyIsNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nexus.toml")
	mustWriteTOML(t, path, `
addr = ":9001"

[runtime.server]
adress = ":8099"
addr = ":8085"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unknown keys must not fail the load: %v", err)
	}
	if cfg.Server.Addr != ":8085" {
		t.Errorf("Config.Server.Addr = %q, want :8085", cfg.Server.Addr)
	}
}

// TestWriteUnknownConfigKeyWarning_NamesKeyFileAndFix: the operator-facing
// message. It must carry the file:line, the key, and the correction —
// everything needed to fix the file without reading framework source.
func TestWriteUnknownConfigKeyWarning_NamesKeyFileAndFix(t *testing.T) {
	var buf strings.Builder
	writeUnknownConfigKeyWarning(&buf, "nexus.toml", []unknownConfigKey{
		{Path: []string{"addr"}, Line: 2, Hint: "did you mean [runtime.server] addr?"},
	})
	out := buf.String()
	for _, want := range []string{"nexus.toml:2", `"addr"`, "[runtime.server] addr?", "nexus lint nexus.toml"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning missing %q:\n%s", want, out)
		}
	}
}

// TestRuntimeConfigLeaves_IndexesTheSchema: the hint index is derived by
// reflection so it can't drift from the structs. Spot-check the shape:
// addr belongs to [runtime.server] and, via the map-typed table, to
// [runtime.server.listeners.*].
func TestRuntimeConfigLeaves_IndexesTheSchema(t *testing.T) {
	leaves := runtimeConfigLeaves()
	if got := leaves["addr"]; len(got) != 2 {
		t.Errorf("leaves[addr] = %v, want the two tables declaring addr", got)
	}
	if got := leaves["environment"]; len(got) != 1 || got[0] != "runtime" {
		t.Errorf("leaves[environment] = %v, want [runtime]", got)
	}
	if table, ok := bestLeafTable(leaves["addr"], ""); !ok || table != "runtime.server" {
		t.Errorf("bestLeafTable(addr) = %q/%v, want runtime.server", table, ok)
	}
}

// TestNearestName_BudgetScalesWithLength: a suggestion nobody asked for is
// worse than none, so short keys get a tighter edit budget than long ones.
func TestNearestName_BudgetScalesWithLength(t *testing.T) {
	cases := []struct {
		name, want string
		candidates []string
	}{
		{"addrr", "addr", []string{"addr", "route_prefix"}},            // 1 edit
		{"enabeld", "enabled", []string{"enabled", "name"}},            // transposition, 2 edits
		{"xyz", "", []string{"rpm", "burst"}},                          // 3 chars: nothing within 1
		{"adress", "", []string{"addr", "route_prefix"}},               // 4 edits from addr — not a spelling match
		{"completely_different", "", []string{"addr", "route_prefix"}}, // nothing close
	}
	for _, c := range cases {
		if got := nearestName(c.name, c.candidates); got != c.want {
			t.Errorf("nearestName(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}
