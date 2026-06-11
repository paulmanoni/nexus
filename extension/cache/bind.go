package cache

import (
	"context"
	"reflect"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/resource"
)

// Bind wires a named cache connection declaratively — the cache
// counterpart to nexus.Database / pubsub.Broker. It lives here (not in
// the nexus root) so that importing the root package does NOT drag Redis,
// gocache, and Prometheus into the build; an app pays for those only when
// it actually calls cache.Bind. T must embed *Manager:
//
//	type SessionCache struct{ *cache.Manager }
//
//	nexus.Run(cfg,
//	    cache.Bind[SessionCache]("session", func() *cache.Config {
//	        c := cache.NewConfig()
//	        c.RedisHost = nexus.Get[string]("cache.redis.host")
//	        c.RedisPort = nexus.Get[string]("cache.redis.port")
//	        return c
//	    }, cache.WithDefault()),
//	)
//
// build() runs in the fx constructor (so nexus.Get resolves), the
// framework Start()s the manager on boot and Stop()s it on shutdown, and
// the connection is registered as a dashboard resource via the manager's
// AsResource (which reports Redis-vs-memory health). Handlers inject
// *SessionCache. T not embedding *Manager panics at wiring time.
func Bind[T any](name string, build func() *Config, opts ...BindOption) nexus.Option {
	fieldIdx := embeddedManagerField[T]()
	if name == "" {
		panic("cache.Bind: name must not be empty")
	}
	if build == nil {
		panic("cache.Bind: build func must not be nil")
	}

	var bc bindConfig
	for _, o := range opts {
		o(&bc)
	}

	ctor := func(lc fx.Lifecycle, logger *zap.Logger) (*T, error) {
		m := NewManager(build(), logger)
		h := new(T)
		reflect.ValueOf(h).Elem().Field(fieldIdx).Set(reflect.ValueOf(m))
		lc.Append(fx.Hook{
			OnStart: func(context.Context) error { m.Start(); return nil },
			OnStop:  func(context.Context) error { m.Stop(); return nil },
		})
		return h, nil
	}

	register := func(app *nexus.App, h *T) {
		m := reflect.ValueOf(h).Elem().Field(fieldIdx).Interface().(*Manager)
		desc := bc.description
		if desc == "" {
			desc = "cache (Redis / in-memory)"
		}
		var ropts []resource.Option
		if bc.asDefault {
			ropts = append(ropts, resource.AsDefault())
		}
		app.Register(m.AsResource(name, desc, ropts...))
	}

	return nexus.Options(nexus.Provide(ctor), nexus.Invoke(register))
}

// BindOption tunes how Bind registers the dashboard resource.
type BindOption func(*bindConfig)

type bindConfig struct {
	description string
	asDefault   bool
}

// WithDefault marks this cache as the default for its kind.
func WithDefault() BindOption {
	return func(c *bindConfig) { c.asDefault = true }
}

// WithDescription overrides the dashboard resource description.
func WithDescription(s string) BindOption {
	return func(c *bindConfig) { c.description = s }
}

var managerPtrType = reflect.TypeFor[*Manager]()

// embeddedManagerField returns the index of T's embedded *Manager field,
// mirroring the nexus root's embeddedFieldIndex locally so this binder
// stays self-contained (the same pattern pubsub.Broker uses).
func embeddedManagerField[T any]() int {
	t := reflect.TypeFor[T]()
	if t == nil || t.Kind() != reflect.Struct {
		panic("cache.Bind: T must be a struct embedding *cache.Manager")
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && f.Type == managerPtrType {
			return i
		}
	}
	panic("cache.Bind: T (" + t.String() + ") must embed *cache.Manager, e.g. `type C struct{ *cache.Manager }`")
}