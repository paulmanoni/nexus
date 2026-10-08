package db

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sync"

	"github.com/paulmanoni/nexus/v2/di"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/internal/bindutil"
	"github.com/paulmanoni/nexus/v2/resource"
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
//	            Host:     config.Get[string]("db.main.hostname"),
//	            Port:     config.Get[string]("db.main.port"),
//	            User:     config.Get[string]("db.main.username"),
//	            Password: config.Get[string]("db.main.password"),
//	            Database: config.Get[string]("db.main.name"),
//	        }
//	    }, db.WithDefault()),
//	    // … modules; handlers keep injecting *MainDB unchanged
//	)
//
// build() is evaluated in the DI constructor (not at option-construction
// time), so config.Get and other startup-time config sources resolve. The
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
// [databases.*] block parsed by config.Load — resolve lazily. This is what
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

	ctor := func(lc di.Lifecycle, logger *slog.Logger) (*T, error) {
		cfg := build()
		if err := cfg.Validate(); err != nil {
			return nil, fmt.Errorf("db %q: %w", name, err)
		}
		m := NewManager(cfg, WithLogger(logger), WithBindName(name))
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
		ropts := []resource.Option{resource.WithDetails(func() map[string]any {
			out := make(map[string]any, len(details)+1)
			for k, v := range details {
				out[k] = v
			}
			for _, d := range described(app, name) {
				out[d.key] = d.value()
			}
			if bc.AsDefault {
				for _, d := range described(app, "") {
					out[d.key] = d.value()
				}
			}
			return out
		})}
		if bc.AsDefault {
			ropts = append(ropts, resource.AsDefault())
		}
		app.Register(resource.NewDatabase(name, desc, details, m.IsConnected, ropts...))
		app.SetValue(boundKey{name}, m)
		if bc.AsDefault {
			app.SetValue(boundKey{}, m)
		}
	}

	return nexus.Options(nexus.Provide(ctor), nexus.Invoke(register))
}

// boundKey is where Bind records a Manager in its app: by name, and with
// no name for the default.
type boundKey struct{ name string }

// Lookup is the Manager db.Bind registered in app under name; with an
// empty name, the one bound WithDefault, else the only one bound. It lets
// packages that reach databases by name (the ORM) find them without a Go
// type to inject.
func Lookup(app *nexus.App, name string) (*Manager, bool) {
	if app == nil {
		return nil, false
	}
	if v, ok := app.Value(boundKey{name}); ok {
		return v.(*Manager), true
	}
	if name != "" {
		return nil, false
	}
	var only *Manager
	for _, r := range app.Registry().Resources() {
		if r.Kind != resource.KindDatabase {
			continue
		}
		if v, ok := app.Value(boundKey{r.Name}); ok {
			if only != nil {
				return nil, false
			}
			only = v.(*Manager)
		}
	}
	return only, only != nil
}

// describeKey is where Describe keeps a database's added details.
type describeKey struct{ name string }

type detail struct {
	key   string
	value func() any
}

type details struct {
	mu   sync.Mutex
	list []detail
}

// Describe adds a detail to the dashboard entry of the database db.Bind
// registered as name in app (the default one for an empty name), computed each time the dashboard reads it:
// how packages built on a database (the ORM's models) show what they keep
// there. A key described again replaces the earlier value.
func Describe(app *nexus.App, name, key string, value func() any) {
	if app == nil || key == "" || value == nil {
		return
	}
	v, _ := app.Value(describeKey{name})
	d, ok := v.(*details)
	if !ok {
		d = &details{}
		app.SetValue(describeKey{name}, d)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, x := range d.list {
		if x.key == key {
			d.list[i].value = value
			return
		}
	}
	d.list = append(d.list, detail{key, value})
}

func described(app *nexus.App, name string) []detail {
	v, _ := app.Value(describeKey{name})
	d, ok := v.(*details)
	if !ok {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]detail(nil), d.list...)
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
