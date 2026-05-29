package nexus

import (
	"context"
	"reflect"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/paulmanoni/nexus/extension/cache"
)

// Cache wires a named cache connection declaratively — the cache
// counterpart to Database. T embeds *cache.Manager:
//
//	type SessionCache struct{ *cache.Manager }
//
//	nexus.Run(cfg,
//	    nexus.Cache[SessionCache]("session", func() *cache.Config {
//	        c := cache.NewConfig()
//	        c.Environment = nexus.Get[string]("cache.env")
//	        c.RedisHost = nexus.Get[string]("cache.redis.host")
//	        c.RedisPort = nexus.Get[string]("cache.redis.port")
//	        return c
//	    }, nexus.WithCacheDefault()),
//	)
//
// build() runs in the fx constructor (so nexus.Get resolves), the
// framework Start()s the manager on boot and Stop()s it on shutdown,
// and the connection is registered as a dashboard resource via the
// manager's own AsResource (which reports Redis-vs-memory health).
//
// Handlers inject *SessionCache exactly as before. T not embedding
// *cache.Manager panics at wiring time.
func Cache[T any](name string, build func() *cache.Config, opts ...CacheOption) Option {
	fieldIdx := mustEmbeddedField[T](cacheManagerPtrType, "nexus.Cache")
	requireResourceArgs("nexus.Cache", name, build == nil)

	rc := resourceConfig{}
	for _, o := range opts {
		o(&rc)
	}

	ctor := func(lc fx.Lifecycle, logger *zap.Logger) (*T, error) {
		m := cache.NewManager(build(), logger)
		h := newHandle[T](fieldIdx, m)
		lc.Append(fx.Hook{
			OnStart: func(context.Context) error { m.Start(); return nil },
			OnStop:  func(context.Context) error { m.Stop(); return nil },
		})
		return h, nil
	}

	register := func(app *App, h *T) {
		m := reflect.ValueOf(h).Elem().Field(fieldIdx).Interface().(*cache.Manager)
		desc := rc.description
		if desc == "" {
			desc = "cache (Redis / in-memory)"
		}
		// The manager's AsResource reports Redis-vs-memory health; reuse
		// it so the dashboard indicator matches the cache's own view.
		app.Register(m.AsResource(name, desc, rc.resourceOpts()...))
	}

	return rawOption{o: fx.Options(fx.Provide(ctor), fx.Invoke(register))}
}

// CacheOption tunes how Cache registers the dashboard resource.
type CacheOption func(*resourceConfig)

// WithCacheDefault marks this cache as the default for its kind.
func WithCacheDefault() CacheOption {
	return func(c *resourceConfig) { c.asDefault = true }
}

// WithCacheDescription overrides the dashboard resource description.
func WithCacheDescription(s string) CacheOption {
	return func(c *resourceConfig) { c.description = s }
}

var cacheManagerPtrType = reflect.TypeFor[*cache.Manager]()
