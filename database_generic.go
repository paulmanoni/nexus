package nexus

import (
	"context"
	"reflect"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/paulmanoni/nexus/db"
	"github.com/paulmanoni/nexus/resource"
)

// Database wires a named database connection declaratively, replacing
// the hand-written "construct a db.Manager, Start it, expose a
// NexusResources() method" provider boilerplate.
//
// T must be a struct that embeds *db.Manager — a one-line marker type
// whose only job is to give the connection a distinct Go type so the
// framework can inject the right one and draw service→DB edges in the
// dashboard:
//
//	type OatsDB struct{ *db.Manager }
//
//	nexus.Run(cfg,
//	    nexus.Database[OatsDB]("oats", func() db.Config {
//	        return db.Config{
//	            Driver:   db.Postgres,
//	            Host:     nexus.Get[string]("db.oats.hostname"),
//	            Port:     nexus.Get[string]("db.oats.port"),
//	            User:     nexus.Get[string]("db.oats.username"),
//	            Password: nexus.Get[string]("db.oats.password"),
//	            Database: nexus.Get[string]("db.oats.name"),
//	        }
//	    }, nexus.WithDatabaseDefault()),
//	    // … modules; handlers keep injecting *OatsDB unchanged
//	)
//
// build() is evaluated in the fx constructor (not at option-construction
// time), so nexus.Get and other startup-time config sources resolve
// correctly. The framework owns the lifecycle: it Start()s the manager
// on boot and Stop()s it on shutdown — so you no longer hand-call
// Start() or leak the connection at exit.
//
// The connection is registered as a dashboard resource via
// resource.NewDatabase regardless of whether it connects, so a down
// database still appears (red) rather than vanishing.
//
// T is validated immediately: a T that doesn't embed *db.Manager panics
// here, at wiring time, with a clear message — never at request time.
func Database[T any](name string, build func() db.Config, opts ...DatabaseOption) Option {
	fieldIdx := mustEmbeddedField[T](managerPtrType, "nexus.Database")
	requireResourceArgs("nexus.Database", name, build == nil)

	rc := resourceConfig{}
	for _, o := range opts {
		o(&rc)
	}

	ctor := func(lc fx.Lifecycle, logger *zap.Logger) (*T, error) {
		m := db.NewManager(build(), db.WithLogger(logger))
		h := newHandle[T](fieldIdx, m)
		lc.Append(fx.Hook{
			OnStart: func(context.Context) error { m.Start(); return nil },
			OnStop:  func(context.Context) error { m.Stop(); return nil },
		})
		return h, nil
	}

	register := func(app *App, h *T) {
		m := reflect.ValueOf(h).Elem().Field(fieldIdx).Interface().(*db.Manager)
		driver := string(m.Driver())
		desc := rc.description
		if desc == "" {
			desc = "GORM — " + driver
		}
		details := rc.details
		if details == nil {
			details = map[string]any{"engine": driver}
		}
		app.Register(resource.NewDatabase(name, desc, details, m.IsConnected, rc.resourceOpts()...))
	}

	return rawOption{o: fx.Options(fx.Provide(ctor), fx.Invoke(register))}
}

// DatabaseOption tunes how Database registers the dashboard resource.
type DatabaseOption func(*resourceConfig)

// WithDatabaseDefault marks this database as the default for its kind —
// the one a Service gets when it depends on a DB without naming one.
// Use on exactly one Database; flagging several is ambiguous.
func WithDatabaseDefault() DatabaseOption {
	return func(c *resourceConfig) { c.asDefault = true }
}

// WithDatabaseDetails overrides the resource detail map shown in the
// dashboard (default {"engine": <driver>}).
func WithDatabaseDetails(d map[string]any) DatabaseOption {
	return func(c *resourceConfig) { c.details = d }
}

// WithDatabaseDescription overrides the resource description shown in
// the dashboard (default "GORM — <driver>").
func WithDatabaseDescription(s string) DatabaseOption {
	return func(c *resourceConfig) { c.description = s }
}

var managerPtrType = reflect.TypeFor[*db.Manager]()
