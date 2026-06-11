package db

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/paulmanoni/nexus"
)

// testDBHandle is the kind of one-line marker type users declare.
type testDBHandle struct{ *Manager }

func TestEmbeddedManagerField(t *testing.T) {
	type good struct{ *Manager }
	type alsoGood struct {
		name string
		*Manager
	}
	type namedNotEmbedded struct{ M *Manager }
	type noManager struct{ X int }

	mustPanic := func(name string, fn func()) {
		defer func() {
			if recover() == nil {
				t.Errorf("%s: expected panic", name)
			}
		}()
		fn()
	}

	if embeddedManagerField[good]() < 0 {
		t.Error("good: expected a valid field index")
	}
	if embeddedManagerField[alsoGood]() != 1 {
		t.Errorf("alsoGood: expected index 1, got %d", embeddedManagerField[alsoGood]())
	}
	mustPanic("namedNotEmbedded", func() { embeddedManagerField[namedNotEmbedded]() })
	mustPanic("noManager", func() { embeddedManagerField[noManager]() })
}

func TestBind_BadTypePanicsAtWiring(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for T without embedded *db.Manager")
		}
	}()
	type bad struct{ X int }
	_ = Bind[bad]("x", func() Config { return Config{Driver: SQLite} })
}

func TestBind_NilBuildPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for nil build func")
		}
	}()
	_ = Bind[testDBHandle]("x", nil)
}

func TestBind_ReturnsOption(t *testing.T) {
	var opt nexus.Option = Bind[testDBHandle]("testdb", func() Config {
		return Config{Driver: SQLite, Database: "file::memory:?cache=shared"}
	}, WithDefault())
	if opt == nil {
		t.Fatal("Bind returned nil Option")
	}
}

func TestConfigFor_KeyMapping(t *testing.T) {
	spec := nexus.DatabaseSpec{Driver: "postgres", KeyPrefix: "db.uaa", SSLMode: "disable", TimeZone: "TZ"}
	seen := map[string]string{
		"db.uaa.hostname": "h", "db.uaa.port": "5432",
		"db.uaa.username": "u", "db.uaa.password": "p", "db.uaa.name": "uaadb",
	}
	cfg := configFor(spec, func(k string) string { return seen[k] })
	want := Config{Driver: Postgres, Host: "h", Port: "5432", User: "u",
		Password: "p", Database: "uaadb", SSLMode: "disable", TimeZone: "TZ"}
	if cfg != want {
		t.Errorf("config mismatch:\n got %+v\nwant %+v", cfg, want)
	}
}

func TestConfigFor_InlineNoConfigServer(t *testing.T) {
	// No key_prefix: all values inline. get must never be consulted.
	spec := nexus.DatabaseSpec{
		Driver: "postgres", Host: "localhost", Port: "5432",
		User: "postgres", Password: "secret", Name: "myapp", SSLMode: "disable",
	}
	cfg := configFor(spec, func(k string) string {
		t.Fatalf("config server consulted for %q in inline mode", k)
		return ""
	})
	want := Config{Driver: Postgres, Host: "localhost", Port: "5432",
		User: "postgres", Password: "secret", Database: "myapp", SSLMode: "disable"}
	if cfg != want {
		t.Errorf("inline config mismatch:\n got %+v\nwant %+v", cfg, want)
	}
}

func TestConfigFor_InlineOverridesPrefix(t *testing.T) {
	// Inline host wins; the rest fall back to key_prefix.
	spec := nexus.DatabaseSpec{Driver: "postgres", KeyPrefix: "db.uaa", Host: "inline-host"}
	get := map[string]string{
		"db.uaa.hostname": "server-host", "db.uaa.port": "5432",
		"db.uaa.username": "u", "db.uaa.password": "p", "db.uaa.name": "n",
	}
	cfg := configFor(spec, func(k string) string { return get[k] })
	if cfg.Host != "inline-host" {
		t.Errorf("Host = %q, want inline-host (inline should win)", cfg.Host)
	}
	if cfg.Port != "5432" || cfg.User != "u" {
		t.Errorf("non-inline fields should fall back to prefix: %+v", cfg)
	}
}

// writeTOML writes body to a temp nexus.toml and returns its path.
func writeTOML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "nexus.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBindFromConfig_Panics drives the spec registry through the public
// loader (nexus.LoadConfig registers [databases.*] specs), then checks
// that resolution fails fast at boot for a missing block or a bad driver —
// while construction itself never panics (the lookup is deferred so the
// option works under nexus.Boot, which builds args before loading config).
func TestBindFromConfig_Panics(t *testing.T) {
	const toml = `
[databases.good]
driver     = "postgres"
key_prefix = "db.good"

[databases.nodriver]
driver     = "mongo"
key_prefix = "db.x"
`
	if _, err := nexus.LoadConfig(writeTOML(t, toml)); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	type H struct{ *Manager }

	cases := []struct{ name, section string }{
		{"missing block", "absent"},
		{"bad driver", "nodriver"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Construction must NOT panic — the spec lookup is deferred
			// to boot so BindFromConfig works under nexus.Boot.
			_ = BindFromConfig[H](c.section)

			// ...but the deferred resolver still fails fast at boot.
			defer func() {
				if recover() == nil {
					t.Fatalf("expected boot-time panic for %s", c.name)
				}
			}()
			_ = resolveSpec(c.section)
		})
	}
}

func TestBindFromConfig_Valid(t *testing.T) {
	const toml = `
[databases.good]
driver     = "postgres"
key_prefix = "db.good"
default    = true
schema     = "main"
`
	if _, err := nexus.LoadConfig(writeTOML(t, toml)); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	type H struct{ *Manager }
	if BindFromConfig[H]("good") == nil {
		t.Fatal("expected non-nil Option")
	}
	spec := resolveSpec("good")
	if spec.Driver != "postgres" || !spec.Default || spec.Schema != "main" {
		t.Errorf("resolved spec = %+v", spec)
	}
}
