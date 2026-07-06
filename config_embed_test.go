package nexus

import (
	"encoding/base64"
	"path/filepath"
	"testing"
)

// TestEmbeddedConfig_DecodeRoundTrip: the linker-injected base64 decodes
// back to the original bytes; an empty/garbage stamp reports "absent".
func TestEmbeddedConfig_DecodeRoundTrip(t *testing.T) {
	orig := embeddedConfigB64
	t.Cleanup(func() { embeddedConfigB64 = orig })

	// Absent by default.
	embeddedConfigB64 = ""
	if _, ok := embeddedConfig(); ok {
		t.Fatal("empty stamp should report absent")
	}

	// Garbage → absent, not a panic.
	embeddedConfigB64 = "not-valid-base64!!!"
	if _, ok := embeddedConfig(); ok {
		t.Fatal("malformed stamp should report absent")
	}

	// Round-trips.
	want := "[runtime.server]\naddr = \":9797\"\n"
	embeddedConfigB64 = base64.StdEncoding.EncodeToString([]byte(want))
	got, ok := embeddedConfig()
	if !ok || string(got) != want {
		t.Fatalf("embeddedConfig() = %q, %v; want %q, true", got, ok, want)
	}
}

// TestAutoLoad_FallsBackToEmbedded: when no nexus.toml exists on disk,
// Boot's autoLoad uses the build-time embedded copy — so a single
// self-contained binary binds its configured addr instead of :8080.
func TestAutoLoad_FallsBackToEmbedded(t *testing.T) {
	ClearConfigStoreForTest()
	t.Cleanup(ClearConfigStoreForTest)

	orig := embeddedConfigB64
	t.Cleanup(func() { embeddedConfigB64 = orig })
	embeddedConfigB64 = base64.StdEncoding.EncodeToString([]byte(`
[runtime.server]
addr = ":9797"

[app]
name = "embedded-demo"
`))

	// Point at a path that does not exist so the disk read misses and the
	// embedded copy takes over.
	cfg, _ := autoLoad(filepath.Join(t.TempDir(), "absent.toml"))
	if cfg.Server.Addr != ":9797" {
		t.Errorf("embedded fallback Config.Server.Addr = %q, want :9797", cfg.Server.Addr)
	}
	if got := Get[string]("app.name"); got != "embedded-demo" {
		t.Errorf("embedded fallback should seed base layer: Get(app.name) = %q", got)
	}
}

// TestAutoLoad_DiskWinsOverEmbedded: an on-disk nexus.toml overrides the
// embedded copy, so operators can re-tune a deployed binary without a
// rebuild.
func TestAutoLoad_DiskWinsOverEmbedded(t *testing.T) {
	ClearConfigStoreForTest()
	t.Cleanup(ClearConfigStoreForTest)

	orig := embeddedConfigB64
	t.Cleanup(func() { embeddedConfigB64 = orig })
	embeddedConfigB64 = base64.StdEncoding.EncodeToString(
		[]byte("[runtime.server]\naddr = \":9797\"\n"))

	path := filepath.Join(t.TempDir(), "nexus.toml")
	mustWriteTOML(t, path, "[runtime.server]\naddr = \":1234\"\n")

	cfg, _ := autoLoad(path)
	if cfg.Server.Addr != ":1234" {
		t.Errorf("disk config should win: Config.Server.Addr = %q, want :1234", cfg.Server.Addr)
	}
}
