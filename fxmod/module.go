// Package fxmod integrates nexus with go.uber.org/fx.
// Add fxmod.Module to your fx.New(...) call; nexus provides *nexus.App into
// the graph and manages the HTTP server's start/stop via fx.Lifecycle.
package fxmod

import (
	"context"
	"net"
	"net/http"

	"go.uber.org/fx"

	"github.com/paulmanoni/nexus"
)

// Config controls how the nexus App is built. Supply it via fx.Supply(cfg)
// or register a provider that returns it.
type Config struct {
	// Addr is the HTTP listen address (default ":8080").
	Addr string

	// DashboardName is the brand shown in the dashboard header and tab title
	// (default "Nexus"). The name is served over /__nexus/config so you can
	// change it per environment without rebuilding the UI.
	DashboardName string

	// TraceCapacity is the ring-buffer size for request traces.
	// 0 disables tracing — the Traces tab will stay empty.
	TraceCapacity int

	// EnableDashboard mounts /__nexus/* if true.
	EnableDashboard bool
}

// Module provides *nexus.App plus a managed *http.Server bound to its Gin engine.
// Inputs expected from the fx graph: Config. Outputs: *nexus.App.
var Module = fx.Module("nexus",
	fx.Provide(NewApp),
	fx.Invoke(registerLifecycle),
)

// NewApp is the fx provider for *nexus.App.
func NewApp(cfg Config) *nexus.App {
	var opts []nexus.Option
	if cfg.TraceCapacity > 0 {
		opts = append(opts, nexus.WithTracing(cfg.TraceCapacity))
	}
	if cfg.EnableDashboard {
		opts = append(opts, nexus.WithDashboard())
	}
	if cfg.DashboardName != "" {
		opts = append(opts, nexus.WithDashboardName(cfg.DashboardName))
	}
	return nexus.New(opts...)
}

func registerLifecycle(lc fx.Lifecycle, app *nexus.App, cfg Config) {
	addr := cfg.Addr
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{Handler: app}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			// Bind synchronously so port conflicts abort fx.Start() cleanly.
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return err
			}
			srv.Addr = ln.Addr().String()
			go func() { _ = srv.Serve(ln) }()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			return srv.Shutdown(ctx)
		},
	})
}
