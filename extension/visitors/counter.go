package visitors

import (
	"sort"
	"sync"
	"time"
)

// Counter is the live tally state — totals, uniques, today, top
// paths, and the rolling "online now" window. One Counter per
// plugin instance; HTTP handlers acquire it through pluginState.
//
// Concurrency: every mutation goes through the mutex. Reads of
// derived values (Stats(), Top()) snapshot under the mutex too, so
// callers don't see partial state. Throughput is fine for any
// real-world marketing site — even a /track ping every ms is
// nowhere near the lock contention threshold.
type Counter struct {
	mu sync.Mutex

	// Cumulative totals (since first boot, persisted across
	// restarts).
	totalVisits int64
	uniqueIDs   map[string]struct{} // set; size = unique visitors all-time

	// Daily breakdown — reset rotates on date change.
	todayKey    string // YYYY-MM-DD
	todayVisits int64

	// Live "online now" — visitorID → last-seen Time. Pruned on
	// every Stats() read past the window.
	lastSeen     map[string]time.Time
	onlineWindow time.Duration

	// Path tracking — bounded LRU-ish. We don't need true LRU
	// fidelity; on overflow we evict the lowest-count entry so
	// the "top N" view stays meaningful.
	pathCounts map[string]int64
	maxPaths   int
}

// NewCounter builds an empty Counter. Both maps are pre-allocated
// with small capacity hints — the typical site has under 1k unique
// visitors at any moment and under 50 distinct tracked paths.
func NewCounter(onlineWindow time.Duration, maxPaths int) *Counter {
	if maxPaths <= 0 {
		maxPaths = 100
	}
	return &Counter{
		uniqueIDs:    make(map[string]struct{}, 1024),
		lastSeen:     make(map[string]time.Time, 256),
		onlineWindow: onlineWindow,
		pathCounts:   make(map[string]int64, maxPaths),
		maxPaths:     maxPaths,
		todayKey:     today(),
	}
}

// Track records one visit. visitorID may be empty when the client
// has cookies disabled — those visits still bump totalVisits but
// don't contribute to unique-visitor / online-now counts (the
// cookieless visitors all collapse into the "" bucket).
//
// path is normalized by the caller (HTTP handler) to the SPA's
// route, not the wire URL — so "/" vs "/blog/post-1" tracks at the
// page level rather than the API level.
func (c *Counter) Track(visitorID, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Roll the daily counter if the date changed since last track.
	// Cheap: one string compare per request.
	if k := today(); k != c.todayKey {
		c.todayKey = k
		c.todayVisits = 0
	}

	c.totalVisits++
	c.todayVisits++

	if visitorID != "" {
		if _, seen := c.uniqueIDs[visitorID]; !seen {
			c.uniqueIDs[visitorID] = struct{}{}
		}
		c.lastSeen[visitorID] = time.Now()
	}

	if path != "" {
		if _, exists := c.pathCounts[path]; !exists && len(c.pathCounts) >= c.maxPaths {
			c.evictLowestPath()
		}
		c.pathCounts[path]++
	}
}

// Stats produces a snapshot suitable for the public API response.
// Cleans expired entries from lastSeen during the read — convenient
// time to do it (the map is already touched), avoids needing a
// separate pruning goroutine.
func (c *Counter) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneOnlineLocked()

	if k := today(); k != c.todayKey {
		c.todayKey = k
		c.todayVisits = 0
	}

	return Stats{
		Total:        c.totalVisits,
		Unique:       int64(len(c.uniqueIDs)),
		Today:        c.todayVisits,
		OnlineNow:    int64(len(c.lastSeen)),
		TopPathCount: int64(len(c.pathCounts)),
	}
}

// Top returns the N highest-traffic paths in descending order. N is
// clamped to maxPaths (the configured ceiling).
func (c *Counter) Top(n int) []PathCount {
	c.mu.Lock()
	defer c.mu.Unlock()
	pairs := make([]PathCount, 0, len(c.pathCounts))
	for p, n := range c.pathCounts {
		pairs = append(pairs, PathCount{Path: p, Count: n})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].Count > pairs[j].Count })
	if n > 0 && n < len(pairs) {
		pairs = pairs[:n]
	}
	return pairs
}

// Reset wipes everything — for /admin/reset. Total + unique + path
// + lastSeen all go back to zero. Doesn't touch the on-disk file
// directly; caller follows with SaveToFile() to persist the wipe.
func (c *Counter) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.totalVisits = 0
	c.todayVisits = 0
	c.todayKey = today()
	c.uniqueIDs = make(map[string]struct{})
	c.lastSeen = make(map[string]time.Time)
	c.pathCounts = make(map[string]int64)
}

// Stats is the public-facing snapshot. JSON tags match the
// frontend's TS interface in nexus-cloud's useVisitors composable.
type Stats struct {
	Total        int64 `json:"total"`
	Unique       int64 `json:"unique"`
	Today        int64 `json:"today"`
	OnlineNow    int64 `json:"onlineNow"`
	TopPathCount int64 `json:"topPathCount"` // count of distinct paths tracked
}

// PathCount is one row in the /top response.
type PathCount struct {
	Path  string `json:"path"`
	Count int64  `json:"count"`
}

// pruneOnlineLocked drops visitors whose lastSeen is older than the
// online window. Called with the mutex already held — the Locked
// suffix is the package convention for "caller must hold the lock".
func (c *Counter) pruneOnlineLocked() {
	cutoff := time.Now().Add(-c.onlineWindow)
	for id, seen := range c.lastSeen {
		if seen.Before(cutoff) {
			delete(c.lastSeen, id)
		}
	}
}

// evictLowestPath drops the smallest-count path entry. Crude — not
// real LRU, but good enough for the use case. Called when the
// pathCounts map hits its size cap and a new path tries to register.
// Without this, a hostile client pinging /track with random paths
// could blow memory.
func (c *Counter) evictLowestPath() {
	var minPath string
	var minCount int64 = -1
	for p, n := range c.pathCounts {
		if minCount < 0 || n < minCount {
			minPath, minCount = p, n
		}
	}
	if minPath != "" {
		delete(c.pathCounts, minPath)
	}
}

// today returns the current date as YYYY-MM-DD in UTC. Used as the
// daily-rollover key. UTC (not local) so a multi-region deploy
// doesn't shift the day around mid-window.
func today() string {
	return time.Now().UTC().Format("2006-01-02")
}
