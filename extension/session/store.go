package session

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/paulmanoni/nexus"
)

// Store persists session data by ID. Implementations must be safe for
// concurrent use. Load returns (nil, nil) for an unknown or expired
// ID — absence is not an error; a fresh session simply starts empty.
//
// The built-in stores serialize data as JSON, so values must
// round-trip through it (Django's JSONSerializer has the same rule).
type Store interface {
	Load(ctx context.Context, id string) (map[string]any, error)
	Save(ctx context.Context, id string, data map[string]any, ttl time.Duration) error
	Delete(ctx context.Context, id string) error
}

// ── memory store ────────────────────────────────────────────────────

// MemoryStore is the default: an in-process TTL map. Right for dev
// (it survives `nexus dev` rebuilds via the dev-state machinery) and
// single-replica apps that accept logout-on-restart; production wants
// CacheStore or a DB-backed Store.
type MemoryStore struct {
	mu        sync.RWMutex
	entries   map[string]memEntry
	maxItems  int
	lastSweep time.Time
}

type memEntry struct {
	Data      map[string]any `json:"data"`
	ExpiresAt time.Time      `json:"expiresAt"`
}

// memStoreMax bounds the map — session IDs arrive on cookies, i.e.
// client-supplied input, and an unbounded map keyed by them is a
// memory sink. At the cap, one sweep runs and then the oldest entry
// is dropped.
const memStoreMax = 65536

const memSweepEvery = time.Minute

// NewMemoryStore returns an empty in-process store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{entries: map[string]memEntry{}}
}

func (m *MemoryStore) Load(_ context.Context, id string) (map[string]any, error) {
	m.mu.RLock()
	e, ok := m.entries[id]
	m.mu.RUnlock()
	if !ok || time.Now().After(e.ExpiresAt) {
		return nil, nil
	}
	// Hand the caller its own copy: the request mutates the map, and
	// the stored one must stay consistent until Save.
	cp := make(map[string]any, len(e.Data))
	for k, v := range e.Data {
		cp[k] = v
	}
	return cp, nil
}

func (m *MemoryStore) Save(_ context.Context, id string, data map[string]any, ttl time.Duration) error {
	cp := make(map[string]any, len(data))
	for k, v := range data {
		cp[k] = v
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if now.Sub(m.lastSweep) >= memSweepEvery {
		m.lastSweep = now
		for k, e := range m.entries {
			if now.After(e.ExpiresAt) {
				delete(m.entries, k)
			}
		}
	}
	if len(m.entries) >= memStoreMax {
		var oldestKey string
		var oldestAt time.Time
		first := true
		for k, e := range m.entries {
			if first || e.ExpiresAt.Before(oldestAt) {
				oldestKey, oldestAt, first = k, e.ExpiresAt, false
			}
		}
		if oldestKey != "" {
			delete(m.entries, oldestKey)
		}
	}
	m.entries[id] = memEntry{Data: cp, ExpiresAt: now.Add(ttl)}
	return nil
}

func (m *MemoryStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	delete(m.entries, id)
	m.mu.Unlock()
	return nil
}

// SnapshotDev / RestoreDev implement nexus.DevState so sessions
// survive `nexus dev` rebuilds — same pattern as auth.MemoryUserStore.
// Expired entries are dropped at snapshot time.
func (m *MemoryStore) SnapshotDev() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	now := time.Now()
	live := make(map[string]memEntry, len(m.entries))
	for k, e := range m.entries {
		if now.Before(e.ExpiresAt) {
			live[k] = e
		}
	}
	return json.Marshal(live)
}

func (m *MemoryStore) RestoreDev(data []byte) error {
	var entries map[string]memEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}
	m.mu.Lock()
	// Entries the new process wrote before restore win, matching
	// PreserveDev's documented merge rule.
	for k, e := range entries {
		if _, exists := m.entries[k]; !exists {
			m.entries[k] = e
		}
	}
	m.mu.Unlock()
	return nil
}

// ── cache store ─────────────────────────────────────────────────────

// CacheStore rides any nexus.Cache — with the Redis backend imported
// (extension/cache/redis) sessions survive restarts and are visible
// to every replica, the production shape. Keys are prefixed
// "session:" so they coexist with the app's own cache traffic.
//
//	type Sessions struct{ *cache.Manager }
//	// … cache.BindFromConfig[Sessions]("session") …
//	session.Module(session.Config{Store: session.CacheStore(mgr)})
//
// Or hand it the app's default cache: session.CacheStore(app.Cache()).
func CacheStore(c nexus.Cache) Store {
	return &cacheStore{c: c}
}

type cacheStore struct{ c nexus.Cache }

const cacheKeyPrefix = "session:"

func (s *cacheStore) Load(ctx context.Context, id string) (map[string]any, error) {
	var data map[string]any
	err := s.c.Get(ctx, cacheKeyPrefix+id, &data)
	if err != nil {
		// Cache misses surface as errors from nexus.Cache; a fresh
		// session is the correct outcome either way.
		return nil, nil //nolint:nilerr
	}
	return data, nil
}

func (s *cacheStore) Save(ctx context.Context, id string, data map[string]any, ttl time.Duration) error {
	return s.c.Set(ctx, cacheKeyPrefix+id, data, ttl)
}

func (s *cacheStore) Delete(ctx context.Context, id string) error {
	err := s.c.Delete(ctx, cacheKeyPrefix+id)
	if errors.Is(err, context.Canceled) {
		return err
	}
	// A delete of an absent key is success.
	return nil
}
