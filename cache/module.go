package cache

import (
	"context"

	"go.uber.org/fx"
	"go.uber.org/zap"
)

// Provide is the fx provider: builds a Manager from a *Config and ties its
// Start/Stop to the Fx lifecycle. Use via cache.Module.
func Provide(lc fx.Lifecycle, cfg *Config, logger *zap.Logger) *Manager {
	m := NewManager(cfg, logger)
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error { m.Start(); return nil },
		OnStop:  func(_ context.Context) error { m.Stop(); return nil },
	})
	return m
}

// Module provides *Config (from env) and *Manager into the Fx graph. Consume
// it by taking *cache.Manager in your constructors.
var Module = fx.Module("nexus-cache",
	fx.Provide(
		NewConfig,
		Provide,
	),
)
