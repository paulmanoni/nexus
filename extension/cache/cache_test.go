package cache

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return NewManager(&Config{
		Environment:   "development",
		DefaultExpiry: time.Minute,
		CleanupExpiry: time.Minute,
	}, nil)
}

type payload struct {
	Name  string
	Count int
}

func TestMemory_SetGet(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()

	if err := m.Set(ctx, "k", payload{Name: "rex", Count: 3}, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var got payload
	if err := m.Get(ctx, "k", &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "rex" || got.Count != 3 {
		t.Errorf("got %+v, want {rex 3}", got)
	}
}

func TestMemory_MissReturnsErrNotFound(t *testing.T) {
	m := newTestManager(t)
	var got payload
	err := m.Get(context.Background(), "absent", &got)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("miss err = %v, want ErrNotFound", err)
	}
}

// TestMemory_ValueSemantics verifies the memory backend serializes (rather
// than aliasing) — mutating the original after Set must not change the cached
// copy, matching the Redis backend.
func TestMemory_ValueSemantics(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	p := payload{Name: "a", Count: 1}
	if err := m.Set(ctx, "k", p, time.Minute); err != nil {
		t.Fatal(err)
	}
	p.Name = "mutated" // must not affect the cached entry
	var got payload
	if err := m.Get(ctx, "k", &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "a" {
		t.Errorf("cached value aliased the caller's struct: got %q", got.Name)
	}
}

func TestMemory_TTLExpiry(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	if err := m.Set(ctx, "k", payload{Name: "x"}, 30*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond)
	var got payload
	if err := m.Get(ctx, "k", &got); !errors.Is(err, ErrNotFound) {
		t.Errorf("after TTL, Get err = %v, want ErrNotFound", err)
	}
}

func TestMemory_DeleteAndClear(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	_ = m.Set(ctx, "a", payload{Count: 1}, time.Minute)
	_ = m.Set(ctx, "b", payload{Count: 2}, time.Minute)

	if err := m.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	var got payload
	if err := m.Get(ctx, "a", &got); !errors.Is(err, ErrNotFound) {
		t.Errorf("after Delete, Get err = %v, want ErrNotFound", err)
	}

	if err := m.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Get(ctx, "b", &got); !errors.Is(err, ErrNotFound) {
		t.Errorf("after Clear, Get err = %v, want ErrNotFound", err)
	}
}

// TestProductionWithoutRedisImport_StaysMemory verifies the key property of
// the split: a production-mode Manager whose binary did NOT import the redis
// backend never engages Redis — Start is a no-op beyond persistence, and the
// cache serves from memory.
func TestProductionWithoutRedisImport_StaysMemory(t *testing.T) {
	m := NewManager(&Config{Environment: "production", DefaultExpiry: time.Minute}, nil)
	m.Start()
	defer m.Stop()
	if m.IsRedisConnected() {
		t.Error("Redis engaged without the redis backend imported")
	}
	ctx := context.Background()
	if err := m.Set(ctx, "k", payload{Count: 7}, time.Minute); err != nil {
		t.Fatal(err)
	}
	var got payload
	if err := m.Get(ctx, "k", &got); err != nil || got.Count != 7 {
		t.Errorf("memory path broken in production mode: got %+v err %v", got, err)
	}
}

// TestPersistRoundTrip exercises the dev-cache snapshot: Set, snapshot on
// Stop, then a fresh Manager restores it on Start.
func TestPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dev-cache.gob")
	ctx := context.Background()

	m1 := NewManager(&Config{DefaultExpiry: time.Minute, PersistPath: path}, nil)
	m1.Start()
	if err := m1.Set(ctx, "tok", payload{Name: "session", Count: 1}, time.Minute); err != nil {
		t.Fatal(err)
	}
	m1.Stop() // writes the snapshot

	m2 := NewManager(&Config{DefaultExpiry: time.Minute, PersistPath: path}, nil)
	m2.Start()
	defer m2.Stop()
	var got payload
	if err := m2.Get(ctx, "tok", &got); err != nil {
		t.Fatalf("restored Get: %v", err)
	}
	if got.Name != "session" {
		t.Errorf("restored value = %+v, want session", got)
	}
}
