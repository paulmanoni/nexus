package cron

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegisterRejectsBadInput(t *testing.T) {
	s := NewScheduler(nil, 0)
	cases := []struct {
		name string
		job  Job
	}{
		{"no name", Job{Schedule: "@every 1s", Handler: noop}},
		{"no schedule", Job{Name: "a", Handler: noop}},
		{"no handler", Job{Name: "a", Schedule: "@every 1s"}},
		{"bad schedule", Job{Name: "a", Schedule: "not-a-cron", Handler: noop}},
	}
	for _, c := range cases {
		if err := s.Register(c.job); err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
	}
}

func TestTriggerRecordsRunAndHistory(t *testing.T) {
	s := NewScheduler(nil, 0)
	var calls atomic.Int32
	if err := s.Register(Job{
		Name:     "noop",
		Schedule: "@every 24h", // far future — only manual triggers fire
		Handler: func(ctx context.Context) error {
			calls.Add(1)
			return nil
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	s.Start()
	defer s.Stop()

	if !s.Trigger("noop") {
		t.Fatal("Trigger returned false for known job")
	}
	waitFor(t, func() bool { return calls.Load() == 1 })

	snap, ok := s.Snapshot("noop")
	if !ok {
		t.Fatal("Snapshot missing")
	}
	if snap.LastRun == nil {
		t.Fatal("LastRun should be set")
	}
	if !snap.LastRun.Success || !snap.LastRun.Manual {
		t.Errorf("LastRun = %+v; want success && manual", snap.LastRun)
	}
	if len(snap.History) != 1 {
		t.Errorf("history len = %d; want 1", len(snap.History))
	}
}

func TestTriggerRecordsErrors(t *testing.T) {
	s := NewScheduler(nil, 0)
	_ = s.Register(Job{
		Name:     "bad",
		Schedule: "@every 24h",
		Handler:  func(ctx context.Context) error { return errors.New("boom") },
	})
	s.Start()
	defer s.Stop()

	s.Trigger("bad")
	waitFor(t, func() bool {
		snap, _ := s.Snapshot("bad")
		return snap.LastRun != nil
	})

	snap, _ := s.Snapshot("bad")
	if snap.LastRun.Success {
		t.Error("expected failure")
	}
	if snap.LastRun.Error != "boom" {
		t.Errorf("error = %q; want boom", snap.LastRun.Error)
	}
}

func TestPauseSkipsScheduledTicks(t *testing.T) {
	s := NewScheduler(nil, 0)
	var calls atomic.Int32
	_ = s.Register(Job{
		Name:     "p",
		Schedule: "@every 100ms",
		Handler:  func(ctx context.Context) error { calls.Add(1); return nil },
	})
	s.SetPaused("p", true)
	s.Start()
	defer s.Stop()

	time.Sleep(350 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Errorf("paused job ran %d times; want 0", got)
	}

	// Manual trigger bypasses the pause.
	s.Trigger("p")
	waitFor(t, func() bool { return calls.Load() == 1 })
}

func TestUnknownJobIsNoOp(t *testing.T) {
	s := NewScheduler(nil, 0)
	if s.Trigger("nope") || s.SetPaused("nope", true) {
		t.Error("unknown job should return false")
	}
	if _, ok := s.Snapshot("nope"); ok {
		t.Error("Snapshot of unknown job should return ok=false")
	}
}

func TestHistoryCapEnforced(t *testing.T) {
	s := NewScheduler(nil, 3)
	_ = s.Register(Job{
		Name:     "cap",
		Schedule: "@every 24h",
		Handler:  noop,
	})
	s.Start()
	defer s.Stop()
	for i := range 5 {
		s.Trigger("cap")
		waitFor(t, func() bool {
			snap, _ := s.Snapshot("cap")
			return snap.LastRun != nil && len(snap.History) >= min(i+1, 3)
		})
	}
	snap, _ := s.Snapshot("cap")
	if len(snap.History) != 3 {
		t.Errorf("history len = %d; want 3", len(snap.History))
	}
}

func noop(ctx context.Context) error { return nil }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("waitFor: condition not met within 2s")
}