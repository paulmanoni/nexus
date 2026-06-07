package nexus

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/paulmanoni/nexus/db"
)

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

func writeTOML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "nexus.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfig_ParsesDatabaseSpecs(t *testing.T) {
	if _, err := LoadConfig(writeTOML(t, tomlWithDatabases)); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	uaa, ok := databaseSpec("uaa")
	if !ok {
		t.Fatal("uaa spec not registered")
	}
	if uaa.Driver != "postgres" || uaa.KeyPrefix != "db.uaa" || uaa.SSLMode != "disable" ||
		uaa.TimeZone != "Africa/Dar_es_Salaam" || uaa.Schema != "main" {
		t.Errorf("uaa spec wrong: %+v", uaa)
	}
	if oats, _ := databaseSpec("oats"); !oats.Default {
		t.Error("oats should be default")
	}
	if ajira, _ := databaseSpec("ajira"); ajira.Driver != "mysql" {
		t.Errorf("ajira driver = %q", ajira.Driver)
	}
}

func TestDatabaseConfigFor_KeyMapping(t *testing.T) {
	spec := DatabaseSpec{Driver: "postgres", KeyPrefix: "db.uaa", SSLMode: "disable", TimeZone: "TZ"}
	seen := map[string]string{
		"db.uaa.hostname": "h", "db.uaa.port": "5432",
		"db.uaa.username": "u", "db.uaa.password": "p", "db.uaa.name": "uaadb",
	}
	cfg := databaseConfigFor(spec, func(k string) string { return seen[k] })
	want := db.Config{Driver: db.Postgres, Host: "h", Port: "5432", User: "u",
		Password: "p", Database: "uaadb", SSLMode: "disable", TimeZone: "TZ"}
	if cfg != want {
		t.Errorf("config mismatch:\n got %+v\nwant %+v", cfg, want)
	}
}

func TestDatabaseConfigFor_InlineNoConfigServer(t *testing.T) {
	// No key_prefix: all values inline. get must never be consulted.
	spec := DatabaseSpec{
		Driver: "postgres", Host: "localhost", Port: "5432",
		User: "postgres", Password: "secret", Name: "myapp", SSLMode: "disable",
	}
	cfg := databaseConfigFor(spec, func(k string) string {
		t.Fatalf("config server consulted for %q in inline mode", k)
		return ""
	})
	want := db.Config{Driver: db.Postgres, Host: "localhost", Port: "5432",
		User: "postgres", Password: "secret", Database: "myapp", SSLMode: "disable"}
	if cfg != want {
		t.Errorf("inline config mismatch:\n got %+v\nwant %+v", cfg, want)
	}
}

func TestDatabaseConfigFor_InlineOverridesPrefix(t *testing.T) {
	// Inline host wins; the rest fall back to key_prefix.
	spec := DatabaseSpec{Driver: "postgres", KeyPrefix: "db.uaa", Host: "inline-host"}
	get := map[string]string{
		"db.uaa.hostname": "server-host", "db.uaa.port": "5432",
		"db.uaa.username": "u", "db.uaa.password": "p", "db.uaa.name": "n",
	}
	cfg := databaseConfigFor(spec, func(k string) string { return get[k] })
	if cfg.Host != "inline-host" {
		t.Errorf("Host = %q, want inline-host (inline should win)", cfg.Host)
	}
	if cfg.Port != "5432" || cfg.User != "u" {
		t.Errorf("non-inline fields should fall back to prefix: %+v", cfg)
	}
}

func TestDatabaseFromConfig_Panics(t *testing.T) {
	registerDatabaseSpecs(map[string]DatabaseSpec{
		"good":     {Driver: "postgres", KeyPrefix: "db.good"},
		"nodriver": {Driver: "mongo", KeyPrefix: "db.x"},
	})
	type H struct{ *db.Manager }

	cases := []struct{ name, section string }{
		{"missing block", "absent"},
		{"bad driver", "nodriver"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Construction must NOT panic — the spec lookup is deferred
			// to boot so DatabaseFromConfig works under nexus.Boot,
			// which builds option args before it loads nexus.toml.
			_ = DatabaseFromConfig[H](c.section)

			// ...but the deferred resolver still fails fast at boot.
			defer func() {
				if recover() == nil {
					t.Fatalf("expected boot-time panic for %s", c.name)
				}
			}()
			_ = resolveDatabaseSpec(c.section)
		})
	}
}

func TestDatabaseFromConfig_ValidReturnsOption(t *testing.T) {
	registerDatabaseSpecs(map[string]DatabaseSpec{
		"good": {Driver: "postgres", KeyPrefix: "db.good", Default: true, Schema: "main"},
	})
	type H struct{ *db.Manager }
	if DatabaseFromConfig[H]("good") == nil {
		t.Fatal("expected non-nil Option")
	}
}

// TestDatabaseFromConfig_BootOrdering guards the nexus.Boot ordering:
// the option is BUILT before nexus.toml is loaded (Go evaluates Boot's
// arguments before Boot runs LoadConfig), then the [databases.*] spec
// registers, then resolution must succeed. Pre-v1.12.6 the eager spec
// lookup panicked at build time, which is the bug this prevents.
func TestDatabaseFromConfig_BootOrdering(t *testing.T) {
	registerDatabaseSpecs(map[string]DatabaseSpec{}) // nothing loaded yet
	type H struct{ *db.Manager }

	// Building the option with no matching spec must not panic — this
	// is exactly what Boot does before it reads nexus.toml.
	_ = DatabaseFromConfig[H]("late")

	// LoadConfig-equivalent (as Boot runs after evaluating its args).
	registerDatabaseSpecs(map[string]DatabaseSpec{
		"late": {Driver: "postgres", Host: "h", Name: "n", Default: true},
	})

	// The deferred resolution now succeeds.
	spec := resolveDatabaseSpec("late")
	if spec.Host != "h" || !spec.Default {
		t.Errorf("resolved spec = %+v, want Host=h Default=true", spec)
	}
}
