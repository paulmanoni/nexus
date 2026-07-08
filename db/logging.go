package db

import (
	"log"
	"os"
	"strings"
	"time"

	"github.com/paulmanoni/nexus"
	gormlogger "gorm.io/gorm/logger"
)

// resolveGormLogger builds the GORM logger for a connection from the
// configured level and the runtime mode. SQL logging is QUIET by default
// OUTSIDE dev — a production binary shouldn't spew slow-query / error lines
// unless asked — and warn-level under `nexus dev` / a development environment.
// Config.LogLevel (from [databases.*] log) overrides that auto-decision in
// either direction, so an operator can opt back into logs in production or
// silence them in dev.
//
// Accepted level values (case-insensitive):
//
//	""                            auto — warn in dev, silent otherwise
//	"silent" / "false" / "off"    never log
//	"error"                       errors only
//	"warn" / "true" / "on"        slow queries + errors (GORM's default)
//	"info" / "all"                every SQL statement
func resolveGormLogger(level string) gormlogger.Interface {
	lvl := resolveLogLevel(level, devMode())
	if lvl == gormlogger.Silent {
		return gormlogger.Default.LogMode(gormlogger.Silent)
	}
	return gormlogger.New(
		log.New(os.Stdout, "", log.LstdFlags),
		gormlogger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                  lvl,
			IgnoreRecordNotFoundError: true, // record-not-found is a normal query result, not an error
			Colorful:                  false,
		},
	)
}

// resolveLogLevel maps a configured level string (+ whether we're in dev) to a
// GORM log level. Unknown values fall back to the auto-by-dev decision.
func resolveLogLevel(raw string, dev bool) gormlogger.LogLevel {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "silent", "false", "off", "0", "none", "disable", "disabled":
		return gormlogger.Silent
	case "error":
		return gormlogger.Error
	case "warn", "warning", "true", "on", "1", "enable", "enabled":
		return gormlogger.Warn
	case "info", "all", "verbose", "debug":
		return gormlogger.Info
	default: // "" or unrecognized → auto
		if dev {
			return gormlogger.Warn
		}
		return gormlogger.Silent
	}
}

// devMode reports whether the app is running under `nexus dev` or an explicit
// development environment — the signal for on-by-default SQL logging. Reads the
// NEXUS_DEV env flag (set by `nexus dev`) and runtime.environment (nexus.toml).
func devMode() bool {
	if os.Getenv("NEXUS_DEV") != "" {
		return true
	}
	return strings.EqualFold(nexus.Get[string]("runtime.environment"), "development")
}
