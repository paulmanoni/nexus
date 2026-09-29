package jobs

import (
	"context"
	"testing"
	"time"
)

var bg = context.Background()

// A snapshot taken while a job runs restores it queued, its attempt
// uncounted, so the rebuilt process runs it again.
func TestMemoryStoreDevRestore(t *testing.T) {
	now := time.Now()
	s := newMemoryStore()
	_, _, _ = s.Insert(bg, Record{ID: "a", Name: "j", Queue: "default", State: StateQueued, RunAt: now, CreatedAt: now})
	_, _, _ = s.Insert(bg, Record{ID: "b", Name: "j", Queue: "default", State: StateQueued, RunAt: now, CreatedAt: now.Add(time.Millisecond)})
	if _, ok, _ := s.Claim(bg, "default", "w1", now, time.Minute); !ok {
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
	a, _, _ := restored.Get(bg, "a")
	if a.State != StateQueued || a.Attempt != 0 || a.Worker != "" {
		t.Fatalf("running job restored as %s/%d/%q, want queued/0/none", a.State, a.Attempt, a.Worker)
	}
	if first, _, _ := restored.Claim(bg, "default", "w2", now, time.Minute); first.ID != "a" {
		t.Fatalf("restore lost enqueue order: claimed %s first", first.ID)
	}
}

func TestMemoryStorePrune(t *testing.T) {
	now := time.Now()
	s := newMemoryStore()
	for i, st := range []State{StateSucceeded, StateFailed, StateSucceeded, StateQueued} {
		id := ID(rune('a' + i))
		_, _, _ = s.Insert(bg, Record{ID: id, State: st, FinishedAt: now.Add(-time.Duration(3-i) * time.Hour)})
	}
	_ = s.Prune(bg, now.Add(-150*time.Minute), 1)
	recs, _ := s.List(bg, Filter{})
	var left []ID
	for _, r := range recs {
		left = append(left, r.ID)
	}
	// "a" is past retention; of "b" and "c" only the newest finished one
	// fits the limit of 1; "d" is still queued.
	if len(left) != 2 || left[0] != "d" || left[1] != "c" {
		t.Fatalf("after prune: %v", left)
	}
}

// Inserting an ID that exists is a no-op — what makes a schedule tick
// enqueue once.
func TestMemoryStoreInsertExistingID(t *testing.T) {
	now := time.Now()
	s := newMemoryStore()
	_, first, _ := s.Insert(bg, Record{ID: "x", Name: "one", CreatedAt: now})
	id, second, _ := s.Insert(bg, Record{ID: "x", Name: "two", CreatedAt: now})
	rec, _, _ := s.Get(bg, "x")
	if !first || second || id != "x" || rec.Name != "one" {
		t.Fatalf("first %v second %v name %s", first, second, rec.Name)
	}
}
