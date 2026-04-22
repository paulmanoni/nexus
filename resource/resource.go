// Package resource defines the abstractions nexus uses to know about
// databases, caches, message queues, and other external dependencies so they
// show up in the dashboard's Architecture view with health status.
//
// nexus does not ship concrete DB/cache implementations — users keep their
// own (GORM+failsafe, Redis/go-cache hybrid, etc.) and register them as
// Resources in one line:
//
//	app.Register(resource.NewDatabase("main", "Primary PG", nil, dbm.IsConnected, resource.AsDefault()))
//	app.Register(resource.NewCache("session", "Redis hybrid", nil, cache.IsRedisConnected, resource.AsDefault()))
//
// Services then reference resources by name, not by instance:
//
//	app.Service("adverts").Using("").MountGraphQL(...)              // default DB
//	app.Service("qb").Using("questions", "session").MountGraphQL(...) // explicit
//	app.Service("uaa").UsingDefaults().MountGraphQL(...)             // default of every kind
package resource

type Kind string

const (
	KindDatabase Kind = "database"
	KindCache    Kind = "cache"
	KindQueue    Kind = "queue" // RabbitMQ, Kafka, NATS, etc.
	KindOther    Kind = "other"
)

// Resource is anything whose health the dashboard should surface.
type Resource interface {
	Name() string            // unique identifier, e.g. "main-db"
	Kind() Kind              // database / cache / queue / other
	Describe() string        // short human description
	Healthy() bool           // called each time the registry snapshots
	Details() map[string]any // free-form (engine, host, version); shown in UI
	IsDefault() bool         // registry's DefaultOfKind picks this first
}

// Option tweaks a resource at construction time.
type Option func(*simple)

// AsDefault marks a resource as the default for its Kind. When a Service
// calls .Using("") (database) or .UsingDefaults() (all kinds), nexus picks the
// one flagged AsDefault. If none is flagged, the lexically-first resource of
// the kind wins.
func AsDefault() Option {
	return func(s *simple) { s.isDefault = true }
}

// WithDetails replaces the static details map with a function called on every
// dashboard snapshot. Use for live-varying metadata — the canonical case is a
// cache reporting "redis" vs "memory" as its backend flips on Redis outage.
func WithDetails(fn func() map[string]any) Option {
	return func(s *simple) { s.detailsFn = fn }
}

type simple struct {
	name      string
	kind      Kind
	desc      string
	details   map[string]any
	detailsFn func() map[string]any
	healthy   func() bool
	isDefault bool
}

func (s *simple) Name() string     { return s.name }
func (s *simple) Kind() Kind       { return s.kind }
func (s *simple) Describe() string { return s.desc }
func (s *simple) IsDefault() bool  { return s.isDefault }
func (s *simple) Details() map[string]any {
	if s.detailsFn != nil {
		return s.detailsFn()
	}
	return s.details
}
func (s *simple) Healthy() bool {
	if s.healthy == nil {
		return true
	}
	return s.healthy()
}

// NewDatabase wraps a connection (e.g. the user's existing DBManager) as a
// Resource. healthy is called on every dashboard poll — keep it cheap.
func NewDatabase(name, desc string, details map[string]any, healthy func() bool, opts ...Option) Resource {
	return build(&simple{name: name, kind: KindDatabase, desc: desc, details: details, healthy: healthy}, opts)
}

func NewCache(name, desc string, details map[string]any, healthy func() bool, opts ...Option) Resource {
	return build(&simple{name: name, kind: KindCache, desc: desc, details: details, healthy: healthy}, opts)
}

func NewQueue(name, desc string, details map[string]any, healthy func() bool, opts ...Option) Resource {
	return build(&simple{name: name, kind: KindQueue, desc: desc, details: details, healthy: healthy}, opts)
}

func build(s *simple, opts []Option) Resource {
	for _, opt := range opts {
		opt(s)
	}
	return s
}
