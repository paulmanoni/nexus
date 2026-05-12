package visitors

import (
	"path/filepath"
	"testing"
	"time"
)

// TestCounter_TrackBasic covers the fast path — one visitor pings
// twice. Total goes to 2; unique stays at 1; online-now is 1.
func TestCounter_TrackBasic(t *testing.T) {
	t.Parallel()
	c := NewCounter(60*time.Second, 100)
	c.Track("v1", "/")
	c.Track("v1", "/blog")
	s := c.Stats()
	if s.Total != 2 {
		t.Errorf("Total: got %d, want 2", s.Total)
	}
	if s.Unique != 1 {
		t.Errorf("Unique: got %d, want 1", s.Unique)
	}
	if s.OnlineNow != 1 {
		t.Errorf("OnlineNow: got %d, want 1", s.OnlineNow)
	}
	if s.Today != 2 {
		t.Errorf("Today: got %d, want 2", s.Today)
	}
}

// TestCounter_OnlinePrune verifies the online-now window. A visitor
// who hasn't pinged in the window's lifetime falls out of OnlineNow
// but stays in Unique.
func TestCounter_OnlinePrune(t *testing.T) {
	t.Parallel()
	c := NewCounter(20*time.Millisecond, 100)
	c.Track("v1", "/")
	c.Track("v2", "/")

	if got := c.Stats().OnlineNow; got != 2 {
		t.Fatalf("OnlineNow before prune: got %d, want 2", got)
	}

	// Sleep past the window so both lastSeen entries are stale.
	time.Sleep(40 * time.Millisecond)

	s := c.Stats()
	if s.OnlineNow != 0 {
		t.Errorf("OnlineNow after prune: got %d, want 0", s.OnlineNow)
	}
	if s.Unique != 2 {
		t.Errorf("Unique after prune: got %d, want 2 (still all-time)", s.Unique)
	}
}

// TestCounter_EmptyVisitorIDBucket — cookieless visitors all
// collapse into the "" bucket but their visits still increment
// Total. This is deliberate: we don't fingerprint, so cookie-
// blocked visitors look like one shared person rather than many
// anonymous ones.
func TestCounter_EmptyVisitorIDBucket(t *testing.T) {
	t.Parallel()
	c := NewCounter(60*time.Second, 100)
	c.Track("", "/")
	c.Track("", "/blog")
	c.Track("", "/")

	s := c.Stats()
	if s.Total != 3 {
		t.Errorf("Total: got %d, want 3", s.Total)
	}
	// uniqueIDs and lastSeen are not touched for empty IDs.
	if s.Unique != 0 {
		t.Errorf("Unique: got %d, want 0 (no cookies, no unique tracking)", s.Unique)
	}
	if s.OnlineNow != 0 {
		t.Errorf("OnlineNow: got %d, want 0", s.OnlineNow)
	}
}

// TestCounter_TopPathsRanking — the most-hit path lands at the top
// of the /top response.
func TestCounter_TopPathsRanking(t *testing.T) {
	t.Parallel()
	c := NewCounter(60*time.Second, 100)
	for i := 0; i < 5; i++ {
		c.Track("v1", "/")
	}
	for i := 0; i < 3; i++ {
		c.Track("v1", "/blog")
	}
	c.Track("v1", "/about")

	top := c.Top(10)
	if len(top) != 3 {
		t.Fatalf("want 3 paths, got %d", len(top))
	}
	if top[0].Path != "/" || top[0].Count != 5 {
		t.Errorf("rank 0: got %+v, want /=5", top[0])
	}
	if top[1].Path != "/blog" || top[1].Count != 3 {
		t.Errorf("rank 1: got %+v, want /blog=3", top[1])
	}
	if top[2].Path != "/about" || top[2].Count != 1 {
		t.Errorf("rank 2: got %+v, want /about=1", top[2])
	}
}

// TestCounter_PathEvictionAtCap — past the maxPaths cap, the
// lowest-count entry gets evicted to make room. Without this, a
// malicious /track ping flood with random paths would blow memory.
func TestCounter_PathEvictionAtCap(t *testing.T) {
	t.Parallel()
	c := NewCounter(60*time.Second, 3)
	// Fill cap with three paths of varied counts.
	for i := 0; i < 10; i++ {
		c.Track("v1", "/heavy")
	}
	for i := 0; i < 5; i++ {
		c.Track("v1", "/medium")
	}
	c.Track("v1", "/light")

	// One more path → the lightest gets evicted.
	c.Track("v1", "/new")

	top := c.Top(10)
	paths := map[string]bool{}
	for _, p := range top {
		paths[p.Path] = true
	}
	if paths["/light"] {
		t.Errorf("/light should have been evicted (lowest count)")
	}
	if !paths["/heavy"] || !paths["/medium"] || !paths["/new"] {
		t.Errorf("eviction kicked the wrong path: %v", paths)
	}
}

// TestSaveLoadRoundTrip pins the JSON persistence shape. Save a
// counter; load it back into a fresh Counter; assert the totals
// + uniques + paths match.
func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	a := NewCounter(60*time.Second, 100)
	a.Track("v1", "/")
	a.Track("v1", "/blog")
	a.Track("v2", "/")
	a.Track("v2", "/")

	path := filepath.Join(t.TempDir(), "v.json")
	if err := a.SaveToFile(path); err != nil {
		t.Fatal(err)
	}

	b := NewCounter(60*time.Second, 100)
	if err := b.LoadFromFile(path); err != nil {
		t.Fatal(err)
	}

	sa, sb := a.Stats(), b.Stats()
	// OnlineNow is intentionally NOT persisted — a restart means
	// nobody is online by definition. So we compare everything
	// EXCEPT OnlineNow.
	if sa.Total != sb.Total {
		t.Errorf("Total: a=%d b=%d", sa.Total, sb.Total)
	}
	if sa.Unique != sb.Unique {
		t.Errorf("Unique: a=%d b=%d", sa.Unique, sb.Unique)
	}
	if sb.OnlineNow != 0 {
		t.Errorf("OnlineNow after load: want 0, got %d (lastSeen should NOT persist)", sb.OnlineNow)
	}

	topB := b.Top(10)
	if len(topB) != 2 {
		t.Fatalf("loaded top paths: want 2, got %d", len(topB))
	}
}

// TestLoad_MissingFileNotAnError — first-run case. No file on
// disk → LoadFromFile returns nil without populating anything.
func TestLoad_MissingFileNotAnError(t *testing.T) {
	t.Parallel()
	c := NewCounter(60*time.Second, 100)
	err := c.LoadFromFile(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("want nil for missing file, got %v", err)
	}
	if c.Stats().Total != 0 {
		t.Errorf("expected empty counter, got %+v", c.Stats())
	}
}

// TestReset wipes counts. Use case: operator hits /__nexus/visitors/reset
// after a bot flood.
func TestReset(t *testing.T) {
	t.Parallel()
	c := NewCounter(60*time.Second, 100)
	c.Track("v1", "/")
	c.Track("v2", "/")
	c.Reset()
	s := c.Stats()
	if s.Total != 0 || s.Unique != 0 || s.OnlineNow != 0 {
		t.Errorf("reset failed: %+v", s)
	}
}
