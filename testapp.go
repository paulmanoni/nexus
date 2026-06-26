package nexus

import (
	"context"

	"github.com/paulmanoni/nexus/di"
)

// InProcess assembles and starts an App exactly like Run — same early/user/late
// option ordering, so AsRest/AsQuery/AsMutation/AsWS are fully mounted and every
// user middleware is installed — but it does NOT bind a network listener. The
// returned *App is an http.Handler (App.ServeHTTP), so tests can drive real
// routing through httptest without a socket. Call stop to run lifecycle teardown
// (resource Close, worker cancel, etc.).
//
// This is the supported building block for the nexustest package and for any
// app that wants to exercise its own handlers in-process. Production code uses
// Boot/Run; InProcess exists for tests.
func InProcess(cfg Config, opts ...Option) (app *App, stop func(context.Context) error, err error) {
	// Quiet-boot: skip the listener bind, go-serve, and "listening on …"
	// banner. Startup tasks, manifest resolution, the SDK dump, cron, and
	// liveness still run, so the app behaves like a real boot minus the
	// socket. Routing goes through App.ServeHTTP (or Server() for WS/loopback,
	// which binds its own httptest socket).
	cfg.Server.NoListener = true

	var captured *App
	capture := di.Invoke(func(a *App) { captured = a })

	// Mirror Run's ordering: early options (Supply cfg, Provide *App,
	// lifecycle), then user options, then the capture + late options
	// (autoMountGraphQL) so the GraphQL schema is built after LoadField and
	// every user-declared field/middleware is visible.
	all := []di.Option{fxEarlyOptions(cfg)}
	all = append(all, unwrap(opts)...)
	all = append(all, capture, fxLateOptions())

	c := di.New(all...)
	if buildErr := c.Err(); buildErr != nil {
		return nil, nil, buildErr
	}
	if startErr := c.Start(context.Background()); startErr != nil {
		// Best-effort teardown of anything that did start.
		_ = c.Stop(context.Background())
		return nil, nil, startErr
	}
	return captured, c.Stop, nil
}
