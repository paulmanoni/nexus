package db

import (
	"os"

	"github.com/paulmanoni/nexus/manifest"
)

// EnvNames maps each Config field that's normally env-driven to the
// env-var name that supplies its value. Same string serves two
// callers: Config.LoadEnv reads it (runtime); NexusEnv / NexusServices
// declare it (manifest). One signal, two consumers — no drift.
//
// Zero-value fields fall back to DefaultEnvNames, so apps that follow
// the framework convention only override the names that differ.
type EnvNames struct {
	Host, Port, User, Password, Database, SSLMode, TimeZone string
}

// DefaultEnvNames is the framework convention. Apps wiring db.Module
// without overrides get these names automatically.
var DefaultEnvNames = EnvNames{
	Host:     "DB_HOSTNAME",
	Port:     "DB_PORT",
	User:     "DB_USERNAME",
	Password: "DB_PASSWORD",
	Database: "DB_NAME",
	SSLMode:  "DB_SSLMODE",
	TimeZone: "DB_TIMEZONE",
}

// merge returns the effective name set: per-field, override wins;
// else default. Field-level merge (not all-or-nothing) so an app
// overriding only User= keeps the standard names for everything
// else.
func (e EnvNames) merge() EnvNames {
	out := e
	if out.Host == "" {
		out.Host = DefaultEnvNames.Host
	}
	if out.Port == "" {
		out.Port = DefaultEnvNames.Port
	}
	if out.User == "" {
		out.User = DefaultEnvNames.User
	}
	if out.Password == "" {
		out.Password = DefaultEnvNames.Password
	}
	if out.Database == "" {
		out.Database = DefaultEnvNames.Database
	}
	if out.SSLMode == "" {
		out.SSLMode = DefaultEnvNames.SSLMode
	}
	if out.TimeZone == "" {
		out.TimeZone = DefaultEnvNames.TimeZone
	}
	return out
}

// LoadConfig builds a Config by reading os.Getenv against the
// effective name set. defaults supplies values for fields whose
// resolved env is empty — the runtime fallback chain. Driver is
// taken straight (env-derived drivers haven't proved useful in
// practice).
//
// Side-effecting; called once from app constructors.
func LoadConfig(driver Driver, names EnvNames, defaults Config) Config {
	n := names.merge()
	pick := func(env, fallback string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		return fallback
	}
	return Config{
		Driver:   driver,
		Host:     pick(n.Host, defaults.Host),
		Port:     pick(n.Port, defaults.Port),
		User:     pick(n.User, defaults.User),
		Password: pick(n.Password, defaults.Password),
		Database: pick(n.Database, defaults.Database),
		SSLMode:  pick(n.SSLMode, defaults.SSLMode),
		TimeZone: pick(n.TimeZone, defaults.TimeZone),
	}
}

// ── Manifest providers ────────────────────────────────────────────
//
// *Manager carries the EnvNames that produced its Config so
// NexusEnv / NexusServices can report the actual env-var names this
// instance reads — including any per-app overrides. Static across
// runtime; safe to call before Start (the print-mode contract).

// NexusEnv lists the env vars this Manager reads, with required /
// secret / boundTo flags so the orchestration platform can render
// the deploy form correctly. Binding name is the manager's
// configured name (defaults to "main" — see NewManager / FromEnv).
func (m *Manager) NexusEnv() []manifest.EnvVar {
	n := m.envNames.merge()
	bind := m.bindName
	if bind == "" {
		bind = "main"
	}
	return []manifest.EnvVar{
		{Name: n.Host, Required: true, BoundTo: bind + ".host"},
		{Name: n.Port, Required: true, BoundTo: bind + ".port"},
		{Name: n.User, Required: true, BoundTo: bind + ".user"},
		{Name: n.Password, Required: true, Secret: true, BoundTo: bind + ".password"},
		{Name: n.Database, Required: true, BoundTo: bind + ".database"},
	}
}

// NexusServices declares the backing database the orchestration
// platform should provision (or external-bind). Kind matches the
// Driver, so a postgres Manager declares a postgres need; a mysql
// Manager declares mysql.
func (m *Manager) NexusServices() []manifest.ServiceNeed {
	n := m.envNames.merge()
	bind := m.bindName
	if bind == "" {
		bind = "main"
	}
	return []manifest.ServiceNeed{{
		Name: bind,
		Kind: string(m.cfg.Driver),
		ExposeAs: map[string]string{
			"host":     n.Host,
			"port":     n.Port,
			"user":     n.User,
			"password": n.Password,
			"database": n.Database,
		},
	}}
}
