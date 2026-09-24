package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/paulmanoni/nexus"
	"go.uber.org/zap"
	"gorm.io/gorm"
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
func resolveGormLogger(level string, logger *zap.Logger) gormlogger.Interface {
	lvl := resolveLogLevel(level, devMode())
	if lvl == gormlogger.Silent {
		return gormlogger.Default.LogMode(gormlogger.Silent)
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return zapGorm{log: logger, level: lvl, slow: 200 * time.Millisecond}
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

// gormInitFailure is the message gorm.Open emits when the dialector cannot
// reach the server. The Manager reports that itself — with the address and a
// hint — so letting GORM print it too doubles every connect failure, and the
// duplicate is the less useful of the two.
const gormInitFailure = "failed to initialize database"

// zapGorm adapts GORM's logger onto the Manager's zap logger. Before this,
// GORM wrote to stdout through the stdlib logger, so SQL and connect errors
// arrived in a different format from every other line the app logs, at a level
// nothing could filter, and outside the reach of `nexus dev`'s log view.
type zapGorm struct {
	log   *zap.Logger
	level gormlogger.LogLevel
	slow  time.Duration
}

func (l zapGorm) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	l.level = level
	return l
}

func (l zapGorm) Info(_ context.Context, msg string, args ...any) {
	if l.level >= gormlogger.Info {
		l.log.Info("db: " + fmt.Sprintf(msg, args...))
	}
}

func (l zapGorm) Warn(_ context.Context, msg string, args ...any) {
	if l.level >= gormlogger.Warn {
		l.log.Warn("db: " + fmt.Sprintf(msg, args...))
	}
}

func (l zapGorm) Error(_ context.Context, msg string, args ...any) {
	if l.level < gormlogger.Error {
		return
	}
	text := fmt.Sprintf(msg, args...)
	if strings.Contains(text, gormInitFailure) {
		// Dropped, not downgraded: the Manager's connect path reports the
		// same failure with the address and a fix, and a Debug copy still
		// prints under any development logger config.
		return
	}
	l.log.Error("db: " + text)
}

// Trace is GORM's per-query hook. Record-not-found is a normal query result
// rather than a fault, so it never reaches Error.
func (l zapGorm) Trace(_ context.Context, begin time.Time, fc func() (string, int64), err error) {
	if l.level <= gormlogger.Silent {
		return
	}
	elapsed := time.Since(begin)
	sql, rows := fc()
	fields := []zap.Field{
		zap.String("sql", sql),
		zap.Int64("rows", rows),
		zap.Duration("took", elapsed),
	}
	switch {
	case err != nil && l.level >= gormlogger.Error && !errors.Is(err, gorm.ErrRecordNotFound):
		l.log.Error("db: query failed", append(fields, zap.Error(err))...)
	case l.slow > 0 && elapsed > l.slow && l.level >= gormlogger.Warn:
		l.log.Warn("db: slow query", append(fields, zap.Duration("threshold", l.slow))...)
	case l.level >= gormlogger.Info:
		l.log.Info("db: query", fields...)
	}
}
