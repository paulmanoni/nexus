package jobsredis_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/extension/jobs"
	"github.com/paulmanoni/nexus/extension/jobs/jobsredis/v2"
)

var bg = context.Background()

func server(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	return miniredis.RunT(t)
}

// newStore connects to srv — each call stands for one process.
func newStore(t *testing.T, srv *miniredis.Miniredis, prefix string) jobs.Store {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return jobsredis.New(c, prefix)
}

func TestStoreContract(t *testing.T) {
	s := newStore(t, server(t), "")
	now := time.Now().Truncate(time.Millisecond)
	rec := jobs.Record{ID: "a1", Name: "Report", Queue: "default", State: jobs.StateQueued,
		Args: []byte(`{"ids":[],"n":1}`), MaxAttempts: 2, CreatedAt: now, RunAt: now}
	if id, ok, err := s.Insert(bg, rec); err != nil || !ok || id != "a1" {
		t.Fatalf("insert: %s %v %v", id, ok, err)
	}
	if _, ok, _ := s.Insert(bg, rec); ok {
		t.Fatal("inserting an existing ID must be a no-op")
	}
	delayed := rec
	delayed.ID, delayed.RunAt, delayed.CreatedAt = "a2", now.Add(time.Hour), now.Add(time.Millisecond)
	_, _, _ = s.Insert(bg, delayed)

	claimed, ok, err := s.Claim(bg, "default", "w1", now, time.Minute)
	if err != nil || !ok || claimed.ID != "a1" || claimed.Attempt != 1 || claimed.Worker != "w1" || claimed.State != jobs.StateRunning {
		t.Fatalf("claim: %+v %v %v", claimed, ok, err)
	}
	if _, ok, _ := s.Claim(bg, "default", "w2", now, time.Minute); ok {
		t.Fatal("the delayed job must not be claimable yet")
	}
	if next, ok, _ := s.NextDue(bg, "default"); !ok || next.UnixMilli() != delayed.RunAt.UnixMilli() {
		t.Fatalf("next due = %v %v", next, ok)
	}
	_, ok, err = s.Update(bg, "a1", func(r *jobs.Record) bool {
		r.Progress = jobs.Progress{Done: 3, Total: 4, Message: "almost"}
		r.Result = []byte(`{"ok":true}`)
		return true
	})
	if err != nil || !ok {
		t.Fatalf("update: %v %v", ok, err)
	}
	got, _, _ := s.Get(bg, "a1")
	if got.Progress.Message != "almost" || string(got.Result) != `{"ok":true}` || string(got.Args) != `{"ids":[],"n":1}` || got.Worker != "w1" {
		t.Fatalf("round trip: %+v", got)
	}
	_, _, _ = s.Update(bg, "a1", func(r *jobs.Record) bool {
		r.State, r.Error, r.FinishedAt = jobs.StateFailed, "boom", now
		return true
	})
	st, err := s.Stats(bg, now)
	q := st.Queues["default"]
	if err != nil || q.Failed != 1 || q.Delayed != 1 || q.Running != 0 || st.LastFailure == nil || st.LastFailure.Error != "boom" {
		t.Fatalf("stats: %+v %v", st, err)
	}
	if list, _ := s.List(bg, jobs.Filter{State: jobs.StateFailed}); len(list) != 1 || list[0].ID != "a1" {
		t.Fatalf("list failed: %+v", list)
	}
	if list, _ := s.List(bg, jobs.Filter{}); len(list) != 2 || list[0].ID != "a2" {
		t.Fatalf("list is newest first: %+v", list)
	}
	if err := s.Prune(bg, now.Add(time.Second), 0); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(bg, "a1"); ok {
		t.Fatal("prune kept a finished job past the cutoff")
	}
	if _, ok, _ := s.Get(bg, "a2"); !ok {
		t.Fatal("prune removed a pending job")
	}
}

func TestPrefixesSeparateQueues(t *testing.T) {
	srv := server(t)
	a, b := newStore(t, srv, "app-a:"), newStore(t, srv, "app-b:")
	now := time.Now()
	_, _, _ = a.Insert(bg, jobs.Record{ID: "x", Name: "R", Queue: "default", State: jobs.StateQueued, MaxAttempts: 1, CreatedAt: now, RunAt: now})
	if _, ok, _ := b.Claim(bg, "default", "w", now, time.Minute); ok {
		t.Fatal("a store claimed another prefix's job")
	}
}

func TestUniqueAcrossProcesses(t *testing.T) {
	srv := server(t)
	a, b := newStore(t, srv, ""), newStore(t, srv, "")
	now := time.Now()
	var inserted atomic.Int32
	var wg sync.WaitGroup
	for i, s := range []jobs.Store{a, b, a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := jobs.Record{ID: jobs.ID(string(rune('p' + i))), Name: "R", Queue: "default", State: jobs.StateQueued,
				MaxAttempts: 1, CreatedAt: now, RunAt: now, UniqueKey: "k1", UniqueUntil: now.Add(time.Minute)}
			if _, ok, err := s.Insert(bg, rec); err != nil {
				t.Error(err)
			} else if ok {
				inserted.Add(1)
			}
		}()
	}
	wg.Wait()
	if inserted.Load() != 1 {
		t.Fatalf("%d inserts won the unique key, want 1", inserted.Load())
	}
}

type Svc struct {
	calls atomic.Int32
	block chan struct{}
}

type Args struct{ N int }

func (s *Svc) Work(ctx context.Context, run *jobs.Run, a Args) error {
	s.calls.Add(1)
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	time.Sleep(5 * time.Millisecond)
	return run.SetResult(a.N)
}

var work = jobs.Define((*Svc).Work, jobs.Retry(1), jobs.Backoff(func(int) time.Duration { return 10 * time.Millisecond }))

func bootProcess(t *testing.T, srv *miniredis.Miniredis, svc *Svc, cfg jobs.Config) *jobs.Manager {
	t.Helper()
	store := newStore(t, srv, "")
	var m *jobs.Manager
	_, stop, err := nexus.InProcess(nexus.Config{},
		nexus.Provide(func() jobs.Store { return store }),
		jobs.Module(cfg),
		nexus.Supply(svc), work,
		nexus.Invoke(func(mgr *jobs.Manager) { m = mgr }),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(bg) })
	return m
}

func waitState(t *testing.T, m *jobs.Manager, id jobs.ID, want jobs.State) jobs.Record {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if rec, ok, _ := m.Get(bg, id); ok && rec.State == want {
			return rec
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec, _, _ := m.Get(bg, id)
	t.Fatalf("job %s: %s (%q), want %s", id, rec.State, rec.Error, want)
	return rec
}

func TestTwoProcessesShareTheQueue(t *testing.T) {
	srv := server(t)
	svc := &Svc{}
	cfg := jobs.Config{Queues: map[string]int{"default": 2}, PollInterval: 10 * time.Millisecond}
	a := bootProcess(t, srv, svc, cfg)
	b := bootProcess(t, srv, svc, cfg)
	var ids []jobs.ID
	for i := range 30 {
		id, err := work.Enqueue(bg, Args{N: i})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	workers := map[string]int{}
	for _, id := range ids {
		workers[waitState(t, a, id, jobs.StateSucceeded).Worker]++
	}
	if svc.calls.Load() != 30 {
		t.Fatalf("%d executions for 30 jobs", svc.calls.Load())
	}
	t.Logf("jobs per process: %v", workers) // both usually take a share; nothing guarantees it
	if st, _ := b.List(bg, jobs.Filter{State: jobs.StateSucceeded}); len(st) != 30 {
		t.Errorf("process b sees %d succeeded jobs", len(st))
	}
}

func TestLeaseTakeover(t *testing.T) {
	srv := server(t)
	dead := newStore(t, srv, "")
	now := time.Now()
	for _, r := range []jobs.Record{
		{ID: "retry", Name: "Svc.Work", Queue: "default", State: jobs.StateQueued, Args: []byte(`{"N":7}`), MaxAttempts: 2, CreatedAt: now, RunAt: now},
		{ID: "last", Name: "Svc.Work", Queue: "default", State: jobs.StateQueued, Args: []byte(`{"N":8}`), MaxAttempts: 1, CreatedAt: now, RunAt: now.Add(time.Millisecond)},
	} {
		if _, _, err := dead.Insert(bg, r); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := dead.Claim(bg, "default", "crashed-process", now.Add(5*time.Millisecond), 50*time.Millisecond); !ok || err != nil {
			t.Fatalf("claim %s: %v %v", r.ID, ok, err)
		}
	}
	m := bootProcess(t, srv, &Svc{}, jobs.Config{PollInterval: 10 * time.Millisecond, Lease: 300 * time.Millisecond})
	if rec := waitState(t, m, "retry", jobs.StateSucceeded); rec.Attempt != 2 || rec.Worker == "crashed-process" || string(rec.Result) != "7" {
		t.Fatalf("taken over: %+v", rec)
	}
	if lost := waitState(t, m, "last", jobs.StateFailed); !strings.Contains(lost.Error, "lease") {
		t.Fatalf("lost job error = %q", lost.Error)
	}
}

func TestCancelAcrossProcesses(t *testing.T) {
	srv := server(t)
	svc := &Svc{block: make(chan struct{})}
	defer close(svc.block)
	a := bootProcess(t, srv, svc, jobs.Config{PollInterval: 10 * time.Millisecond, Lease: 90 * time.Millisecond})
	id, err := work.Enqueue(bg, Args{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, a, id, jobs.StateRunning)
	other := bootProcess(t, srv, &Svc{}, jobs.Config{DisableWorkers: true})
	if found, err := other.Cancel(bg, id); !found || err != nil {
		t.Fatalf("cancel from the other process: %v %v", found, err)
	}
	waitState(t, other, id, jobs.StateCancelled)
}

// Bind connects from a URL and selects the redis driver.
func TestBindSelectsTheRedisDriver(t *testing.T) {
	srv := server(t)
	var m *jobs.Manager
	var app *nexus.App
	_, stop, err := nexus.InProcess(nexus.Config{},
		jobsredis.Bind(jobsredis.Config{URL: "redis://" + srv.Addr()}),
		jobs.Module(jobs.Config{PollInterval: 10 * time.Millisecond}),
		nexus.Supply(&Svc{}), work,
		nexus.Invoke(func(mm *jobs.Manager, a *nexus.App) { m, app = mm, a }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(bg) }()
	id, _ := work.Enqueue(bg, Args{N: 5})
	waitState(t, m, id, jobs.StateSucceeded)
	for _, r := range app.Registry().Resources() {
		if r.Name == "jobs" && r.Details["driver"] != "redis" {
			t.Errorf("dashboard: %v", r.Details)
		}
	}
	if keys := srv.Keys(); len(keys) == 0 || !strings.HasPrefix(keys[0], "{nexus:jobs}:") {
		t.Errorf("keys = %v, want the {nexus:jobs}: prefix", keys)
	}
}

func TestBindRejectsABadURL(t *testing.T) {
	_, stop, err := nexus.InProcess(nexus.Config{},
		jobsredis.Bind(jobsredis.Config{URL: "not a url"}),
		jobs.Module(jobs.Config{}), work, nexus.Supply(&Svc{}),
	)
	if stop != nil {
		defer func() { _ = stop(bg) }()
	}
	if err == nil || !strings.Contains(err.Error(), "jobsredis: url") {
		t.Fatalf("err = %v", err)
	}
}
