package visitors

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// persisted is the on-disk shape. Distinct from Counter's in-memory
// shape because the live state has unexported fields + a mutex; the
// JSON wire format is what the operator sees on disk and any tooling
// that reads the file.
//
// Forward compatibility: adding fields is safe (json.Unmarshal
// ignores unknowns); removing fields breaks rollback. Bump SchemaV
// when shape changes.
type persisted struct {
	SchemaV     int                `json:"schemaVersion"`
	TotalVisits int64              `json:"totalVisits"`
	UniqueIDs   []string           `json:"uniqueIds"`
	TodayKey    string             `json:"todayKey"`
	TodayVisits int64              `json:"todayVisits"`
	PathCounts  map[string]int64   `json:"pathCounts"`
	SavedAt     time.Time          `json:"savedAt"`
}

// schemaVersion bumps when the persisted shape changes incompatibly.
// v1 = the initial format defined here.
const schemaVersion = 1

// SaveToFile writes the current state atomically to path. Atomic =
// write to .tmp, fsync, rename. A crash during write leaves the
// previous file intact rather than truncated.
//
// lastSeen is NOT persisted — it's a transient liveness signal that
// doesn't survive restarts cleanly (a restart should reset "online
// now" to zero, because nobody is online if the process died).
func (c *Counter) SaveToFile(path string) error {
	if path == "" {
		return nil // disabled — no-op
	}
	c.mu.Lock()
	snap := persisted{
		SchemaV:     schemaVersion,
		TotalVisits: c.totalVisits,
		UniqueIDs:   make([]string, 0, len(c.uniqueIDs)),
		TodayKey:    c.todayKey,
		TodayVisits: c.todayVisits,
		PathCounts:  make(map[string]int64, len(c.pathCounts)),
		SavedAt:     time.Now().UTC(),
	}
	for id := range c.uniqueIDs {
		snap.UniqueIDs = append(snap.UniqueIDs, id)
	}
	for p, n := range c.pathCounts {
		snap.PathCounts[p] = n
	}
	c.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadFromFile restores state from a previous SaveToFile. Missing
// file is fine (first run); malformed JSON is an error (operator
// edited and broke it). schemaVersion mismatch is also an error —
// we'd rather refuse to load than silently dropping fields.
func (c *Counter) LoadFromFile(path string) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // first run, no file yet
		}
		return err
	}
	if len(data) == 0 {
		return nil
	}
	var snap persisted
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}
	if snap.SchemaV != 0 && snap.SchemaV != schemaVersion {
		// Older format the current binary doesn't understand —
		// refuse rather than guess. Operator can wipe the file
		// to start fresh.
		return errSchemaMismatch(snap.SchemaV)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.totalVisits = snap.TotalVisits
	c.todayKey = snap.TodayKey
	c.todayVisits = snap.TodayVisits
	// If the persisted day no longer matches today (we crossed
	// midnight while the process was down), reset today's bucket
	// so the dashboard's "Today" matches the calendar.
	if c.todayKey != today() {
		c.todayKey = today()
		c.todayVisits = 0
	}
	c.uniqueIDs = make(map[string]struct{}, len(snap.UniqueIDs))
	for _, id := range snap.UniqueIDs {
		c.uniqueIDs[id] = struct{}{}
	}
	if snap.PathCounts != nil {
		c.pathCounts = make(map[string]int64, len(snap.PathCounts))
		for p, n := range snap.PathCounts {
			c.pathCounts[p] = n
		}
	}
	return nil
}

type schemaMismatchErr struct{ version int }

func (e *schemaMismatchErr) Error() string {
	return "visitors: store schema version mismatch — file is " +
		formatInt(e.version) + ", binary expects " + formatInt(schemaVersion)
}

func errSchemaMismatch(v int) error { return &schemaMismatchErr{version: v} }

// formatInt is strconv.Itoa inlined so this file doesn't need an
// extra import for one call.
func formatInt(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
