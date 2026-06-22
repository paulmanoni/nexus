package nexus

import (
	"context"
	"testing"

	"github.com/paulmanoni/nexus/di"
)

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
