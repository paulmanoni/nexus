// Package ratelimit provides the rate-limit primitives nexus uses to
// throttle endpoints: a Limit shape, a Store interface (pluggable to
// memory, Redis, or any backend), and a token-bucket MemoryStore.
//
// Integration lives in the nexus package (AsQuery/AsMutation accept a
// RateLimit option; an auto-attached middleware consults the store per
// request). Dashboard reads + writes through the store so operators can
// tune limits live without redeploying.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Limit describes a per-endpoint rate ceiling. RPM is the steady-state
// request-per-minute target; Burst is the short-term capacity (defaults to
// max(RPM/6, 1) when zero — a 10-second burst window). PerIP, when true,
// scopes the bucket to the caller's IP address so a single tenant can't
// starve the endpoint for others.
type Limit struct {
	RPM   int  `json:"rpm"`
	Burst int  `json:"burst,omitempty"`
	PerIP bool `json:"perIP,omitempty"`
}

// Zero reports whether the limit is effectively disabled (RPM <= 0).
// Used by the middleware wrapper to short-circuit when no limit applies.
func (l Limit) Zero() bool { return l.RPM <= 0 }

// GlobalKey is the well-known key under which an app-wide rate limit is
// stored. Every request consults this bucket in addition to its per-op
// key; exhausting either denies the call. Kept as a constant so the
// dashboard can special-case the row ("Global" heading).
const GlobalKey = "_global"

// EffectiveBurst returns Burst if set, else a sensible default derived
// from RPM. Keeps callers from sprinkling max-logic everywhere.
func (l Limit) EffectiveBurst() int {
	if l.Burst > 0 {
		return l.Burst
	}
	if l.RPM <= 0 {
		return 0
	}
	b := l.RPM / 6
	if b < 1 {
		return 1
	}
	return b
}

// Record bundles a store entry for Snapshot listings — the declared
// baseline (from code) plus the effective limit (possibly overridden via
// the dashboard) for a single endpoint key.
type Record struct {
	Key       string `json:"key"`
	Declared  Limit  `json:"declared"`
	Effective Limit  `json:"effective"`
	// Overridden is true when Effective differs from Declared — the
	// operator tuned the limit live. Dashboard surfaces this as a badge
	// so someone reading the config doesn't mistake UI state for source
	// of truth.
	Overridden bool `json:"overridden,omitempty"`
}

// Store is the backend contract. Memory + Redis variants implement it.
// All methods are safe for concurrent use.
type Store interface {
	// Declare registers the compile-time declared limit for key. Called
	// at boot by the nexus auto-mount; overridden limits persist across
	// re-declarations so boot doesn't wipe operator tuning.
	Declare(key string, limit Limit)

	// Configure replaces the effective limit for key — the operator
	// override path. Returns the record it produced so the caller
	// (dashboard HTTP) can echo it back to the browser.
	Configure(ctx context.Context, key string, limit Limit) (Record, error)

	// Reset drops any operator override for key, reverting to the
	// declared baseline. No-op when nothing was overridden.
	Reset(ctx context.Context, key string) error

	// Allow is the enforcement call. scope is the IP-or-global bucket
	// identifier; the store combines it with key to isolate buckets.
	// Returns (ok, retryAfter). retryAfter is zero when ok=true.
	Allow(ctx context.Context, key, scope string) (bool, time.Duration)

	// Snapshot returns every key's current Record. Dashboard consumes
	// this on GET /__nexus/ratelimits.
	Snapshot(ctx context.Context) []Record
}

// NewMemoryStore returns an in-process token-bucket Store. Safe for
// single-instance deployments and tests; replace with a Redis-backed
// store when you need counters to survive restarts or shard across
// replicas.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		declared:  map[string]Limit{},
		effective: map[string]Limit{},
		buckets:   map[string]*bucket{},
	}
}

// MemoryStore is the default Store. Token-bucket per (key, scope): the
// bucket refills at Limit.RPM/60 tokens per second and holds at most
// Limit.EffectiveBurst() tokens. scope="" when PerIP is false.
type MemoryStore struct {
	mu        sync.RWMutex
	declared  map[string]Limit
	effective map[string]Limit
	buckets   map[string]*bucket
}

type bucket struct {
	mu        sync.Mutex
	tokens    float64
	lastTick  time.Time
	rpm       int // captured so a Configure that changed rpm resets
	burstCap  int
}

func (s *MemoryStore) Declare(key string, limit Limit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.declared[key] = limit
	// If no override exists, the effective limit tracks declared.
	if _, overridden := s.effective[key]; !overridden {
		s.effective[key] = limit
	}
}

func (s *MemoryStore) Configure(_ context.Context, key string, limit Limit) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.effective[key] = limit
	// Drop cached buckets for this key — next call rebuilds with the
	// new RPM/burst so the operator sees the change immediately.
	for bk := range s.buckets {
		if bucketKeyHasPrefix(bk, key) {
			delete(s.buckets, bk)
		}
	}
	return Record{
		Key:        key,
		Declared:   s.declared[key],
		Effective:  limit,
		Overridden: limit != s.declared[key],
	}, nil
}

func (s *MemoryStore) Reset(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if declared, ok := s.declared[key]; ok {
		s.effective[key] = declared
	} else {
		delete(s.effective, key)
	}
	for bk := range s.buckets {
		if bucketKeyHasPrefix(bk, key) {
			delete(s.buckets, bk)
		}
	}
	return nil
}

func (s *MemoryStore) Allow(_ context.Context, key, scope string) (bool, time.Duration) {
	s.mu.RLock()
	limit, ok := s.effective[key]
	s.mu.RUnlock()
	if !ok || limit.Zero() {
		return true, 0
	}

	bk := key + "|" + scope
	s.mu.Lock()
	b, exists := s.buckets[bk]
	if !exists {
		b = &bucket{
			tokens:   float64(limit.EffectiveBurst()),
			lastTick: time.Now(),
			rpm:      limit.RPM,
			burstCap: limit.EffectiveBurst(),
		}
		s.buckets[bk] = b
	}
	s.mu.Unlock()

	b.mu.Lock()
	defer b.mu.Unlock()

	// Refill based on time elapsed.
	now := time.Now()
	elapsed := now.Sub(b.lastTick).Seconds()
	b.lastTick = now
	b.tokens += elapsed * float64(limit.RPM) / 60.0
	cap := float64(limit.EffectiveBurst())
	if b.tokens > cap {
		b.tokens = cap
	}

	if b.tokens >= 1 {
		b.tokens -= 1
		return true, 0
	}
	// Tokens short — compute wait until one is available.
	needed := 1.0 - b.tokens
	wait := time.Duration(needed * 60.0 / float64(limit.RPM) * float64(time.Second))
	return false, wait
}

func (s *MemoryStore) Snapshot(_ context.Context) []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.declared)+len(s.effective))
	seen := map[string]struct{}{}
	for k := range s.declared {
		if _, ok := seen[k]; !ok {
			keys = append(keys, k)
			seen[k] = struct{}{}
		}
	}
	for k := range s.effective {
		if _, ok := seen[k]; !ok {
			keys = append(keys, k)
			seen[k] = struct{}{}
		}
	}
	out := make([]Record, 0, len(keys))
	for _, k := range keys {
		d := s.declared[k]
		e := s.effective[k]
		out = append(out, Record{
			Key:        k,
			Declared:   d,
			Effective:  e,
			Overridden: d != e,
		})
	}
	return out
}

// bucketKeyHasPrefix reports whether a "<key>|<scope>" bucket entry
// belongs to the given key. Extracted so the logic reads like English.
func bucketKeyHasPrefix(bucketKey, key string) bool {
	if len(bucketKey) < len(key)+1 {
		return false
	}
	return bucketKey[:len(key)] == key && bucketKey[len(key)] == '|'
}
