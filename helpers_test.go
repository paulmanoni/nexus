package nexus

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/di"
)

// writeTOML writes body to a temp nexus.toml and returns its path. Shared
// across the config-loading tests (previously lived in database_toml_test.go,
// whose binders moved to package db).
func writeTOML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "nexus.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// fxBootOptions returns the complete baseline di.Option chain —
// test-only sugar over the same fxEarlyOptions/fxLateOptions pair
// nexus.Run assembles (Run additionally interleaves user options,
// deferred options, and the stop timeout between them).
func fxBootOptions(cfg config.Runtime) di.Option {
	return di.Options(
		fxEarlyOptions(cfg),
		fxLateOptions(),
	)
}

func mustWriteTOML(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// diTestApp mirrors the slice of go.uber.org/fx/fxtest the tests relied on
// (New + RequireStart/RequireStop) for the builtin di container, so the
// migration off fx leaves test bodies essentially unchanged.
type diTestApp struct {
	*di.App
	tb testing.TB
}

// newTestApp builds an app from di options and fails the test on any build
// error — the drop-in replacement for fxtest.New(t, ...).
func newTestApp(tb testing.TB, opts ...di.Option) *diTestApp {
	tb.Helper()
	a := di.New(opts...)
	if err := a.Err(); err != nil {
		tb.Fatalf("di: build: %v", err)
	}
	return &diTestApp{App: a, tb: tb}
}

// RequireStart starts the app's lifecycle, failing the test on error.
func (a *diTestApp) RequireStart() *diTestApp {
	a.tb.Helper()
	if err := a.Start(context.Background()); err != nil {
		a.tb.Fatalf("di: start: %v", err)
	}
	return a
}

// RequireStop stops the app's lifecycle, failing the test on error.
func (a *diTestApp) RequireStop() {
	a.tb.Helper()
	if err := a.Stop(context.Background()); err != nil {
		a.tb.Fatalf("di: stop: %v", err)
	}
}
