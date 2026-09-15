package cache

import (
	"context"
	"reflect"

	"github.com/paulmanoni/nexus/di"
	"go.uber.org/zap"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/internal/bindutil"
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
// build() runs in the DI constructor (so nexus.Get resolves), the
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

	ctor := func(lc di.Lifecycle, logger *zap.Logger) (*T, error) {
		m := NewManager(build(), logger)
		lc.Append(di.Hook{
			OnStart: func(context.Context) error { m.Start(); return nil },
			OnStop:  func(context.Context) error { m.Stop(); return nil },
		})
		return bindutil.NewHolder[T](fieldIdx, m), nil
	}

	// Options are applied here — at register time — not at option
	// construction, matching db.Bind, so options derived from config
	// parsed at boot resolve lazily under nexus.Boot.
	register := func(app *nexus.App, h *T) {
		bc := bindutil.Apply(opts)
		m := bindutil.ManagerOf[*Manager](h, fieldIdx)
		desc := bc.Description
		if desc == "" {
			desc = "cache (Redis / in-memory)"
		}
		var ropts []resource.Option
		if bc.AsDefault {
			ropts = append(ropts, resource.AsDefault())
		}
		app.Register(m.AsResource(name, desc, ropts...))
	}

	return nexus.Options(nexus.Provide(ctor), nexus.Invoke(register))
}

// BindOption tunes how Bind registers the dashboard resource. Alias of
// the shared binder option type so db/cache/mail/storage stay uniform.
type BindOption = bindutil.Option

// WithDefault marks this cache as the default cache resource.
func WithDefault() BindOption {
	return func(c *bindutil.Options) { c.AsDefault = true }
}

// WithDescription overrides the dashboard resource description.
func WithDescription(s string) BindOption {
	return func(c *bindutil.Options) { c.Description = s }
}

func embeddedManagerField[T any]() int {
	return bindutil.EmbeddedField[T]("cache.Bind", reflect.TypeFor[*Manager](),
		"type C struct{ *cache.Manager }")
}
