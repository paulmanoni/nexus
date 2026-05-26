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
