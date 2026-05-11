package ratelimit

import (
	"context"
	"testing"
	"time"
)

func TestLimit_EffectiveBurst(t *testing.T) {
	cases := []struct {
		name string
		in   Limit
		want int
	}{
		{"explicit burst", Limit{RPM: 60, Burst: 30}, 30},
		{"default burst from RPM", Limit{RPM: 60}, 10},
		{"min 1 for small RPM", Limit{RPM: 1}, 1},
		{"disabled", Limit{}, 0},
	}
	for _, c := range cases {
		if got := c.in.EffectiveBurst(); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestMemoryStore_AllowWithinBurst(t *testing.T) {
	s := NewMemoryStore()
	s.Declare("adverts.getAllAdverts", Limit{RPM: 60, Burst: 3})

	for i := 0; i < 3; i++ {
		ok, _ := s.Allow(context.Background(), "adverts.getAllAdverts", "")
		if !ok {
			t.Fatalf("call %d: expected allowed", i+1)
		}
	}
	ok, wait := s.Allow(context.Background(), "adverts.getAllAdverts", "")
	if ok {
		t.Fatal("call 4: expected denied (burst exhausted)")
	}
	if wait <= 0 {
		t.Errorf("expected positive retry-after, got %v", wait)
	}
}

func TestMemoryStore_RefillsOverTime(t *testing.T) {
	s := NewMemoryStore()
	s.Declare("k", Limit{RPM: 600, Burst: 1}) // 10 req/sec, burst 1
	ctx := context.Background()

	// Consume the single token.
	ok, _ := s.Allow(ctx, "k", "")
	if !ok {
		t.Fatal("first call should be allowed")
	}
	ok, _ = s.Allow(ctx, "k", "")
	if ok {
		t.Fatal("second immediate call should be denied")
	}
	// Wait long enough for one token to refill (100ms @ 600rpm = 1 token).
	time.Sleep(150 * time.Millisecond)
	ok, _ = s.Allow(ctx, "k", "")
	if !ok {
		t.Fatal("call after refill window should be allowed")
	}
}

func TestMemoryStore_PerIPIsolation(t *testing.T) {
	s := NewMemoryStore()
	s.Declare("k", Limit{RPM: 60, Burst: 1, PerIP: true})
	ctx := context.Background()

	// Caller Allow with different scopes — burst is 1 per scope.
	if ok, _ := s.Allow(ctx, "k", "1.1.1.1"); !ok {
		t.Fatal("1.1.1.1 first call denied")
	}
	if ok, _ := s.Allow(ctx, "k", "2.2.2.2"); !ok {
		t.Fatal("2.2.2.2 first call denied")
	}
	if ok, _ := s.Allow(ctx, "k", "1.1.1.1"); ok {
		t.Fatal("1.1.1.1 second call should be denied")
	}
	if ok, _ := s.Allow(ctx, "k", "2.2.2.2"); ok {
		t.Fatal("2.2.2.2 second call should be denied")
	}
}

func TestMemoryStore_ConfigureOverrides(t *testing.T) {
	s := NewMemoryStore()
	s.Declare("k", Limit{RPM: 60})
	rec, err := s.Configure(context.Background(), "k", Limit{RPM: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Overridden {
		t.Error("record should be flagged overridden")
	}
	if rec.Effective.RPM != 5 {
		t.Errorf("effective = %d; want 5", rec.Effective.RPM)
	}
	if rec.Declared.RPM != 60 {
		t.Errorf("declared = %d; want 60 (preserved)", rec.Declared.RPM)
	}
}

func TestMemoryStore_ResetReturnsToDeclared(t *testing.T) {
	s := NewMemoryStore()
	s.Declare("k", Limit{RPM: 60})
	_, _ = s.Configure(context.Background(), "k", Limit{RPM: 5})
	if err := s.Reset(context.Background(), "k"); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot(context.Background())
	if len(snap) != 1 {
		t.Fatalf("snapshot len=%d", len(snap))
	}
	r := snap[0]
	if r.Overridden {
		t.Error("after reset, should not be overridden")
	}
	if r.Effective.RPM != 60 {
		t.Errorf("after reset, effective = %d; want 60", r.Effective.RPM)
	}
}

func TestMemoryStore_UnknownKeyNeverLimits(t *testing.T) {
	s := NewMemoryStore()
	for i := 0; i < 100; i++ {
		ok, _ := s.Allow(context.Background(), "unknown", "")
		if !ok {
			t.Fatalf("call %d on unknown key should always allow", i)
		}
	}
}
