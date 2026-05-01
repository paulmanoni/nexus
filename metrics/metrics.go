// Package metrics records per-endpoint request counts + errors so the
// dashboard can show at-a-glance health next to each op: how busy it is,
// whether it's failing, and if so, what the last error was.
//
// The middleware layer lives in middleware.go; this file is the data
// plane — a Store interface + in-memory implementation. Swap the store
// for a Prometheus- or StatsD-backed version later without touching the
// handlers or UI.
package metrics

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/paulmanoni/nexus/trace"
)

// ErrorEvent captures one error occurrence for the dashboard's click-
// through panel. Rolling per-endpoint ring keeps the latest few so
// operators see both "who's hitting this" (IP) and "what's wrong"
// (message) without flipping between tabs.
type ErrorEvent struct {
	Timestamp time.Time `json:"timestamp"`
	IP        string    `json:"ip,omitempty"`
	Message   string    `json:"message"`
	// Stack is the captured Go runtime stack trace when the error
	// originated from a panic (extracted via trace.StackOf). Empty
	// for plain returned errors — those don't carry a stack unless
	// user code explicitly wraps them in a *trace.StackError.
	Stack string `json:"stack,omitempty"`
}

// RecentErrorsCap is the ring-buffer depth per endpoint. Large enough
// to survive a burst without losing context; /stats polling strips the
// events so the hot path doesn't pay for this depth — the dashboard
// fetches the full ring on demand via the per-op errors endpoint.
const RecentErrorsCap = 1000

// EndpointStats is the serializable per-op snapshot the dashboard shows
// next to each op row on the Architecture tab — request counter, error
// counter, plus the most recent error's message and timestamp for a
// "what broke?" quick read.
//
// RecentErrors is a ring-capped list of recent error events (most recent
// first) so the UI can pop a dialog with timestamps + IPs + messages
// when an operator clicks the error badge.
type EndpointStats struct {
	Key           string       `json:"key"`
	Count         int64        `json:"count"`
	Errors        int64        `json:"errors"`
	LastError     string       `json:"lastError,omitempty"`
	// LastErrStack is the stack trace captured at the most recent
	// errored request (when the error was a panic wrapped via
	// trace.StackError). Surfaces in the drawer's last-error panel as
	// a collapsible disclosure so an operator can see the failing
	// call chain without opening the per-op error ring.
	LastErrStack  string       `json:"lastErrStack,omitempty"`
	LastAt        time.Time    `json:"lastAt,omitempty"`      // time of last request
	LastErrAt     time.Time    `json:"lastErrAt,omitempty"`   // time of last errored request
	RecentErrors  []ErrorEvent `json:"recentErrors,omitempty"`
}

// Store is the backend contract. Record is called once per request with
// nil err on success; Snapshot returns every key's current stats for the
// dashboard. Safe for concurrent use.
type Store interface {
	// Record one request outcome against key. ip is the caller's client
	// address (empty when unavailable); err is the handler's return
	// (nil on success). Error events get stashed in a per-endpoint ring
	// so the dashboard can surface IP + message together.
	Record(key, ip string, err error)

	// Snapshot returns a point-in-time copy of every key's stats. The
	// returned EndpointStats omits RecentErrors so the /stats polling
	// payload stays small at any ring depth; use Errors(key) for the
	// full per-endpoint error list. Sorted by key.
	Snapshot() []EndpointStats

	// Get returns a single endpoint's stats (without RecentErrors).
	Get(key string) (EndpointStats, bool)

	// Errors returns the ring of recent error events for key, newest
	// first. Called by the dashboard when a user clicks the error
	// badge — lazy fetch means a 1000-entry ring doesn't inflate the
	// hot-polling /stats payload.
	Errors(key string) []ErrorEvent
}

// NewMemoryStore returns an in-process Store. Atomic counters keep the
// hot path lock-free; a mutex only guards LastError / LastAt updates.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{entries: map[string]*entry{}}
}

// MemoryStore is the default Store. Per-key entries use atomic ints for
// counters and a mutex for the last-error text, which mutates rarely
// compared to the counter path.
type MemoryStore struct {
	mu      sync.RWMutex
	entries map[string]*entry
}

type entry struct {
	count        atomic.Int64
	errors       atomic.Int64
	mu           sync.Mutex
	lastErr      string
	lastErrStack string
	lastAt       time.Time
	lastErrAt    time.Time
	// recent is a newest-first ring of error events. Size capped at
	// RecentErrorsCap; oldest entries drop as new ones arrive.
	recent []ErrorEvent
}

func (s *MemoryStore) pick(key string) *entry {
	s.mu.RLock()
	e, ok := s.entries[key]
	s.mu.RUnlock()
	if ok {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key]; ok {
		return e
	}
	e = &entry{}
	s.entries[key] = e
	return e
}

func (s *MemoryStore) Record(key, ip string, err error) {
	e := s.pick(key)
	e.count.Add(1)
	now := time.Now()
	e.mu.Lock()
	e.lastAt = now
	if err != nil {
		e.errors.Add(1)
		e.lastErr = err.Error()
		e.lastErrStack = trace.StackOf(err)
		e.lastErrAt = now
		// newest-first insertion; cap ring length so old events drop.
		// trace.StackOf walks the error chain via errors.As so wrapped
		// stack-carriers still surface their stack trace here.
		ev := ErrorEvent{
			Timestamp: now,
			IP:        ip,
			Message:   err.Error(),
			Stack:     trace.StackOf(err),
		}
		e.recent = append([]ErrorEvent{ev}, e.recent...)
		if len(e.recent) > RecentErrorsCap {
			e.recent = e.recent[:RecentErrorsCap]
		}
	}
	e.mu.Unlock()
}

func (s *MemoryStore) Get(key string) (EndpointStats, bool) {
	s.mu.RLock()
	e, ok := s.entries[key]
	s.mu.RUnlock()
	if !ok {
		return EndpointStats{}, false
	}
	return snapshotOf(key, e), true
}

func (s *MemoryStore) Snapshot() []EndpointStats {
	s.mu.RLock()
	keys := make([]string, 0, len(s.entries))
	for k := range s.entries {
		keys = append(keys, k)
	}
	entries := make([]*entry, 0, len(keys))
	for _, k := range keys {
		entries = append(entries, s.entries[k])
	}
	s.mu.RUnlock()

	out := make([]EndpointStats, len(keys))
	for i, k := range keys {
		out[i] = snapshotOf(k, entries[i])
	}
	sortByKey(out)
	return out
}

func snapshotOf(key string, e *entry) EndpointStats {
	e.mu.Lock()
	lastErr := e.lastErr
	lastErrStack := e.lastErrStack
	lastAt := e.lastAt
	lastErrAt := e.lastErrAt
	e.mu.Unlock()
	// RecentErrors intentionally omitted — /stats polling stays lean.
	// The dashboard's error dialog fetches the full ring via Errors(key).
	return EndpointStats{
		Key:          key,
		Count:        e.count.Load(),
		Errors:       e.errors.Load(),
		LastError:    lastErr,
		LastErrStack: lastErrStack,
		LastAt:       lastAt,
		LastErrAt:    lastErrAt,
	}
}

// Errors returns the ring of recent error events for key, newest first.
// Empty slice when the key is unknown. Safe to call concurrently with
// Record — we snapshot under the entry's lock before returning.
func (s *MemoryStore) Errors(key string) []ErrorEvent {
	s.mu.RLock()
	e, ok := s.entries[key]
	s.mu.RUnlock()
	if !ok {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.recent) == 0 {
		return nil
	}
	out := make([]ErrorEvent, len(e.recent))
	copy(out, e.recent)
	return out
}

// sortByKey does a small-N insertion sort — the keys slice is always the
// number of distinct endpoints in the app (dozens at most), so stdlib
// sort's overhead isn't worth the import.
func sortByKey(xs []EndpointStats) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1].Key > xs[j].Key; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}
