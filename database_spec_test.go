package nexus

import "testing"

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
	if _, err := LoadConfig(writeTOML(t, tomlWithDatabases)); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	uaa, ok := DatabaseSpecFor("uaa")
	if !ok {
		t.Fatal("uaa spec not registered")
	}
	if uaa.Driver != "postgres" || uaa.KeyPrefix != "db.uaa" || uaa.SSLMode != "disable" ||
		uaa.TimeZone != "Africa/Dar_es_Salaam" || uaa.Schema != "main" {
		t.Errorf("uaa spec wrong: %+v", uaa)
	}
	if oats, _ := DatabaseSpecFor("oats"); !oats.Default {
		t.Error("oats should be default")
	}
	if ajira, _ := DatabaseSpecFor("ajira"); ajira.Driver != "mysql" {
		t.Errorf("ajira driver = %q", ajira.Driver)
	}
}
