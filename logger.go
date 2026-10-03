package nexus

import (
	"log/slog"
	"os"
	"strings"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/di"
)

// Logger returns the app's structured logger. It is also what the framework
// provides into the DI graph as *slog.Logger, so binders (db, cache,
// Managed, …) and any constructor that takes a *slog.Logger share it.
//
// The default writes JSON to stdout (slog.NewJSONHandler, AddSource on) at
// the level in [runtime.logging] level (debug | info | warn | error; info
// when unset). `nexus dev` renders those lines in its columnar log view; a
// production binary emits them as-is for a log collector. Replace it with
// WithLogger.
func (a *App) Logger() *slog.Logger {
	if l := a.logger.Load(); l != nil {
		return l
	}
	a.logger.CompareAndSwap(nil, defaultLogger())
	return a.logger.Load()
}

// WithLogger replaces the app's logger (App.Logger and the *slog.Logger the
// framework provides into DI):
//
//	nexus.Boot(nexus.WithLogger(slog.New(myHandler)))
//
// Any slog.Handler works. An app that prefers zap's encoder wraps a zap core
// in an slog.Handler (zap's own zapslog package does this) and passes the
// result here — the framework itself links no zap. Pass it once; providing a
// *slog.Logger by hand (nexus.Provide) collides with the framework's own.
func WithLogger(l *slog.Logger) Option {
	if l == nil {
		return Options()
	}
	return rawOption{o: di.Supply(&loggerOverride{l: l})}
}

// loggerOverride carries a WithLogger choice through the graph. Supplied
// (not scanned for in Run) so every graph builder — Run, InProcess, manifest
// print mode — honors it, and resolved by installLogger regardless of where
// the option sits in the list.
type loggerOverride struct{ l *slog.Logger }

// installLogger stashes a WithLogger choice on the app. Registered in
// fxEarlyOptions so it runs before any user invoke can read App.Logger.
func installLogger(a *App, o *loggerOverride) {
	if o != nil && o.l != nil {
		a.logger.Store(o.l)
	}
}

// provideLogger is the framework's *slog.Logger provider.
func provideLogger(a *App) *slog.Logger { return a.Logger() }

// defaultLogger is the JSON-to-stdout logger an app gets without WithLogger.
func defaultLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
		Level:     parseLogLevel(config.Get("runtime.logging.level", "")),
	}))
}

// parseLogLevel maps a [runtime.logging] level string to a slog level;
// empty or unrecognized values mean info.
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
