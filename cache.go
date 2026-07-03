package nexus

import (
	"context"
	"time"
)

// Cache is the minimal interface the framework core needs from a cache
// tier. It lives in the root package so the core never imports a concrete
// cache implementation — keeping Redis, gocache, and Prometheus OUT of the
// dependency graph of an app that doesn't actually wire a cache.
//
// The concrete *cache.Manager in extension/cache satisfies this interface.
// Wire one declaratively with cache.Bind[T](...) (the cache counterpart to
// nexus.Database / pubsub.Broker), or hand the framework an existing one via
// Config.Stores.Cache. App.Cache() returns whatever was wired, or nil.
type Cache interface {
	Get(ctx context.Context, key string, out any) error
	Set(ctx context.Context, key string, value any, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
	Clear(ctx context.Context) error
}
