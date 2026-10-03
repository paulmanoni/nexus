package jobsdb_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/db"
	_ "github.com/paulmanoni/nexus/v2/db/sqlite"
	"github.com/paulmanoni/nexus/v2/extension/jobs"
	"github.com/paulmanoni/nexus/v2/extension/jobs/jobsdb"
)

var bg = context.Background()

// openDB opens a connection to the SQLite file at path — each call stands
// for one process.
func openDB(t *testing.T, path string) *gorm.DB {
	t.Helper()
	g, err := gorm.Open(sqlite.Open(path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := g.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	return g
}

func newStore(t *testing.T, path string) jobs.Store {
	g := openDB(t, path)
	return jobsdb.New(func() *gorm.DB { return g })
}

func dbPath(t *testing.T) string { return filepath.Join(t.TempDir(), "jobs.db") }

func TestStoreContract(t *testing.T) {
	s := newStore(t, dbPath(t))
	now := time.Now().Truncate(time.Millisecond)
	rec := jobs.Record{ID: "a1", Name: "Report", Queue: "default", State: jobs.StateQueued,
		Args: []byte(`{"id":1}`), MaxAttempts: 2, CreatedAt: now, RunAt: now}
	if id, ok, err := s.Insert(bg, rec); err != nil || !ok || id != "a1" {
		t.Fatalf("insert: %s %v %v", id, ok, err)
	}
	if _, ok, _ := s.Insert(bg, rec); ok {
		t.Fatal("inserting an existing ID must be a no-op")
	}
	delayed := rec
	delayed.ID, delayed.RunAt = "a2", now.Add(time.Hour)
	_, _, _ = s.Insert(bg, delayed)

	claimed, ok, err := s.Claim(bg, "default", "w1", now, time.Minute)
	if err != nil || !ok || claimed.ID != "a1" || claimed.Attempt != 1 || claimed.Worker != "w1" {
		t.Fatalf("claim: %+v %v %v", claimed, ok, err)
	}
	if _, ok, _ := s.Claim(bg, "default", "w2", now, time.Minute); ok {
		t.Fatal("the delayed job must not be claimable yet")
	}
	if next, ok, _ := s.NextDue(bg, "default"); !ok || !next.Equal(delayed.RunAt.UTC()) {
		t.Fatalf("next due = %v %v", next, ok)
	}
	updated, ok, err := s.Update(bg, "a1", func(r *jobs.Record) bool {
		r.Progress = jobs.Progress{Done: 3, Total: 4, Message: "almost"}
		r.Result = []byte(`{"ok":true}`)
		return true
	})
	if err != nil || !ok || updated.Progress.Done != 3 {
		t.Fatalf("update: %+v %v %v", updated, ok, err)
	}
	got, _, _ := s.Get(bg, "a1")
	if got.Progress.Message != "almost" || string(got.Result) != `{"ok":true}` || string(got.Args) != `{"id":1}` {
		t.Fatalf("round trip: %+v", got)
	}
	_, _, _ = s.Update(bg, "a1", func(r *jobs.Record) bool {
		r.State, r.Error, r.FinishedAt = jobs.StateFailed, "boom", now
		return true
	})
	st, err := s.Stats(bg, now)
	if err != nil || st.Queues["default"].Failed != 1 || st.Queues["default"].Delayed != 1 || st.LastFailure == nil || st.LastFailure.Error != "boom" {
		t.Fatalf("stats: %+v %v", st, err)
	}
	list, _ := s.List(bg, jobs.Filter{State: jobs.StateFailed})
	if len(list) != 1 || list[0].ID != "a1" {
		t.Fatalf("list failed: %+v", list)
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

func TestUniqueAcrossProcesses(t *testing.T) {
	path := dbPath(t)
	a, b := newStore(t, path), newStore(t, path)
	now := time.Now()
	mk := func(id string) jobs.Record {
		return jobs.Record{ID: jobs.ID(id), Name: "R", Queue: "default", State: jobs.StateQueued, MaxAttempts: 1,
			CreatedAt: now, RunAt: now, UniqueKey: "k1", UniqueUntil: now.Add(time.Minute)}
	}
	_, _, _ = a.Insert(bg, mk("warm")) // create the tables before the race
	_, _, _ = a.Update(bg, "warm", func(r *jobs.Record) bool { r.State, r.FinishedAt = jobs.StateSucceeded, now; return true })

	var inserted atomic.Int32
	var wg sync.WaitGroup
	for i, s := range []jobs.Store{a, b, a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok, err := s.Insert(bg, mk(string(rune('p'+i)))); err == nil && ok {
				inserted.Add(1)
			} else if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if inserted.Load() != 1 {
		t.Fatalf("%d inserts won the unique key, want 1", inserted.Load())
	}
}

type Svc struct {
	calls  atomic.Int32
	block  chan struct{}
	byWork sync.Map
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

// bootProcess boots one app on its own connection to the shared file.
func bootProcess(t *testing.T, path string, svc *Svc, cfg jobs.Config) *jobs.Manager {
	t.Helper()
	store := newStore(t, path)
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

// Two processes share one queue: every job runs exactly once, and both take
// a share.
func TestTwoProcessesShareTheQueue(t *testing.T) {
	path := dbPath(t)
	svc := &Svc{}
	cfg := jobs.Config{Queues: map[string]int{"default": 2}, PollInterval: 10 * time.Millisecond}
	a := bootProcess(t, path, svc, cfg)
	b := bootProcess(t, path, svc, cfg)
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
		rec := waitState(t, a, id, jobs.StateSucceeded)
		workers[rec.Worker]++
	}
	if svc.calls.Load() != 30 {
		t.Fatalf("%d executions for 30 jobs", svc.calls.Load())
	}
	t.Logf("jobs per process: %v", workers) // both usually take a share; nothing guarantees it
	if st, _ := b.List(bg, jobs.Filter{State: jobs.StateSucceeded}); len(st) != 30 {
		t.Errorf("process b sees %d succeeded jobs", len(st))
	}
}

// A job whose worker died is taken over once its lease lapses; one whose
// worker died on its last attempt is failed.
func TestLeaseTakeover(t *testing.T) {
	path := dbPath(t)
	dead := newStore(t, path)
	now := time.Now()
	for _, r := range []jobs.Record{
		{ID: "retry", Name: "Svc.Work", Queue: "default", State: jobs.StateQueued, Args: []byte(`{"N":7}`), MaxAttempts: 2, CreatedAt: now, RunAt: now},
		{ID: "last", Name: "Svc.Work", Queue: "default", State: jobs.StateQueued, Args: []byte(`{"N":8}`), MaxAttempts: 1, CreatedAt: now.Add(time.Millisecond), RunAt: now},
	} {
		if _, _, err := dead.Insert(bg, r); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := dead.Claim(bg, "default", "crashed-process", now, 50*time.Millisecond); !ok || err != nil {
			t.Fatalf("claim %s: %v %v", r.ID, ok, err)
		}
	}
	svc := &Svc{}
	m := bootProcess(t, path, svc, jobs.Config{PollInterval: 10 * time.Millisecond, Lease: 300 * time.Millisecond})
	rec := waitState(t, m, "retry", jobs.StateSucceeded)
	if rec.Attempt != 2 || rec.Worker == "crashed-process" || string(rec.Result) != "7" {
		t.Fatalf("taken over: %+v", rec)
	}
	lost := waitState(t, m, "last", jobs.StateFailed)
	if !strings.Contains(lost.Error, "lease") {
		t.Fatalf("lost job error = %q", lost.Error)
	}
}

// Cancelling from one process stops the job running in another.
func TestCancelAcrossProcesses(t *testing.T) {
	path := dbPath(t)
	svc := &Svc{block: make(chan struct{})}
	defer close(svc.block)
	a := bootProcess(t, path, svc, jobs.Config{PollInterval: 10 * time.Millisecond, Lease: 90 * time.Millisecond})
	id, err := work.Enqueue(bg, Args{N: 1}) // work is bound to the last-booted manager: a
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, a, id, jobs.StateRunning)
	other := bootProcess(t, path, &Svc{}, jobs.Config{DisableWorkers: true})
	if found, err := other.Cancel(bg, id); !found || err != nil {
		t.Fatalf("cancel from the other process: %v %v", found, err)
	}
	waitState(t, other, id, jobs.StateCancelled)
}

type TestDB struct{ *db.Manager }

// jobsdb.Bind on a nexus database selects the db driver.
func TestBindSelectsTheDBDriver(t *testing.T) {
	mgr, err := db.Open(db.Config{Driver: db.SQLite, Database: dbPath(t), LogLevel: "silent"})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Stop()
	svc := &Svc{}
	var m *jobs.Manager
	var app *nexus.App
	_, stop, err := nexus.InProcess(nexus.Config{},
		nexus.Supply(&TestDB{Manager: mgr}),
		jobsdb.Bind[TestDB](),
		jobs.Module(jobs.Config{PollInterval: 10 * time.Millisecond}),
		nexus.Supply(svc), work,
		nexus.Invoke(func(mm *jobs.Manager, a *nexus.App) { m, app = mm, a }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(bg) }()
	id, _ := work.Enqueue(bg, Args{N: 5})
	waitState(t, m, id, jobs.StateSucceeded)
	for _, r := range app.Registry().Resources() {
		if r.Name == "jobs" && (r.Details["driver"] != "db" || !strings.Contains(r.Description, "(db)")) {
			t.Errorf("dashboard: %s %v", r.Description, r.Details)
		}
	}
}

func TestUnavailableDatabase(t *testing.T) {
	s := jobsdb.New(func() *gorm.DB { return nil })
	if _, _, err := s.Get(bg, "x"); !errors.Is(err, jobsdb.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}
