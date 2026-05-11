package metrics

import (
	"context"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/cache"
)

// NewCacheStore returns a Store backed by a nexus cache.Manager —
// meaning the in-memory path uses go-cache (the same primitive every
// other nexus cache does), and the Redis path persists counters so
// multi-replica deploys see aggregated totals.
//
// Semantics: counters are best-effort. Every Record does a read-modify-
// write against the cache under the endpoint key, so concurrent writers
// from different goroutines (or replicas in Redis mode) can step on each
// other's increments. For a first-pass dashboard view that's fine — if
// you need exact counts under heavy contention, point your app at a
// Prometheus collector instead. The API shape stays identical either
// way so swapping later is a one-line change.
//
// Keys in the cache are namespaced under "nexus.metrics." so they don't
// collide with whatever else the app caches. TTL is 24h; rolling out
// restarts within a day preserves counters.
func NewCacheStore(mgr *cache.Manager) Store {
	return &cacheStore{mgr: mgr, ttl: 24 * time.Hour, keys: map[string]struct{}{}}
}

type cacheStore struct {
	mgr  *cache.Manager
	ttl  time.Duration
	mu   sync.Mutex
	// keys is the set of namespaced keys we've ever written, so
	// Snapshot can enumerate them without a KEYS * over redis.
	keys map[string]struct{}
}

func (s *cacheStore) cacheKey(key string) string { return "nexus.metrics." + key }

func (s *cacheStore) Record(key, ip string, err error) {
	ck := s.cacheKey(key)
	s.mu.Lock()
	s.keys[ck] = struct{}{}
	s.mu.Unlock()

	ctx := context.Background()
	var current EndpointStats
	_ = s.mgr.Get(ctx, ck, &current) // miss → zero value, which is fine

	now := time.Now()
	current.Key = key
	current.Count++
	current.LastAt = now
	if err != nil {
		current.Errors++
		current.LastError = err.Error()
		current.LastErrAt = now
		ev := ErrorEvent{Timestamp: now, IP: ip, Message: err.Error()}
		current.RecentErrors = append([]ErrorEvent{ev}, current.RecentErrors...)
		if len(current.RecentErrors) > RecentErrorsCap {
			current.RecentErrors = current.RecentErrors[:RecentErrorsCap]
		}
	}
	_ = s.mgr.Set(ctx, ck, &current, s.ttl)
}

func (s *cacheStore) Get(key string) (EndpointStats, bool) {
	ck := s.cacheKey(key)
	var out EndpointStats
	if err := s.mgr.Get(context.Background(), ck, &out); err != nil {
		return EndpointStats{}, false
	}
	// Same contract as Snapshot — callers who want the error ring call Errors().
	out.RecentErrors = nil
	return out, true
}

func (s *cacheStore) Snapshot() []EndpointStats {
	s.mu.Lock()
	keys := make([]string, 0, len(s.keys))
	for k := range s.keys {
		keys = append(keys, k)
	}
	s.mu.Unlock()

	ctx := context.Background()
	out := make([]EndpointStats, 0, len(keys))
	for _, ck := range keys {
		var row EndpointStats
		if err := s.mgr.Get(ctx, ck, &row); err == nil {
			// Strip the heavy RecentErrors slice — /stats polling stays
			// cheap even as the ring grows; use Errors(key) for the full
			// list on demand.
			row.RecentErrors = nil
			out = append(out, row)
		}
	}
	sortByKey(out)
	return out
}

// Errors returns the full ring of recent errors for key. Pulled straight
// from the cache payload since the store persists the whole slice each
// Record call — cost is one Get per dialog-open.
func (s *cacheStore) Errors(key string) []ErrorEvent {
	var row EndpointStats
	if err := s.mgr.Get(context.Background(), s.cacheKey(key), &row); err != nil {
		return nil
	}
	return row.RecentErrors
}
