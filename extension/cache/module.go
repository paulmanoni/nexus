package cache

import (
	"context"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/paulmanoni/nexus/manifest"
)

// Provide is the fx provider: builds a Manager from a *Config, ties
// its Start/Stop to the Fx lifecycle, AND self-registers as a
// manifest.EnvProvider + ServiceDependencyProvider so the
// orchestration manifest reflects this cache's REDIS_HOST/PORT/
// PASSWORD requirements without the app's main.go having to call
// nexus.DeclareEnv / DeclareService.
//
// reg comes from the fx graph — *nexus.App satisfies the interface
// (it has matching methods) and is provided by fxEarlyOptions, so
// every app that uses cache.Module gets the registration for free.
// Apps that build their own *App via nexus.New() still wire it
// the same way.
func Provide(lc fx.Lifecycle, cfg *Config, logger *zap.Logger, reg manifest.Registrar) *Manager {
	m := NewManager(cfg, logger)
	reg.DeclareEnvProvider(m)
	reg.DeclareServiceProvider(m)
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
