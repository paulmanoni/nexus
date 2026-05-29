package nexus

import (
	"fmt"
	"sync"

	"github.com/paulmanoni/nexus/db"
)

// DatabaseSpec is one [databases.<name>] block in nexus.toml. It
// declares connection *structure* only — the secret values (host,
// port, user, password, db name) are pulled from the config server at
// boot using KeyPrefix, so credentials never live in nexus.toml.
//
//	[databases.uaa]
//	driver     = "postgres"
//	key_prefix = "db.uaa"      # reads db.uaa.{hostname,port,username,password,name}
//	sslmode    = "disable"
//	timezone   = "Africa/Dar_es_Salaam"
//	schema     = "main"        # optional, dashboard detail only
//	default    = false
type DatabaseSpec struct {
	Driver    string `toml:"driver"`
	KeyPrefix string `toml:"key_prefix"`
	SSLMode   string `toml:"sslmode"`
	TimeZone  string `toml:"timezone"`
	Schema    string `toml:"schema"`
	Default   bool   `toml:"default"`
}

var (
	dbSpecsMu sync.RWMutex
	dbSpecs   map[string]DatabaseSpec
)

// registerDatabaseSpecs stores the [databases.*] blocks parsed from
// nexus.toml so DatabaseFromConfig can resolve them. Called by
// LoadConfig; replaces any previously-registered set.
func registerDatabaseSpecs(m map[string]DatabaseSpec) {
	dbSpecsMu.Lock()
	defer dbSpecsMu.Unlock()
	dbSpecs = m
}

func databaseSpec(name string) (DatabaseSpec, bool) {
	dbSpecsMu.RLock()
	defer dbSpecsMu.RUnlock()
	s, ok := dbSpecs[name]
	return s, ok
}

// The fixed config-key suffixes read under a database's key_prefix.
// Chosen to match the conventional layout
// (db.<name>.hostname/port/username/password/name).
const (
	dbKeyHostname = ".hostname"
	dbKeyPort     = ".port"
	dbKeyUsername = ".username"
	dbKeyPassword = ".password"
	dbKeyName     = ".name"
)

// DatabaseFromConfig binds a marker type T to the [databases.<name>]
// block declared in nexus.toml. Connection *structure* (driver,
// sslmode, timezone) comes from the TOML; the host/port/credentials are
// read from the config server at boot via the block's key_prefix —
// secrets stay out of nexus.toml. Otherwise identical to Database[T]:
// the framework manages lifecycle and dashboard registration, and
// handlers inject *T unchanged.
//
//	nexus.DatabaseFromConfig[OatsUAADB]("uaa")
//
// Requires nexus.LoadConfig / MustLoadConfig to have run first (it
// parses the [databases.*] blocks). A missing or malformed block panics
// at wiring time — fail-fast, never at request time.
func DatabaseFromConfig[T any](name string, opts ...DatabaseOption) Option {
	spec, ok := databaseSpec(name)
	if !ok {
		panic(fmt.Sprintf("nexus.DatabaseFromConfig[%q]: no [databases.%s] block found — "+
			"declare it in nexus.toml and ensure nexus.MustLoadConfig() runs before options are built", name, name))
	}
	switch db.Driver(spec.Driver) {
	case db.Postgres, db.MySQL, db.SQLite:
	default:
		panic(fmt.Sprintf("nexus.DatabaseFromConfig[%q]: [databases.%s].driver = %q is not one of postgres/mysql/sqlite", name, name, spec.Driver))
	}
	if spec.KeyPrefix == "" {
		panic(fmt.Sprintf("nexus.DatabaseFromConfig[%q]: [databases.%s] is missing key_prefix", name, name))
	}

	// Spec-derived options first so explicit caller opts can override.
	base := make([]DatabaseOption, 0, len(opts)+2)
	if spec.Default {
		base = append(base, WithDatabaseDefault())
	}
	details := map[string]any{"engine": spec.Driver}
	if spec.Schema != "" {
		details["schema"] = spec.Schema
	}
	base = append(base, WithDatabaseDetails(details))
	base = append(base, opts...)

	return Database[T](name, func() db.Config {
		return databaseConfigFor(spec, func(k string) string { return Get[string](k) })
	}, base...)
}

// databaseConfigFor maps a spec + a key→value lookup into a db.Config.
// Split out from the build closure so the key mapping is unit-testable
// without a live config server (the closure passes nexus.Get as get).
func databaseConfigFor(spec DatabaseSpec, get func(string) string) db.Config {
	p := spec.KeyPrefix
	return db.Config{
		Driver:   db.Driver(spec.Driver),
		Host:     get(p + dbKeyHostname),
		Port:     get(p + dbKeyPort),
		User:     get(p + dbKeyUsername),
		Password: get(p + dbKeyPassword),
		Database: get(p + dbKeyName),
		SSLMode:  spec.SSLMode,
		TimeZone: spec.TimeZone,
	}
}
