package errors

import (
	"sort"
	"sync"
	"time"
)

// store is the in-memory ring buffer + issue index. Two views of the
// same data:
//
//   - "recent" — chronologically last N events, dropped oldest on
//     capacity overflow. What the dashboard's "Recent errors" feed
//     reads.
//   - "issues" — fingerprint → IssueSummary, with first-seen,
//     last-seen, count, and a sample event. What the dashboard's
//     "Grouped" view reads. Equivalent to Sentry's issues list.
//
// Concurrent reads + writes go through one mutex. The store is hot
// for writes (one per captured error) but cold for reads (dashboard
// requests). A sync.RWMutex is the right shape — reads don't fight
// each other; writes serialize.
type store struct {
	mu       sync.RWMutex
	capacity int
	recent   []Event              // ring buffer; len() <= capacity
	issues   map[string]*Issue    // fingerprint → issue
}

// Issue is the grouped view of a recurring error. One row per
// fingerprint; counts up on each new occurrence.
type Issue struct {
	Fingerprint string    `json:"fingerprint"`
	Method      string    `json:"method,omitempty"`
	Path        string    `json:"path,omitempty"`
	Service     string    `json:"service,omitempty"`
	Endpoint    string    `json:"endpoint,omitempty"`
	Error       string    `json:"error"`
	Status      int       `json:"status,omitempty"`
	Count       int       `json:"count"`
	FirstSeen   time.Time `json:"firstSeen"`
	LastSeen    time.Time `json:"lastSeen"`
	Sample      *Event    `json:"sample,omitempty"`
}

// newStore creates a store with the given ring-buffer capacity.
// capacity=0 is treated as "unbounded" — caller still pays memory
// for every captured error; useful in tests where determinism
// matters more than the upper bound.
func newStore(capacity int) *store {
	return &store{
		capacity: capacity,
		recent:   make([]Event, 0, capacityHint(capacity)),
		issues:   make(map[string]*Issue),
	}
}

func capacityHint(c int) int {
	if c <= 0 {
		return 32
	}
	return c
}

// add records a captured event into both views. Writes are
// fast-path: append to the ring + upsert into the issue map. We
// avoid copying Event because it's small (no slice fields) and
// callers don't retain references to the value they passed in.
func (s *store) add(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Recent view: append, evict oldest when full.
	if s.capacity > 0 && len(s.recent) >= s.capacity {
		// Shift left by 1. With small N (capacity is typically
		// 100–500), this is faster than ring-buffer arithmetic
		// because the slice stays a single contiguous block —
		// good for the JSON serializer's range loop later.
		copy(s.recent, s.recent[1:])
		s.recent = s.recent[:len(s.recent)-1]
	}
	s.recent = append(s.recent, e)

	// Issue view: upsert.
	issue, ok := s.issues[e.Fingerprint]
	if !ok {
		issue = &Issue{
			Fingerprint: e.Fingerprint,
			Method:      e.Method,
			Path:        e.Path,
			Service:     e.Service,
			Endpoint:    e.Endpoint,
			Error:       e.Error,
			Status:      e.Status,
			FirstSeen:   e.CapturedAt,
			Sample:      copyEvent(e),
		}
		s.issues[e.Fingerprint] = issue
	}
	issue.Count++
	issue.LastSeen = e.CapturedAt
	// Refresh the sample on each occurrence so the dashboard's
	// "view this issue" page shows the LATEST stack/error, not a
	// fossilized first-occurrence snapshot.
	issue.Sample = copyEvent(e)
}

// recentSnapshot returns a copy of the recent events, newest first.
// Copying upfront keeps the caller's iteration outside the lock.
func (s *store) recentSnapshot() []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Event, len(s.recent))
	// Reverse copy: newest entries first matches dashboard
	// expectations.
	for i, e := range s.recent {
		out[len(s.recent)-1-i] = e
	}
	return out
}

// issueSnapshot returns issues sorted by last-seen desc — same
// order Sentry's issues page uses.
func (s *store) issueSnapshot() []*Issue {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Issue, 0, len(s.issues))
	for _, iss := range s.issues {
		copy := *iss
		out = append(out, &copy)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out
}

// issueByFingerprint returns the issue + ok flag. Used by the
// dashboard's drill-down route.
func (s *store) issueByFingerprint(fp string) (*Issue, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	iss, ok := s.issues[fp]
	if !ok {
		return nil, false
	}
	copy := *iss
	return &copy, true
}

// clear wipes both views — exposed via /__nexus/errors/clear for
// operators who want to start fresh after fixing a flood (the
// dashboard's "Mark resolved" button posts here).
func (s *store) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recent = s.recent[:0]
	s.issues = make(map[string]*Issue)
}

// stats returns a small counts-only snapshot for the status endpoint.
func (s *store) stats() (recent, issues int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.recent), len(s.issues)
}

// copyEvent allocates a pointer-form copy so issue.Sample doesn't
// alias the recent slice entry (which is overwritten on capacity
// overflow). Pointer-form because Issue.Sample is *Event.
func copyEvent(e Event) *Event {
	dup := e
	return &dup
}
