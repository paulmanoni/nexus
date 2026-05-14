package dataloader

import (
	"context"
	"sync"
)

// Registry is the per-request map of loaders. The GraphQL transport's
// request middleware drops a fresh Registry on the context for every
// POST /graphql; resolvers reach into it via Get to share a single
// Loader instance with their siblings.
//
// Methods are safe for concurrent use. A nil receiver makes Get
// return a one-shot Loader (no sharing) — useful in tests that don't
// install the middleware, but pointless in production.
type Registry struct {
	mu      sync.Mutex
	loaders map[string]interface{}
}

// NewRegistry returns an empty per-request registry.
func NewRegistry() *Registry {
	return &Registry{loaders: make(map[string]interface{})}
}

type ctxKey struct{}

// WithRegistry attaches reg to ctx. The GraphQL middleware calls this
// once per request; resolvers retrieve the registry implicitly via
// Get. Returns a new context derived from ctx.
func WithRegistry(ctx context.Context, reg *Registry) context.Context {
	return context.WithValue(ctx, ctxKey{}, reg)
}

// FromContext returns the registry stashed by WithRegistry, or nil
// if the middleware didn't run for this request. Most callers should
// use Get instead; FromContext is exposed for advanced use (e.g.
// manually priming several loaders from a top-level resolver).
func FromContext(ctx context.Context) *Registry {
	if v, ok := ctx.Value(ctxKey{}).(*Registry); ok {
		return v
	}
	return nil
}

// Get returns the registered loader for (ctx, name), creating it on
// the first call within a request and reusing it on subsequent
// calls. The fetch function is captured on first call; second-and-
// later calls with different fetch functions silently get the
// first-registered one, which keeps siblings batching together.
//
// When the registry isn't on ctx (no middleware), Get returns a
// brand-new Loader every call — batching still works within one
// resolver's scope, but cross-resolver sharing is lost. Convenient
// for tests, wrong for production: install the middleware.
func Get[K comparable, V any](ctx context.Context, name string, fetch Fetch[K, V]) *Loader[K, V] {
	reg := FromContext(ctx)
	if reg == nil {
		return New(fetch)
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if existing, ok := reg.loaders[name]; ok {
		// Type assertion can't fail in correct use; callers using
		// the same name with different K/V types is a bug the
		// type system can't catch (the name is a string), so we
		// panic loudly rather than silently miscasting.
		l, ok := existing.(*Loader[K, V])
		if !ok {
			panic("dataloader.Get: name " + name + " registered with incompatible K/V types")
		}
		return l
	}
	// First call for this name in this request — capture ctx so
	// the eventual batch fetch inherits the GraphQL request's
	// cancellation + deadlines.
	l := New(fetch)
	l.batchCtx = ctx
	reg.loaders[name] = l
	return l
}

// Names returns the names of every loader registered in this request.
// Useful for tests and the dashboard's per-request introspection.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	out := make([]string, 0, len(r.loaders))
	for k := range r.loaders {
		out = append(out, k)
	}
	r.mu.Unlock()
	return out
}
