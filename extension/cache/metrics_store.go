package cache

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/extension/metrics"
)

// NewMetricsStore returns a metrics.Store backed by a nexus *Manager —
// the in-memory path uses go-cache, and the Redis path persists counters
// so multi-replica deploys see aggregated totals. It lives in package
// cache (not metrics) so that extension/metrics — which the framework core
// imports for the default in-process store — never drags Redis/gocache
// into the build. Opt in explicitly:
//
//	Config.Stores.Metrics = cache.NewMetricsStore(mgr)
//
// Semantics: counters are best-effort. Every Record does a read-modify-
// write against the cache under the endpoint key, so concurrent writers
// (or replicas in Redis mode) can step on each other's increments. For a
// dashboard view that's fine; for exact counts under contention point the
// app at a Prometheus collector instead. Keys are namespaced under
// "nexus.metrics." with a 24h TTL.
func NewMetricsStore(mgr *Manager) metrics.Store {
	return &metricsStore{mgr: mgr, ttl: 24 * time.Hour, keys: map[string]struct{}{}}
}

type metricsStore struct {
	mgr *Manager
	ttl time.Duration
	mu  sync.Mutex
	// keys is the set of namespaced keys we've ever written, so Snapshot
	// can enumerate them without a KEYS * over redis.
	keys map[string]struct{}
}

func (s *metricsStore) cacheKey(key string) string { return "nexus.metrics." + key }

func (s *metricsStore) Record(key, ip string, err error) {
	ck := s.cacheKey(key)
	s.mu.Lock()
	s.keys[ck] = struct{}{}
	s.mu.Unlock()

	ctx := context.Background()
	var current metrics.EndpointStats
	_ = s.mgr.Get(ctx, ck, &current) // miss → zero value, which is fine

	now := time.Now()
	current.Key = key
	current.Count++
	current.LastAt = now
	if err != nil {
		current.Errors++
		current.LastError = err.Error()
		current.LastErrAt = now
		ev := metrics.ErrorEvent{Timestamp: now, IP: ip, Message: err.Error()}
		current.RecentErrors = append([]metrics.ErrorEvent{ev}, current.RecentErrors...)
		if len(current.RecentErrors) > metrics.RecentErrorsCap {
			current.RecentErrors = current.RecentErrors[:metrics.RecentErrorsCap]
		}
	}
	_ = s.mgr.Set(ctx, ck, &current, s.ttl)
}

func (s *metricsStore) Get(key string) (metrics.EndpointStats, bool) {
	ck := s.cacheKey(key)
	var out metrics.EndpointStats
	if err := s.mgr.Get(context.Background(), ck, &out); err != nil {
		return metrics.EndpointStats{}, false
	}
	// Same contract as Snapshot — callers who want the error ring call Errors().
	out.RecentErrors = nil
	return out, true
}

func (s *metricsStore) Snapshot() []metrics.EndpointStats {
	s.mu.Lock()
	keys := make([]string, 0, len(s.keys))
	for k := range s.keys {
		keys = append(keys, k)
	}
	s.mu.Unlock()

	ctx := context.Background()
	out := make([]metrics.EndpointStats, 0, len(keys))
	for _, ck := range keys {
		var row metrics.EndpointStats
		if err := s.mgr.Get(ctx, ck, &row); err == nil {
			row.RecentErrors = nil
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Errors returns the full ring of recent errors for key, pulled straight
// from the cache payload since the store persists the whole slice each
// Record call — cost is one Get per dialog-open.
func (s *metricsStore) Errors(key string) []metrics.ErrorEvent {
	var row metrics.EndpointStats
	if err := s.mgr.Get(context.Background(), s.cacheKey(key), &row); err != nil {
		return nil
	}
	return row.RecentErrors
}
