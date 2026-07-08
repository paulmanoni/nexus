package db

import (
	"fmt"

	"github.com/paulmanoni/nexus"
)

// The fixed config-key suffixes read under a database's key_prefix. Chosen
// to match the conventional layout
// (db.<name>.hostname/port/username/password/name).
const (
	keyHostname = ".hostname"
	keyPort     = ".port"
	keyUsername = ".username"
	keyPassword = ".password"
	keyName     = ".name"
)

// BindFromConfig binds a marker type T to the [databases.<name>] block
// declared in nexus.toml. Structure (driver, sslmode, timezone) comes from
// the TOML; each connection value resolves inline-first then via the
// block's key_prefix against the config server (see nexus.DatabaseSpec) —
// so it works with or without a config server. Otherwise identical to
// Bind[T]: the framework manages lifecycle and dashboard registration, and
// handlers inject *T unchanged.
//
//	db.BindFromConfig[OatsUAADB]("uaa")
//
// The [databases.*] blocks are parsed by nexus.LoadConfig / MustLoadConfig
// / Boot. The spec lookup is DEFERRED to fx-construction time (boot), so
// this option may be built before the config is loaded — which is the case
// under nexus.Boot, since Go evaluates a call's arguments before the call
// runs. A missing block or an invalid driver still fails fast — at boot,
// never at request time.
func BindFromConfig[T any](name string, opts ...BindOption) nexus.Option {
	build := func() Config {
		return configFor(resolveSpec(name), func(k string) string { return nexus.Get[string](k) })
	}

	// Spec-derived options first so explicit caller opts can override.
	// Resolved lazily alongside build so the whole option defers the spec
	// lookup past construction.
	optsFn := func() []BindOption {
		spec := resolveSpec(name)
		base := make([]BindOption, 0, len(opts)+2)
		if spec.Default {
			base = append(base, WithDefault())
		}
		details := map[string]any{"engine": spec.Driver}
		if spec.Schema != "" {
			details["schema"] = spec.Schema
		}
		base = append(base, WithDetails(details))
		base = append(base, opts...)
		return base
	}

	return bindOption[T](name, build, optsFn)
}

// resolveSpec looks up a [databases.<name>] block via the nexus core's
// spec registry and validates its driver, panicking with a clear message
// on a missing block or an unsupported driver. Called lazily (from the fx
// constructor / register invoke) so the spec need only exist by boot.
func resolveSpec(name string) nexus.DatabaseSpec {
	spec, ok := nexus.DatabaseSpecFor(name)
	if !ok {
		panic(fmt.Sprintf("db.BindFromConfig[%q]: no [databases.%s] block found — "+
			"declare it in nexus.toml (loaded by nexus.Boot or nexus.MustLoadConfig)", name, name))
	}
	switch Driver(spec.Driver) {
	case Postgres, MySQL, SQLite:
	default:
		panic(fmt.Sprintf("db.BindFromConfig[%q]: [databases.%s].driver = %q is not one of postgres/mysql/sqlite", name, name, spec.Driver))
	}
	// No key_prefix requirement: a block may supply its values inline
	// (works without a config server). A block with neither inline values
	// nor key_prefix yields empty connection fields, surfaced through the
	// manager's health rather than a panic.
	return spec
}

// configFor maps a spec + a key→value lookup into a Config. Each field
// prefers the inline spec value; failing that, it reads
// <key_prefix>.<suffix> via get (the config server). Split out from the
// build closure so the resolution is unit-testable without a live config
// server (the closure passes nexus.Get as get).
func configFor(spec nexus.DatabaseSpec, get func(string) string) Config {
	field := func(inline, suffix string) string {
		if inline != "" {
			return inline
		}
		if spec.KeyPrefix != "" {
			return get(spec.KeyPrefix + suffix)
		}
		return ""
	}
	return Config{
		Driver:   Driver(spec.Driver),
		Host:     field(spec.Host, keyHostname),
		Port:     field(spec.Port, keyPort),
		User:     field(spec.User, keyUsername),
		Password: field(spec.Password, keyPassword),
		Database: field(spec.Name, keyName),
		SSLMode:  spec.SSLMode,
		TimeZone: spec.TimeZone,
		LogLevel: spec.Log,
	}
}
