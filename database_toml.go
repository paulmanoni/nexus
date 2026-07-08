package nexus

import (
	"sync"
)

// DatabaseSpec is one [databases.<name>] block in nexus.toml. Each
// connection value (host, port, user, password, db name) resolves
// inline-first, then via the config server:
//
//   - if the inline field is set, it's used (supports ${ENV} expansion,
//     applied by LoadConfig) — works with NO config server;
//   - else, if KeyPrefix is set, the value is read from the config
//     server at boot as <KeyPrefix>.{hostname,port,username,password,name}
//     — keeping secrets out of nexus.toml.
//
// Config-server mode (secrets stay external):
//
//	[databases.uaa]
//	driver     = "postgres"
//	key_prefix = "db.uaa"
//	sslmode    = "disable"
//	timezone   = "Africa/Dar_es_Salaam"
//
// Inline mode (no config server needed):
//
//	[databases.local]
//	driver   = "postgres"
//	host     = "localhost"
//	port     = "5432"
//	user     = "postgres"
//	password = "${DB_PASSWORD}"
//	name     = "myapp"
//	sslmode  = "disable"
//
// The binders that consume a spec (db.Bind / db.BindFromConfig) live in
// package db, not here — so the nexus core never imports GORM or a driver
// just to hold these TOML structs. The core only parses and stores specs;
// db reads them back via DatabaseSpecFor.
type DatabaseSpec struct {
	Driver    string `toml:"driver"`
	KeyPrefix string `toml:"key_prefix"`
	SSLMode   string `toml:"sslmode"`
	TimeZone  string `toml:"timezone"`
	Schema    string `toml:"schema"`
	Default   bool   `toml:"default"`

	// Log controls SQL/GORM logging for this connection. Empty means auto —
	// on (warn) under `nexus dev` / a development environment, silent
	// otherwise, so a production binary is quiet by default. Set it to opt
	// out of the auto-decision: "silent"/"false"/"off", "error", "warn"/
	// "true"/"on" (GORM's slow-query+error default), or "info"/"all" (every
	// statement). Case-insensitive.
	Log string `toml:"log"`

	// Inline values (optional). Each, when set, takes precedence over
	// the config-server lookup for that field. ${ENV} placeholders are
	// expanded by LoadConfig.
	Host     string `toml:"host"`
	Port     string `toml:"port"`
	User     string `toml:"user"`
	Password string `toml:"password"`
	Name     string `toml:"name"`
}

var (
	dbSpecsMu sync.RWMutex
	dbSpecs   map[string]DatabaseSpec
)

// registerDatabaseSpecs stores the [databases.*] blocks parsed from
// nexus.toml so db.BindFromConfig can resolve them. Called by LoadConfig;
// replaces any previously-registered set.
func registerDatabaseSpecs(m map[string]DatabaseSpec) {
	dbSpecsMu.Lock()
	defer dbSpecsMu.Unlock()
	dbSpecs = m
}

// DatabaseSpecFor returns the parsed [databases.<name>] block, if any. It
// is the seam db.BindFromConfig uses to read specs without the core
// importing package db. The lookup is deferred to boot (the binder calls
// it from its fx constructor), so specs need only exist by boot, not at
// option-construction time — which is what lets BindFromConfig work under
// nexus.Boot (Boot builds options before it loads nexus.toml).
func DatabaseSpecFor(name string) (DatabaseSpec, bool) {
	dbSpecsMu.RLock()
	defer dbSpecsMu.RUnlock()
	s, ok := dbSpecs[name]
	return s, ok
}
