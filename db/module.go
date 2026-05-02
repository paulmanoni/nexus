package db

import (
	"context"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/paulmanoni/nexus/manifest"
)

// ProvideOptions packages the inputs for the optional fx-graph
// constructor. Apps that want db.Module's auto-wiring populate this
// struct with their driver + per-app overrides; everything else
// falls back to defaults.
//
// Why a struct (and not free-form options): fx wants concrete typed
// inputs in its graph. A single ProvideOptions value supplied via
// fx.Supply is the cleanest way to thread per-app config into a
// shared module without per-app fx wiring.
type ProvideOptions struct {
	Driver Driver

	// EnvNames overrides the framework defaults for env-var names.
	// Empty fields inherit DefaultEnvNames so apps only override
	// what differs.
	EnvNames EnvNames

	// Defaults supplies fallback values for fields whose env var
	// resolves empty. SSLMode and TimeZone are the typical ones —
	// most apps hard-code them rather than expose env knobs.
	Defaults Config

	// BindName names this Manager in the manifest. Defaults to
	// "main". Multi-DB apps build multiple ProvideOptions with
	// distinct names + sub-modules.
	BindName string
}

// Provide is the fx provider: builds a Manager from ProvideOptions,
// ties Start/Stop to the fx lifecycle, AND self-registers as a
// manifest.EnvProvider + ServiceDependencyProvider so the
// orchestration manifest reflects this DB's connection surface
// without app code calling DeclareEnv / DeclareService.
//
// reg comes from the fx graph — *nexus.App satisfies the interface
// via its Declare* methods. Auto-supplied by fxEarlyOptions.
func Provide(lc fx.Lifecycle, opts ProvideOptions, logger *zap.Logger, reg manifest.Registrar) *Manager {
	cfg := LoadConfig(opts.Driver, opts.EnvNames, opts.Defaults)
	managerOpts := []Option{WithEnvNames(opts.EnvNames), WithBindName(opts.BindName)}
	if logger != nil {
		managerOpts = append(managerOpts, WithLogger(logger))
	}
	m := NewManager(cfg, managerOpts...)
	reg.DeclareEnvProvider(m)
	reg.DeclareServiceProvider(m)
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			// Manager.Start spawns the reconnect goroutine; the
			// actual gorm.Open + connect happens inside that loop
			// so a slow / unreachable DB never blocks fx.Start.
			// Failures surface as IsConnected()=false; callers
			// gate their first query on it.
			m.Start()
			return nil
		},
		OnStop: func(_ context.Context) error { m.Stop(); return nil },
	})
	return m
}

// Module is the fx wiring for an app that wants a single Manager
// with auto-manifest declarations. Compose with fx.Supply to feed
// the per-app ProvideOptions:
//
//	nexus.Run(cfg,
//	    fx.Supply(db.ProvideOptions{
//	        Driver: db.Postgres,
//	        EnvNames: db.EnvNames{
//	            User: "DB_USERNAME", Password: "PASSWORD",
//	            Database: "DATABASE_NAME",
//	        },
//	        Defaults: db.Config{SSLMode: "disable", TimeZone: "UTC"},
//	    }),
//	    db.Module,
//	    // ...other modules...
//	)
//
// Apps that need multiple Managers don't use this Module — they
// wire each Manager via nexus.Provide(NewMyDBA, NewMyDBB, ...) and
// rely on the *App auto-walk in nexus.Provide to register both.
var Module = fx.Module("nexus-db",
	fx.Provide(Provide),
)