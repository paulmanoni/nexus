package db

import (
	"context"
	"fmt"
	"reflect"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/paulmanoni/nexus"
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
//	type OatsDB struct{ *db.Manager }
//
//	nexus.Run(cfg,
//	    db.Bind[OatsDB]("oats", func() db.Config {
//	        return db.Config{
//	            Driver:   db.Postgres,
//	            Host:     nexus.Get[string]("db.oats.hostname"),
//	            Port:     nexus.Get[string]("db.oats.port"),
//	            User:     nexus.Get[string]("db.oats.username"),
//	            Password: nexus.Get[string]("db.oats.password"),
//	            Database: nexus.Get[string]("db.oats.name"),
//	        }
//	    }, db.WithDefault()),
//	    // … modules; handlers keep injecting *OatsDB unchanged
//	)
//
// build() is evaluated in the fx constructor (not at option-construction
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
// evaluated at fx-invoke time (inside register), NOT at option-construction
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

	ctor := func(lc fx.Lifecycle, logger *zap.Logger) (*T, error) {
		m := NewManager(build(), WithLogger(logger))
		h := new(T)
		reflect.ValueOf(h).Elem().Field(fieldIdx).Set(reflect.ValueOf(m))
		lc.Append(fx.Hook{
			OnStart: func(context.Context) error { m.Start(); return nil },
			OnStop:  func(context.Context) error { m.Stop(); return nil },
		})
		return h, nil
	}

	register := func(app *nexus.App, h *T) {
		var bc bindConfig
		for _, o := range optsFn() {
			o(&bc)
		}
		m := reflect.ValueOf(h).Elem().Field(fieldIdx).Interface().(*Manager)
		driver := string(m.Driver())
		desc := bc.description
		if desc == "" {
			desc = "GORM — " + driver
		}
		details := bc.details
		if details == nil {
			details = map[string]any{"engine": driver}
		}
		var ropts []resource.Option
		if bc.asDefault {
			ropts = append(ropts, resource.AsDefault())
		}
		app.Register(resource.NewDatabase(name, desc, details, m.IsConnected, ropts...))
	}

	return nexus.Options(nexus.Provide(ctor), nexus.Invoke(register))
}

// BindOption tunes how Bind registers the dashboard resource.
type BindOption func(*bindConfig)

type bindConfig struct {
	description string
	details     map[string]any
	asDefault   bool
}

// WithDefault marks this database as the default for its kind — the one a
// Service gets when it depends on a DB without naming one. Use on exactly
// one Bind; flagging several is ambiguous.
func WithDefault() BindOption {
	return func(c *bindConfig) { c.asDefault = true }
}

// WithDetails overrides the resource detail map shown in the dashboard
// (default {"engine": <driver>}).
func WithDetails(d map[string]any) BindOption {
	return func(c *bindConfig) { c.details = d }
}

// WithDescription overrides the resource description shown in the dashboard
// (default "GORM — <driver>").
func WithDescription(s string) BindOption {
	return func(c *bindConfig) { c.description = s }
}

var managerPtrType = reflect.TypeFor[*Manager]()

// embeddedManagerField returns the index of T's embedded *Manager field,
// mirroring the nexus root's embeddedFieldIndex locally so this binder
// stays self-contained (the same pattern pubsub.Broker / cache.Bind use).
func embeddedManagerField[T any]() int {
	t := reflect.TypeFor[T]()
	if t == nil || t.Kind() != reflect.Struct {
		panic("db.Bind: T must be a struct embedding *db.Manager")
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && f.Type == managerPtrType {
			return i
		}
	}
	panic(fmt.Sprintf("db.Bind: T (%s) must embed *db.Manager, e.g. `type C struct{ *db.Manager }`", t))
}
