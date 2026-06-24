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
	// Default to an ephemeral loopback port. registerLifecycle always binds a
	// listener (the bind also gates startup tasks + manifest resolution), so we
	// can't skip it; binding :0 keeps tests parallel-safe and free of fixed-port
	// conflicts. Routing in tests goes through App.ServeHTTP, not this socket —
	// the listener is only used by Server() for WS/loopback cases.
	//
	// TODO(harness): registerLifecycle still prints a "listening on" banner for
	// the resolved port. A quiet-boot seam (skip the banner + go-serve when an
	// app-level "no listener" flag is set) would make InProcess truly silent and
	// socket-free; that's a separate framework change, tracked in the test design.
	if cfg.Server.Addr == "" {
		cfg.Server.Addr = "127.0.0.1:0"
	}

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
