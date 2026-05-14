package gql

import (
	"sort"
	"sync"
)

// StatsRegistry is the per-app catalog of DocumentCaches that the
// dashboard reads from. Each Mount call with a non-nil cache and a
// non-nil registry registers itself here under its mount path, and
// Snapshot returns the current counters for every registered mount.
//
// Lifetime is tied to the *App that owns it — tests can construct a
// fresh registry per app to avoid cross-test bleed. The framework's
// Mount call wires this up automatically when nexus.autoMount runs;
// graphql-go-only users who construct the cache themselves can
// register manually.
//
// Methods are safe for concurrent use. A nil receiver is a no-op so
// callers can pass a nil registry to opt out of stats.
type StatsRegistry struct {
	mu      sync.RWMutex
	entries map[string]*DocumentCache
}

// NewStatsRegistry returns an empty registry. The framework constructs
// one per *App; passing it via WithStatsRegistry to gql.Mount enrolls
// each path's cache.
func NewStatsRegistry() *StatsRegistry {
	return &StatsRegistry{entries: map[string]*DocumentCache{}}
}

// Register associates cache with path. Subsequent registrations on
// the same path overwrite — Mount is the only caller and each path
// is mounted at most once per engine. Nil cache unregisters.
func (r *StatsRegistry) Register(path string, cache *DocumentCache) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cache == nil {
		delete(r.entries, path)
		return
	}
	r.entries[path] = cache
}

// MountCacheStats pairs a mount path with its current cache counters.
// The dashboard renders one row per entry.
type MountCacheStats struct {
	Path string `json:"path"`
	DocumentCacheStats
}

// Snapshot returns the current counters for every registered mount,
// sorted by path so successive calls produce a stable order (useful
// for the WS live snapshot's hash-based dedup).
//
// Safe on a nil receiver — returns nil.
func (r *StatsRegistry) Snapshot() []MountCacheStats {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	out := make([]MountCacheStats, 0, len(r.entries))
	for path, c := range r.entries {
		out = append(out, MountCacheStats{
			Path:               path,
			DocumentCacheStats: c.Stats(),
		})
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
