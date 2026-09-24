package db

import (
	"context"
	"reflect"

	"github.com/paulmanoni/nexus/di"
	"go.uber.org/zap"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/internal/bindutil"
	"github.com/paulmanoni/nexus/resource"
)

// Bind wires a named database connection declaratively, replacing the
// hand-written "construct a Manager, Start it, expose NexusResources()"
// provider boilerplate. It lives in package db (not the nexus root) so
// that importing the root does NOT drag GORM and the SQL drivers into the
// build — an app pays for those only when it actually calls db.Bind /
// db.BindFromConfig. This is the database counterpart to cache.Bind and
// pubsub.Broker.
//
// T must be a struct that embeds *Manager — a one-line marker type whose
// only job is to give the connection a distinct Go type so the framework
// can inject the right one and draw service→DB edges in the dashboard:
//
//	type MainDB struct{ *db.Manager }
//
//	nexus.Run(cfg,
//	    db.Bind[MainDB]("main", func() db.Config {
//	        return db.Config{
//	            Driver:   db.Postgres,
//	            Host:     nexus.Get[string]("db.main.hostname"),
//	            Port:     nexus.Get[string]("db.main.port"),
//	            User:     nexus.Get[string]("db.main.username"),
//	            Password: nexus.Get[string]("db.main.password"),
//	            Database: nexus.Get[string]("db.main.name"),
//	        }
//	    }, db.WithDefault()),
//	    // … modules; handlers keep injecting *MainDB unchanged
//	)
//
// build() is evaluated in the DI constructor (not at option-construction
// time), so nexus.Get and other startup-time config sources resolve. The
// framework owns the lifecycle (Start on boot, Stop on shutdown) and
// registers the connection as a dashboard resource via resource.NewDatabase
// regardless of whether it connects, so a down database appears (red)
// rather than vanishing. A T that doesn't embed *Manager panics here, at
// wiring time, with a clear message — never at request time.
func Bind[T any](name string, build func() Config, opts ...BindOption) nexus.Option {
	return bindOption[T](name, build, func() []BindOption { return opts })
}

// bindOption is the shared core behind Bind and BindFromConfig. optsFn is
// evaluated at register time (inside the invoke), NOT at option-construction
// time, so options derived from data only available after startup — like a
// [databases.*] block parsed by LoadConfig — resolve lazily. This is what
// lets BindFromConfig work under nexus.Boot, which evaluates its option
// arguments before it loads nexus.toml.
func bindOption[T any](name string, build func() Config, optsFn func() []BindOption) nexus.Option {
	fieldIdx := embeddedManagerField[T]()
	if name == "" {
		panic("db.Bind: name must not be empty")
	}
	if build == nil {
		panic("db.Bind: build func must not be nil")
	}

	ctor := func(lc di.Lifecycle, logger *zap.Logger) (*T, error) {
		m := NewManager(build(), WithLogger(logger), WithBindName(name))
		lc.Append(di.Hook{
			OnStart: func(context.Context) error { m.Start(); return nil },
			OnStop:  func(context.Context) error { m.Stop(); return nil },
		})
		return bindutil.NewHolder[T](fieldIdx, m), nil
	}

	register := func(app *nexus.App, h *T) {
		bc := bindutil.Apply(optsFn())
		m := bindutil.ManagerOf[*Manager](h, fieldIdx)
		driver := string(m.Driver())
		desc := bc.Description
		if desc == "" {
			desc = "GORM — " + driver
		}
		details := bc.Details
		if details == nil {
			details = map[string]any{"engine": driver}
		}
		var ropts []resource.Option
		if bc.AsDefault {
			ropts = append(ropts, resource.AsDefault())
		}
		app.Register(resource.NewDatabase(name, desc, details, m.IsConnected, ropts...))
	}

	return nexus.Options(nexus.Provide(ctor), nexus.Invoke(register))
}

// BindOption tunes how Bind registers the dashboard resource. Alias of
// the shared binder option type so db/cache/mail/storage stay uniform.
type BindOption = bindutil.Option

// WithDefault marks this database as the default for its kind — the one a
// Service gets when it depends on a DB without naming one. Use on exactly
// one Bind; flagging several is ambiguous.
func WithDefault() BindOption {
	return func(c *bindutil.Options) { c.AsDefault = true }
}

// WithDetails overrides the resource detail map shown in the dashboard
// (default {"engine": <driver>}).
func WithDetails(d map[string]any) BindOption {
	return func(c *bindutil.Options) { c.Details = d }
}

// WithDescription overrides the resource description shown in the dashboard
// (default "GORM — <driver>").
func WithDescription(s string) BindOption {
	return func(c *bindutil.Options) { c.Description = s }
}

func embeddedManagerField[T any]() int {
	return bindutil.EmbeddedField[T]("db.Bind", reflect.TypeFor[*Manager](),
		"type C struct{ *db.Manager }")
}
