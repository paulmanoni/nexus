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

func TestDatabaseFromConfig_Panics(t *testing.T) {
	registerDatabaseSpecs(map[string]DatabaseSpec{
		"good":     {Driver: "postgres", KeyPrefix: "db.good"},
		"nodriver": {Driver: "mongo", KeyPrefix: "db.x"},
		"noprefix": {Driver: "postgres"},
	})
	type H struct{ *db.Manager }

	cases := []struct{ name, section string }{
		{"missing block", "absent"},
		{"bad driver", "nodriver"},
		{"missing key_prefix", "noprefix"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("expected panic for %s", c.name)
				}
			}()
			_ = DatabaseFromConfig[H](c.section)
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
