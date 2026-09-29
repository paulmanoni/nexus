package jobs

import (
	"testing"
	"time"
)

// A snapshot taken while a job runs restores it queued, its attempt
// uncounted, so the rebuilt process runs it again.
func TestMemoryStoreDevRestore(t *testing.T) {
	now := time.Now()
	s := newMemoryStore()
	s.insert(&Record{ID: "a", Name: "j", Queue: "default", State: StateQueued, RunAt: now}, now)
	s.insert(&Record{ID: "b", Name: "j", Queue: "default", State: StateQueued, RunAt: now}, now)
	if _, ok := s.claim("default", now); !ok {
		t.Fatal("claim failed")
	}
	snap, err := s.SnapshotDev()
	if err != nil {
		t.Fatal(err)
	}
	restored := newMemoryStore()
	if err := restored.RestoreDev(snap); err != nil {
		t.Fatal(err)
	}
	a, _ := restored.get("a")
	if a.State != StateQueued || a.Attempt != 0 {
		t.Fatalf("running job restored as %s/%d, want queued/0", a.State, a.Attempt)
	}
	if first, _ := restored.claim("default", now); first.ID != "a" {
		t.Fatalf("restore lost enqueue order: claimed %s first", first.ID)
	}
}

func TestMemoryStorePrune(t *testing.T) {
	now := time.Now()
	s := newMemoryStore()
	for i, st := range []State{StateSucceeded, StateFailed, StateSucceeded, StateQueued} {
		id := ID(rune('a' + i))
		s.insert(&Record{ID: id, State: st, FinishedAt: now.Add(-time.Duration(3-i) * time.Hour)}, now)
	}
	s.prune(now, 150*time.Minute, 1)
	var left []ID
	for _, r := range s.list(nil) {
		left = append(left, r.ID)
	}
	// "a" is past retention; of "b" and "c" only the newest finished one
	// fits the limit of 1; "d" is still queued.
	if len(left) != 2 || left[0] != "d" || left[1] != "c" {
		t.Fatalf("after prune: %v", left)
	}
}
