package nexus

import (
	"fmt"
	"sync"

	"github.com/paulmanoni/nexus/db"
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
type DatabaseSpec struct {
	Driver    string `toml:"driver"`
	KeyPrefix string `toml:"key_prefix"`
	SSLMode   string `toml:"sslmode"`
	TimeZone  string `toml:"timezone"`
	Schema    string `toml:"schema"`
	Default   bool   `toml:"default"`

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
// block declared in nexus.toml. Structure (driver, sslmode, timezone)
// comes from the TOML; each connection value resolves inline-first then
// via the block's key_prefix against the config server (see
// DatabaseSpec) — so it works with or without a config server.
// Otherwise identical to Database[T]: the framework manages lifecycle
// and dashboard registration, and handlers inject *T unchanged.
//
//	nexus.DatabaseFromConfig[OatsUAADB]("uaa")
//
// Requires nexus.LoadConfig / MustLoadConfig to have run first (it
// parses the [databases.*] blocks). A missing block or an invalid
// driver panics at wiring time — fail-fast, never at request time.
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
	// No key_prefix requirement: a block may supply its values inline
	// instead (works without a config server). A block with neither
	// inline values nor key_prefix yields empty connection fields,
	// surfaced through the manager's health rather than a panic.

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
// Each field prefers the inline spec value; failing that, it reads
// <key_prefix>.<suffix> via get (the config server). Split out from the
// build closure so the resolution is unit-testable without a live
// config server (the closure passes nexus.Get as get).
func databaseConfigFor(spec DatabaseSpec, get func(string) string) db.Config {
	field := func(inline, suffix string) string {
		if inline != "" {
			return inline
		}
		if spec.KeyPrefix != "" {
			return get(spec.KeyPrefix + suffix)
		}
		return ""
	}
	return db.Config{
		Driver:   db.Driver(spec.Driver),
		Host:     field(spec.Host, dbKeyHostname),
		Port:     field(spec.Port, dbKeyPort),
		User:     field(spec.User, dbKeyUsername),
		Password: field(spec.Password, dbKeyPassword),
		Database: field(spec.Name, dbKeyName),
		SSLMode:  spec.SSLMode,
		TimeZone: spec.TimeZone,
	}
}
