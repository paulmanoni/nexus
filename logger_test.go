package nexus

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2/config"
)

// TestLogger_DefaultProvided: without WithLogger the graph still provides a
// *slog.Logger, and it is the same one App.Logger returns.
func TestLogger_DefaultProvided(t *testing.T) {
	var got *slog.Logger
	app, stop, err := InProcess(config.Runtime{}, Invoke(func(l *slog.Logger) { got = l }))
	if err != nil {
		t.Fatal(err)
	}
	defer stop(context.Background())
	if got == nil || got != app.Logger() {
		t.Fatalf("provided logger %p, App.Logger %p", got, app.Logger())
	}
}

// TestLogger_WithLoggerOverrides: WithLogger reaches DI consumers, Managed
// builders and App.Logger, wherever it sits in the option list.
func TestLogger_WithLoggerOverrides(t *testing.T) {
	var buf bytes.Buffer
	mine := slog.New(slog.NewJSONHandler(&buf, nil))
	type handle struct{ log *slog.Logger }
	var injected *slog.Logger
	var built *handle
	app, stop, err := InProcess(config.Runtime{},
		Invoke(func(l *slog.Logger) { injected = l }),
		Managed("thing", func(l *slog.Logger) (*handle, error) { return &handle{l}, nil }, nil),
		Invoke(func(h *handle) { built = h }),
		WithLogger(mine),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(context.Background())
	if injected != mine || built.log != mine || app.Logger() != mine {
		t.Fatal("WithLogger did not replace the app logger everywhere")
	}
	app.Logger().Info("hello", "k", "v")
	if !strings.Contains(buf.String(), `"msg":"hello"`) {
		t.Fatalf("log line not written through the override: %q", buf.String())
	}
}

// TestLogger_SupplyCollisionHint: an app that still hands the graph its own
// logger gets a boot error whose fix line points at WithLogger.
func TestLogger_SupplyCollisionHint(t *testing.T) {
	_, _, err := InProcess(config.Runtime{},
		Supply(slog.New(slog.DiscardHandler)),
		Invoke(func(*slog.Logger) {}),
	)
	if err == nil {
		t.Fatal("want a duplicate-provider error")
	}
	if got := wiringHint(err); got != loggerDupHint {
		t.Fatalf("hint = %q for %v", got, err)
	}
}

func TestParseLogLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"": slog.LevelInfo, "DEBUG": slog.LevelDebug, "warn": slog.LevelWarn,
		"error": slog.LevelError, "bogus": slog.LevelInfo,
	} {
		if got := parseLogLevel(in); got != want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", in, got, want)
		}
	}
}
