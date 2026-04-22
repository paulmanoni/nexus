// Package multi routes N named instances of the same type behind a single
// .Using(name) dispatcher. The canonical case is multiple databases:
//
//	dbs := multi.New[*gorm.DB]().
//	    Register("main",      mainDB,  multi.AsDefault()).
//	    Register("questions", qbDB).
//	    Register("uaa",       uaaDB)
//
//	// Inside a resolver / handler:
//	dbs.Using("main").Find(&rows)           // explicit
//	dbs.Using("").Find(&rows)               // "" = default ("main" here)
//	dbs.Using("questions").Find(&rows)
//
// The package is transport-agnostic: T can be *gorm.DB, *sql.DB, an HTTP
// client, a Redis client — anything you'd route by name. nexus uses it in
// examples/graphapp and you can pair it with resource.NewDatabase to show
// every instance on the dashboard:
//
//	dbs.Each(func(name string, db *gorm.DB) {
//	    app.Register(resource.NewDatabase(name, "Postgres", nil,
//	        func() bool { sql, _ := db.DB(); return sql.Ping() == nil }))
//	})
package multi

import (
	"context"
	"sort"
	"sync"
)

// Option configures an entry at Register time.
type Option[T any] func(*entry[T])

// AsDefault marks this instance as the one returned for .Using("") and
// .Default(). If multiple entries are marked, the first wins. If none is
// marked, the first Register call sets the default implicitly.
func AsDefault[T any]() Option[T] {
	return func(e *entry[T]) { e.isDefault = true }
}

type entry[T any] struct {
	value     T
	isDefault bool
}

// Registry is the dispatcher. Thread-safe for concurrent Register and Using.
type Registry[T any] struct {
	mu          sync.RWMutex
	instances   map[string]entry[T]
	defaultName string
	hook        func(ctx context.Context, name string)
}

// New returns an empty Registry. Type parameter T is the instance type.
func New[T any]() *Registry[T] {
	return &Registry[T]{instances: map[string]entry[T]{}}
}

// Register stores v under name. Calling Register with an existing name
// overwrites the entry. Returns the Registry so calls chain.
func (r *Registry[T]) Register(name string, v T, opts ...Option[T]) *Registry[T] {
	e := entry[T]{value: v}
	for _, opt := range opts {
		opt(&e)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, existed := r.instances[name]; !existed && r.defaultName == "" {
		// First Register wins the default slot unless a later call marks another.
		r.defaultName = name
	}
	if e.isDefault {
		r.defaultName = name
	}
	r.instances[name] = e
	return r
}

// SetDefault explicitly marks an already-registered name as the default.
// Panics if the name isn't registered — that's a programmer bug.
func (r *Registry[T]) SetDefault(name string) *Registry[T] {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.instances[name]; !ok {
		panic("multi: SetDefault on unknown name " + name)
	}
	r.defaultName = name
	return r
}

// Using returns the instance registered under name. An empty string resolves
// to the default. Returns the zero value of T when the name isn't known —
// rather than panicking — so optional lookups can chain .Has() first.
func (r *Registry[T]) Using(name string) T {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name == "" {
		name = r.defaultName
	}
	return r.instances[name].value
}

// Default is shorthand for Using("").
func (r *Registry[T]) Default() T { return r.Using("") }

// OnUse installs a callback fired every time UsingCtx is called. nexus uses
// this to auto-attach service→resource edges on the dashboard — the hook
// receives the caller's context.Context, and whatever the integrator reads
// out of it (e.g. trace.SpanFromCtx → service name) decides attribution.
//
// Setting the hook is idempotent — a second OnUse call replaces the first.
// Returns no value so the method satisfies a structural interface for the
// hook-installer side (nexus.UseReporter), at the cost of not chaining.
func (r *Registry[T]) OnUse(h func(ctx context.Context, name string)) {
	r.mu.Lock()
	r.hook = h
	r.mu.Unlock()
}

// UsingCtx is Using with a context. When a hook is installed via OnUse, it
// fires before the lookup; otherwise this is exactly Using(name).
// Context-aware code paths (GraphQL resolvers, HTTP handlers) should prefer
// UsingCtx so dashboards can correlate lookups with the current request.
func (r *Registry[T]) UsingCtx(ctx context.Context, name string) T {
	r.mu.RLock()
	h := r.hook
	r.mu.RUnlock()
	if h != nil {
		h(ctx, name)
	}
	return r.Using(name)
}

// DefaultName returns the name currently marked default, or "" if empty.
func (r *Registry[T]) DefaultName() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaultName
}

// Has reports whether an instance is registered under name. Empty string
// checks for a default.
func (r *Registry[T]) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name == "" {
		_, ok := r.instances[r.defaultName]
		return ok
	}
	_, ok := r.instances[name]
	return ok
}

// Names returns the registered names in lexical order. Useful for iterating
// to register each instance as a nexus.Resource or for health dashboards.
func (r *Registry[T]) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.instances))
	for k := range r.instances {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Each invokes fn(name, instance) for every registered entry, in lexical
// order. fn must not call back into the Registry's mutating methods — doing
// so deadlocks. For registration-side work, copy the names via Names() first.
func (r *Registry[T]) Each(fn func(name string, v T)) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.instances))
	for k := range r.instances {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		fn(n, r.instances[n].value)
	}
}

// Len returns the number of registered instances.
func (r *Registry[T]) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.instances)
}
