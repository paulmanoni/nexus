package jobs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/resource"
)

// Config configures the jobs runtime. Zero values read the [jobs] block of
// nexus.toml, then fall back to the defaults noted per field:
//
//	[jobs]
//	driver = "memory"         # the one driver in this release
//	run    = true             # false: this process enqueues but runs nothing
//	shutdown_grace = "10s"    # how long running jobs get to finish on shutdown
//
//	[jobs.queues]             # queue → concurrent workers
//	default = 4
//	low     = 1
type Config struct {
	// Driver stores the jobs. "memory" (the default, and the one driver in
	// this release) keeps them in the process; they survive `nexus dev`
	// rebuilds but not restarts.
	Driver string

	// Queues maps each queue to its number of concurrent workers. Default
	// {"default": 4}; a job naming a queue missing here gets 1 worker.
	Queues map[string]int

	// DisableWorkers makes this process enqueue without running jobs — a web
	// replica beside dedicated worker replicas. (With the memory driver
	// nothing else would run them, so boot warns.) nexus.toml: run = false.
	DisableWorkers bool

	// ShutdownGrace is how long running jobs may keep going once shutdown
	// begins; after it their contexts are cancelled and they are requeued.
	// Default 10s, or 0 under `nexus dev` (a rebuild shouldn't wait).
	ShutdownGrace time.Duration

	// Retention keeps finished jobs this long for lookups and the dashboard
	// (default 24h); KeepFinished caps how many are kept (default 1000).
	Retention    time.Duration
	KeepFinished int

	// Logger receives job failures and lifecycle events. Default
	// slog.Default().
	Logger *slog.Logger
}

func (c Config) resolve() (Config, error) {
	if c.Driver == "" {
		c.Driver = nexus.Get("jobs.driver", "memory")
	}
	if c.Driver != "memory" {
		return c, fmt.Errorf("jobs: unknown driver %q — this release ships \"memory\"", c.Driver)
	}
	if c.Queues == nil {
		c.Queues = map[string]int{}
		for q, v := range nexus.Get[map[string]any]("jobs.queues") {
			n, err := toInt(v)
			if err != nil || n < 1 {
				return c, fmt.Errorf("jobs: [jobs.queues] %s = %v — want a worker count of 1 or more", q, v)
			}
			c.Queues[q] = n
		}
		if len(c.Queues) == 0 {
			c.Queues[DefaultQueue] = 4
		}
	}
	if !c.DisableWorkers {
		c.DisableWorkers = !nexus.Get("jobs.run", true)
	}
	if c.ShutdownGrace == 0 {
		def := 10 * time.Second
		if nexus.IsDev() {
			def = 0
		}
		c.ShutdownGrace = nexus.Get("jobs.shutdown_grace", def)
	}
	if c.Retention == 0 {
		c.Retention = 24 * time.Hour
	}
	if c.KeepFinished == 0 {
		c.KeepFinished = 1000
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c, nil
}

func toInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	}
	return 0, fmt.Errorf("not a number")
}

// Module installs the jobs runtime: a *Manager in DI (inject it to look jobs
// up or cancel them), worker pools that start with the app and drain on
// shutdown, and a "jobs" resource on the dashboard.
func Module(cfg Config) nexus.Option {
	return nexus.Options(
		nexus.Provide(func(lc nexus.Lifecycle) (*Manager, error) {
			resolved, err := cfg.resolve()
			if err != nil {
				return nil, err
			}
			m := newManager(resolved)
			lc.Append(nexus.Hook{OnStart: m.start, OnStop: m.stop})
			return m, nil
		}),
		nexus.Invoke(func(app *nexus.App, m *Manager) { app.Register(m.asResource()) }),
	)
}

// Manager runs and tracks jobs. Inject *jobs.Manager to look a job up or
// cancel it.
type Manager struct {
	cfg   Config
	store *memoryStore
	log   *slog.Logger

	mu      sync.Mutex
	defs    map[string]*definition
	running map[ID]*execution
	wake    map[string]chan struct{}

	runCtx     context.Context // cancelled after the shutdown grace
	cancelRuns context.CancelFunc
	stopping   chan struct{}
	stopOnce   sync.Once
	workers    sync.WaitGroup
}

type execution struct {
	cancel    context.CancelFunc
	cancelled bool // by Manager.Cancel, not by shutdown
}

func newManager(cfg Config) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:        cfg,
		store:      newMemoryStore(),
		log:        cfg.Logger,
		defs:       map[string]*definition{},
		running:    map[ID]*execution{},
		wake:       map[string]chan struct{}{},
		runCtx:     ctx,
		cancelRuns: cancel,
		stopping:   make(chan struct{}),
	}
	nexus.PreserveDev("jobs", m.store)
	return m
}

// register attaches a definition to this manager; the job's queue gets a
// wake channel (and a worker pool, when it starts).
func (m *Manager) register(d *definition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, dup := m.defs[d.name]; dup && prev != d {
		return fmt.Errorf("jobs: two jobs are named %q — give one jobs.Name(...)", d.name)
	}
	m.defs[d.name] = d
	if _, ok := m.wake[d.queue]; !ok {
		m.wake[d.queue] = make(chan struct{}, 1)
	}
	d.mu.Lock()
	d.mgr = m
	d.mu.Unlock()
	return nil
}

func (m *Manager) start(context.Context) error {
	if m.cfg.DisableWorkers {
		m.log.Warn("jobs: workers disabled (run = false) — with the memory driver, jobs enqueued here never run")
		return nil
	}
	m.mu.Lock()
	queues := make(map[string]int, len(m.wake))
	for q := range m.wake {
		n := m.cfg.Queues[q]
		if n < 1 {
			n = 1
		}
		queues[q] = n
	}
	m.mu.Unlock()
	for q, n := range queues {
		for range n {
			m.workers.Add(1)
			go m.work(q)
		}
	}
	return nil
}

// stop stops claiming, gives running jobs the grace period, then cancels
// them (they are requeued) and waits for them to return.
func (m *Manager) stop(ctx context.Context) error {
	m.stopOnce.Do(func() { close(m.stopping) })
	// Workers return once their current job does; idle ones return now.
	done := make(chan struct{})
	go func() { m.workers.Wait(); close(done) }()
	grace := time.NewTimer(m.cfg.ShutdownGrace)
	defer grace.Stop()
	select {
	case <-done:
		return nil
	case <-grace.C:
	case <-ctx.Done():
	}
	m.cancelRuns()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errors.New("jobs: running jobs ignored cancellation past the shutdown deadline")
	}
}

// work is one worker of queue: claim a due job, run it, repeat; sleep until
// woken by an enqueue, the next delayed job is due, or shutdown.
func (m *Manager) work(queue string) {
	defer m.workers.Done()
	m.mu.Lock()
	wake := m.wake[queue]
	m.mu.Unlock()
	for {
		select {
		case <-m.stopping:
			return
		default:
		}
		if rec, ok := m.store.claim(queue, time.Now()); ok {
			m.execute(rec)
			continue
		}
		wait := time.Minute
		if next, ok := m.store.nextDue(queue); ok {
			wait = max(time.Until(next), time.Millisecond)
		}
		timer := time.NewTimer(wait)
		select {
		case <-m.stopping:
			timer.Stop()
			return
		case <-wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

func (m *Manager) execute(rec Record) {
	m.mu.Lock()
	d := m.defs[rec.Name]
	m.mu.Unlock()
	if d == nil || d.call == nil {
		m.finish(rec, StateFailed, fmt.Sprintf("no job named %q is registered in this process", rec.Name))
		return
	}

	var ctx context.Context
	var cancel context.CancelFunc
	if d.timeout > 0 {
		ctx, cancel = context.WithTimeout(m.runCtx, d.timeout)
	} else {
		ctx, cancel = context.WithCancel(m.runCtx)
	}
	exec := &execution{cancel: cancel}
	m.mu.Lock()
	m.running[rec.ID] = exec
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		delete(m.running, rec.ID)
		m.mu.Unlock()
	}()

	run := &Run{m: m, id: rec.ID, attempt: rec.Attempt, actor: rec.Actor, ctx: ctx}
	err := m.call(d, ctx, run, rec.Args)

	m.mu.Lock()
	byCancel := exec.cancelled
	m.mu.Unlock()
	switch {
	case err == nil:
		m.finish(rec, StateSucceeded, "")
	case byCancel:
		m.finish(rec, StateCancelled, "cancelled")
	case m.runCtx.Err() != nil:
		// Interrupted by shutdown: back to the queue, the attempt uncounted.
		m.store.update(rec.ID, func(r *Record) {
			r.State, r.Attempt, r.StartedAt, r.RunAt = StateQueued, r.Attempt-1, time.Time{}, time.Now()
		})
	default:
		m.fail(d, rec, err)
	}
}

// call runs the job, turning a panic into an error.
func (m *Manager) call(d *definition, ctx context.Context, run *Run, args json.RawMessage) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = Permanent(fmt.Errorf("panic: %v\n%s", p, debug.Stack()))
		}
	}()
	return d.call(ctx, run, args)
}

func (m *Manager) fail(d *definition, rec Record, err error) {
	var perm permanentError
	if rec.Attempt < d.maxAttempts && !errors.As(err, &perm) {
		wait := d.backoff(rec.Attempt)
		m.store.update(rec.ID, func(r *Record) {
			r.State, r.Error, r.RunAt = StateQueued, err.Error(), time.Now().Add(wait)
		})
		m.log.Warn("jobs: attempt failed, retrying", "job", rec.Name, "id", rec.ID,
			"attempt", rec.Attempt, "retryIn", wait, "error", err)
		return
	}
	m.finish(rec, StateFailed, err.Error())
	m.log.Error("jobs: job failed", "job", rec.Name, "id", rec.ID, "attempts", rec.Attempt, "error", err)
}

func (m *Manager) finish(rec Record, state State, msg string) {
	now := time.Now()
	m.store.update(rec.ID, func(r *Record) {
		r.State, r.FinishedAt = state, now
		if msg != "" {
			r.Error = msg
		}
		if state == StateSucceeded {
			r.Error = ""
		}
	})
	m.store.prune(now, m.cfg.Retention, m.cfg.KeepFinished)
}

func (m *Manager) enqueue(ctx context.Context, d *definition, args json.RawMessage, ec enqueueConfig) (ID, error) {
	now := time.Now()
	rec := &Record{
		ID:          newID(),
		Name:        d.name,
		Queue:       d.queue,
		Args:        args,
		State:       StateQueued,
		MaxAttempts: d.maxAttempts,
		CreatedAt:   now,
		RunAt:       now,
	}
	if !ec.runAt.IsZero() {
		rec.RunAt = ec.runAt
	}
	if actor, ok := nexus.RequestIdentity(ctx); ok {
		rec.Actor = actor
	}
	if d.unique > 0 {
		sum := sha256.Sum256(append([]byte(d.name+"\x00"), args...))
		rec.UniqueKey = hex.EncodeToString(sum[:16])
		rec.UniqueUntil = now.Add(d.unique)
	}
	id, _ := m.store.insert(rec, now)
	m.mu.Lock()
	wake := m.wake[d.queue]
	m.mu.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
	return id, nil
}

// Get returns the job's record — state, progress, result, error.
func (m *Manager) Get(id ID) (Record, bool) { return m.store.get(id) }

// Cancel stops a job: a queued one never runs, a running one has its context
// cancelled (it ends when the job returns). Finished jobs are left as they
// are; false means there was no such job.
func (m *Manager) Cancel(id ID) bool {
	rec, ok := m.store.get(id)
	if !ok {
		return false
	}
	switch rec.State {
	case StateQueued:
		m.store.update(id, func(r *Record) {
			if r.State == StateQueued {
				r.State, r.FinishedAt, r.Error = StateCancelled, time.Now(), "cancelled"
			}
		})
	case StateRunning:
		m.mu.Lock()
		if exec, ok := m.running[id]; ok {
			exec.cancelled = true
			exec.cancel()
		}
		m.mu.Unlock()
	}
	return true
}

// Filter narrows List.
type Filter struct {
	Name  string // one job's records
	State State  // one state
	Actor string // enqueued by one user
	Limit int    // at most this many (0: all)
}

// List returns records, newest first.
func (m *Manager) List(f Filter) []Record {
	out := m.store.list(func(r *Record) bool {
		return (f.Name == "" || r.Name == f.Name) && (f.State == "" || r.State == f.State) &&
			(f.Actor == "" || r.Actor == f.Actor)
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out
}

// asResource shows the queues on the dashboard: per-queue counts and the
// latest failure, live.
func (m *Manager) asResource() resource.Resource {
	return resource.NewQueue("jobs", "Background jobs ("+m.cfg.Driver+")", nil,
		func() bool { return true },
		resource.WithDetails(m.details))
}

func (m *Manager) details() map[string]any {
	type counts struct{ running, queued, delayed, failed, succeeded int }
	per := map[string]*counts{}
	m.mu.Lock()
	for _, d := range m.defs {
		if per[d.queue] == nil {
			per[d.queue] = &counts{}
		}
	}
	names := make([]string, 0, len(m.defs))
	for n := range m.defs {
		names = append(names, n)
	}
	m.mu.Unlock()
	now := time.Now()
	var lastFailure *Record
	for _, r := range m.store.list(nil) {
		c := per[r.Queue]
		if c == nil {
			c = &counts{}
			per[r.Queue] = c
		}
		switch r.State {
		case StateRunning:
			c.running++
		case StateQueued:
			if r.RunAt.After(now) {
				c.delayed++
			} else {
				c.queued++
			}
		case StateFailed:
			c.failed++
			if lastFailure == nil {
				r := r
				lastFailure = &r
			}
		case StateSucceeded:
			c.succeeded++
		}
	}
	sort.Strings(names)
	details := map[string]any{
		"driver": m.cfg.Driver,
		"jobs":   strings.Join(names, ", "),
	}
	if m.cfg.DisableWorkers {
		details["workers"] = "off (run = false)"
	}
	for q, c := range per {
		var parts []string
		for _, n := range []struct {
			v    int
			word string
		}{{c.running, "running"}, {c.queued, "queued"}, {c.delayed, "delayed"}, {c.failed, "failed"}, {c.succeeded, "done"}} {
			if n.v > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n.v, n.word))
			}
		}
		if len(parts) == 0 {
			parts = append(parts, "idle")
		}
		workers := max(m.cfg.Queues[q], 1)
		plural := "s"
		if workers == 1 {
			plural = ""
		}
		details["queue "+q] = fmt.Sprintf("%s · %d worker%s", strings.Join(parts, " · "), workers, plural)
	}
	if lastFailure != nil {
		details["last failure"] = fmt.Sprintf("%s (%s ago): %s", lastFailure.Name,
			now.Sub(lastFailure.FinishedAt).Round(time.Second), firstLine(lastFailure.Error))
	}
	return details
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func newID() ID {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return ID(hex.EncodeToString(b[:]))
}
