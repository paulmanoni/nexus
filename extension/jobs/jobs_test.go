package jobs_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/extension/jobs"
)

type ReportArgs struct {
	ReportID int `json:"reportId"`
}

type ReportService struct {
	calls    atomic.Int32
	failures int32 // attempts that fail before one succeeds
	block    chan struct{}
}

func (s *ReportService) Generate(ctx context.Context, run *jobs.Run, a ReportArgs) error {
	n := s.calls.Add(1)
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if n <= s.failures {
		return errors.New("flaky")
	}
	if err := run.Progress(1, 2, "half"); err != nil {
		return err
	}
	return run.SetResult(map[string]int{"report": a.ReportID, "attempt": run.Attempt()})
}

func (s *ReportService) Summarize(ctx context.Context, run *jobs.Run, a ReportArgs) error { return nil }

// Twice is defined twice, so enqueuing it by method expression is ambiguous.
func (s *ReportService) Twice(ctx context.Context, run *jobs.Run, a ReportArgs) error { return nil }

var (
	twiceA = jobs.Define((*ReportService).Twice)
	twiceB = jobs.Define((*ReportService).Twice, jobs.Name("twice-b"))
)

func (s *ReportService) Explode(ctx context.Context, run *jobs.Run, a ReportArgs) error {
	panic("boom")
}

func (s *ReportService) Reject(ctx context.Context, run *jobs.Run, a ReportArgs) error {
	s.calls.Add(1)
	return jobs.Permanent(errors.New("no such report"))
}

func (s *ReportService) Slow(ctx context.Context, run *jobs.Run, a ReportArgs) error {
	<-ctx.Done()
	return ctx.Err()
}

// Resumable checkpoints before failing once; the retry resumes from it.
func (s *ReportService) Resumable(ctx context.Context, run *jobs.Run, a ReportArgs) error {
	var done int
	if ok, err := run.Resume(&done); err != nil {
		return err
	} else if !ok {
		done = 0
	}
	for i := done; i < 3; i++ {
		if err := run.Checkpoint(i + 1); err != nil {
			return err
		}
		if i == 1 && run.Attempt() == 1 {
			return errors.New("interrupted midway")
		}
	}
	return run.SetResult(map[string]int{"resumedFrom": done})
}

func boot(t *testing.T, svc *ReportService, opts ...nexus.Option) (*jobs.Manager, func()) {
	t.Helper()
	var m *jobs.Manager
	all := append([]nexus.Option{
		jobs.Module(jobs.Config{Queues: map[string]int{"default": 2, "low": 1}}),
		nexus.Supply(svc),
		nexus.Invoke(func(mgr *jobs.Manager) { m = mgr }),
	}, opts...)
	_, stop, err := nexus.InProcess(config.Runtime{}, all...)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	return m, func() { _ = stop(context.Background()) }
}

func waitState(t *testing.T, m *jobs.Manager, id jobs.ID, want jobs.State) jobs.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rec, ok, _ := m.Get(context.Background(), id); ok && rec.State == want {
			return rec
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec, _, _ := m.Get(context.Background(), id)
	t.Fatalf("job %s: state %s (error %q), want %s", id, rec.State, rec.Error, want)
	return rec
}

var (
	generate  = jobs.Define((*ReportService).Generate, jobs.Retry(2), jobs.Backoff(func(int) time.Duration { return time.Millisecond }))
	explode   = jobs.Define((*ReportService).Explode, jobs.Retry(3))
	reject    = jobs.Define((*ReportService).Reject, jobs.Retry(3))
	slow      = jobs.Define((*ReportService).Slow, jobs.Queue("low"), jobs.Timeout(30*time.Millisecond))
	resumable = jobs.Define((*ReportService).Resumable, jobs.Retry(1), jobs.Backoff(func(int) time.Duration { return time.Millisecond }))
)

func TestJobRunsWithResultAndProgress(t *testing.T) {
	svc := &ReportService{}
	m, stop := boot(t, svc, generate)
	defer stop()
	id, err := generate.Enqueue(context.Background(), ReportArgs{ReportID: 7})
	if err != nil {
		t.Fatal(err)
	}
	rec := waitState(t, m, id, jobs.StateSucceeded)
	if string(rec.Result) != `{"attempt":1,"report":7}` || rec.Progress.Message != "half" || generate.Name() != "ReportService.Generate" {
		t.Fatalf("record = %+v (name %s)", rec, generate.Name())
	}
}

func TestRetryThenSucceed(t *testing.T) {
	svc := &ReportService{failures: 2}
	m, stop := boot(t, svc, generate)
	defer stop()
	id, _ := generate.Enqueue(context.Background(), ReportArgs{ReportID: 1})
	rec := waitState(t, m, id, jobs.StateSucceeded)
	if rec.Attempt != 3 || svc.calls.Load() != 3 || rec.Error != "" {
		t.Fatalf("attempt %d, calls %d, error %q", rec.Attempt, svc.calls.Load(), rec.Error)
	}
}

func TestPanicsAndPermanentErrorsDontRetry(t *testing.T) {
	svc := &ReportService{}
	m, stop := boot(t, svc, explode, reject)
	defer stop()
	id1, _ := explode.Enqueue(context.Background(), ReportArgs{})
	id2, _ := reject.Enqueue(context.Background(), ReportArgs{})
	if rec := waitState(t, m, id1, jobs.StateFailed); !strings.Contains(rec.Error, "panic: boom") || rec.Attempt != 1 {
		t.Errorf("panic: %+v", rec)
	}
	if rec := waitState(t, m, id2, jobs.StateFailed); rec.Error != "no such report" || svc.calls.Load() != 1 {
		t.Errorf("permanent: %+v, calls %d", rec, svc.calls.Load())
	}
}

func TestTimeoutFails(t *testing.T) {
	m, stop := boot(t, &ReportService{}, slow)
	defer stop()
	id, _ := slow.Enqueue(context.Background(), ReportArgs{})
	if rec := waitState(t, m, id, jobs.StateFailed); !strings.Contains(rec.Error, "deadline exceeded") {
		t.Fatalf("timeout error = %q", rec.Error)
	}
}

func TestCancelQueuedAndRunning(t *testing.T) {
	svc := &ReportService{block: make(chan struct{})}
	m, stop := boot(t, svc, generate)
	defer stop()
	running, _ := generate.Enqueue(context.Background(), ReportArgs{ReportID: 1})
	waitState(t, m, running, jobs.StateRunning)
	delayed, _ := generate.Enqueue(context.Background(), ReportArgs{ReportID: 2}, jobs.Delay(time.Hour))
	_, _ = m.Cancel(context.Background(), delayed)
	_, _ = m.Cancel(context.Background(), running)
	waitState(t, m, running, jobs.StateCancelled)
	if rec, _, _ := m.Get(context.Background(), delayed); rec.State != jobs.StateCancelled {
		t.Fatalf("queued job state %s after Cancel", rec.State)
	}
	if found, _ := m.Cancel(context.Background(), "nope"); found {
		t.Fatal("Cancel of an unknown job reported true")
	}
}

func TestUniqueAndDelay(t *testing.T) {
	unique := jobs.Define((*ReportService).Summarize, jobs.Unique(time.Minute))
	svc := &ReportService{}
	m, stop := boot(t, svc, unique)
	defer stop()
	a, _ := unique.Enqueue(context.Background(), ReportArgs{ReportID: 5}, jobs.Delay(500*time.Millisecond))
	b, _ := unique.Enqueue(context.Background(), ReportArgs{ReportID: 5})
	c, _ := unique.Enqueue(context.Background(), ReportArgs{ReportID: 6}, jobs.Delay(500*time.Millisecond))
	if a != b || a == c {
		t.Fatalf("unique ids: %s %s %s", a, b, c)
	}
	if rec, _, _ := m.Get(context.Background(), a); rec.State != jobs.StateQueued {
		t.Fatalf("delayed job ran early: %s", rec.State)
	}
	waitState(t, m, a, jobs.StateSucceeded)
	waitState(t, m, c, jobs.StateSucceeded)
}

func TestShutdownRequeuesRunningJobs(t *testing.T) {
	svc := &ReportService{block: make(chan struct{})}
	var m *jobs.Manager
	_, stop, err := nexus.InProcess(config.Runtime{},
		jobs.Module(jobs.Config{ShutdownGrace: time.Millisecond}),
		nexus.Supply(svc), generate,
		nexus.Invoke(func(mgr *jobs.Manager) { m = mgr }),
	)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := generate.Enqueue(context.Background(), ReportArgs{})
	waitState(t, m, id, jobs.StateRunning)
	_ = stop(context.Background())
	if rec, _, _ := m.Get(context.Background(), id); rec.State != jobs.StateQueued || rec.Attempt != 0 {
		t.Fatalf("after shutdown: state %s attempt %d, want queued/0", rec.State, rec.Attempt)
	}
}

func TestCheckpointResume(t *testing.T) {
	m, stop := boot(t, &ReportService{}, resumable)
	defer stop()
	id, _ := resumable.Enqueue(context.Background(), ReportArgs{})
	if rec := waitState(t, m, id, jobs.StateSucceeded); string(rec.Result) != `{"resumedFrom":2}` {
		t.Fatalf("result = %s", rec.Result)
	}
}

func TestEnqueueByMethodExpressionAndErrors(t *testing.T) {
	svc := &ReportService{}
	m, stop := boot(t, svc, generate)
	defer stop()
	id, err := jobs.Enqueue(context.Background(), (*ReportService).Generate, ReportArgs{ReportID: 3})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, id, jobs.StateSucceeded)
	if _, err := jobs.Enqueue(context.Background(), (*ReportService).Twice, ReportArgs{}); err == nil || !strings.Contains(err.Error(), "defined 2 times") {
		t.Fatalf("ambiguous enqueue = %v", err)
	}
	_, _ = twiceA, twiceB
	orphan := jobs.DefineFunc(func(ctx context.Context, run *jobs.Run, a ReportArgs) error { return nil }, jobs.Name("orphan"))
	if _, err := orphan.Enqueue(context.Background(), ReportArgs{}); !errors.Is(err, jobs.ErrNotInstalled) {
		t.Fatalf("enqueue before registration = %v, want ErrNotInstalled", err)
	}
	type other struct{}
	if _, err := jobs.Enqueue(context.Background(), func(o *other, ctx context.Context, run *jobs.Run, a ReportArgs) error { return nil }, ReportArgs{}); err == nil {
		t.Fatal("enqueue of an undefined function should fail")
	}
}

func TestBootErrors(t *testing.T) {
	closure := jobs.DefineFunc(func(ctx context.Context, run *jobs.Run, a ReportArgs) error { return nil })
	dupA := jobs.DefineFunc(func(ctx context.Context, run *jobs.Run, a ReportArgs) error { return nil }, jobs.Name("dup"))
	dupB := jobs.DefineFunc(func(ctx context.Context, run *jobs.Run, a ReportArgs) error { return nil }, jobs.Name("dup"))
	cases := []struct {
		name string
		opts []nexus.Option
		want string
	}{
		{"closure without a name", []nexus.Option{jobs.Module(jobs.Config{}), closure}, "no stable name"},
		{"duplicate names", []nexus.Option{jobs.Module(jobs.Config{}), dupA, dupB}, `two jobs are named "dup"`},
		{"driver without a store", []nexus.Option{jobs.Module(jobs.Config{Driver: "db"}), dupA}, `driver "db" needs its store bound`},
		{"no module", []nexus.Option{dupA}, "Manager"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stop, err := nexus.InProcess(config.Runtime{}, tc.opts...)
			if stop != nil {
				defer func() { _ = stop(context.Background()) }()
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a boot error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestDashboardDetails(t *testing.T) {
	svc := &ReportService{}
	var app *nexus.App
	m, stop := boot(t, svc, generate, reject, nexus.Invoke(func(a *nexus.App) { app = a }))
	defer stop()
	id, _ := reject.Enqueue(context.Background(), ReportArgs{})
	waitState(t, m, id, jobs.StateFailed)
	var details map[string]any
	for _, r := range app.Registry().Resources() {
		if r.Name == "jobs" {
			details = r.Details
		}
	}
	if details == nil {
		t.Fatal("no jobs resource on the dashboard")
	}
	if q, _ := details["queue default"].(string); !strings.Contains(q, "1 failed") {
		t.Errorf("queue default = %v", details["queue default"])
	}
	if f, _ := details["last failure"].(string); !strings.Contains(f, "ReportService.Reject") || !strings.Contains(f, "no such report") {
		t.Errorf("last failure = %v", details["last failure"])
	}
	if n, _ := details["jobs"].(string); !strings.Contains(n, "ReportService.Generate") {
		t.Errorf("jobs = %v", details["jobs"])
	}
}

func (s *ReportService) Tick(ctx context.Context, run *jobs.Run, a ReportArgs) error {
	s.calls.Add(1)
	return nil
}

var tick = jobs.Define((*ReportService).Tick)

// A schedule enqueues each tick once, even when two schedules for the same
// job and spec (standing in for two replicas) fire.
func TestSchedule(t *testing.T) {
	svc := &ReportService{}
	started := time.Now()
	m, stop := boot(t, svc, tick.Schedule("@every 1s", ReportArgs{ReportID: 9}), tick.Schedule("@every 1s", ReportArgs{ReportID: 9}))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && svc.calls.Load() < 1 {
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	ticks := int(time.Since(started)/time.Second) + 1 // @every aligns to whole seconds
	recs, err := m.List(context.Background(), jobs.Filter{Name: "ReportService.Tick"})
	// Two schedulers fire each tick; without the per-tick ID each would enqueue.
	if err != nil || len(recs) < 1 || len(recs) > ticks || int(svc.calls.Load()) != len(recs) {
		t.Fatalf("%d records, %d calls for at most %d ticks (%v)", len(recs), svc.calls.Load(), ticks, err)
	}
	if string(recs[0].Args) != `{"reportId":9}` || !strings.HasPrefix(string(recs[0].ID), "s") {
		t.Fatalf("scheduled record = %+v", recs[0])
	}
}

func TestBadSchedule(t *testing.T) {
	_, stop, err := nexus.InProcess(config.Runtime{}, jobs.Module(jobs.Config{}), nexus.Supply(&ReportService{}),
		tick.Schedule("every tuesday-ish", ReportArgs{}))
	if stop != nil {
		defer func() { _ = stop(context.Background()) }()
	}
	if err == nil || !strings.Contains(err.Error(), "every tuesday-ish") {
		t.Fatalf("want a boot error naming the schedule, got %v", err)
	}
}
