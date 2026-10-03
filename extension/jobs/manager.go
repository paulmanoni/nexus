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
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/di"
	"github.com/paulmanoni/nexus/v2/resource"
)

// Config configures the jobs runtime. Zero values read the [jobs] block of
// nexus.toml, then fall back to the defaults noted per field:
//
//	[jobs]
//	driver = "db"             # memory | db | redis (db and redis need a store bound)
//	run    = true             # false: this process enqueues but runs nothing
//	shutdown_grace = "10s"    # how long running jobs get to finish on shutdown
//	lease  = "30s"            # a claimed job's lease, renewed while it runs
//	poll   = "1s"             # how often idle workers look for work in a shared store
//
//	[jobs.queues]             # queue → concurrent workers
//	default = 4
//	low     = 1
type Config struct {
	// Driver names the store. "memory" keeps jobs in the process (they
	// survive `nexus dev` rebuilds, not restarts). "db" (jobsdb.Bind) and
	// "redis" (jobsredis.Bind) keep them where several processes share them.
	// Default: the bound store's driver, else "memory".
	Driver string

	// Queues maps each queue to its number of concurrent workers. Default
	// {"default": 4}; a job naming a queue missing here gets 1 worker.
	Queues map[string]int

	// DisableWorkers makes this process enqueue without running jobs — a web
	// replica beside dedicated worker replicas. nexus.toml: run = false.
	DisableWorkers bool

	// ShutdownGrace is how long running jobs may keep going once shutdown
	// begins; after it their contexts are cancelled and they are requeued.
	// Default 10s, or 0 under `nexus dev` (a rebuild shouldn't wait).
	ShutdownGrace time.Duration

	// Lease is how long a claim holds without renewal (default 30s). A
	// running job's lease is renewed every Lease/3; when a process dies,
	// another takes its jobs over once their leases lapse.
	Lease time.Duration

	// PollInterval is how often idle workers check a shared store for work
	// (default 1s). An enqueue in the same process wakes them at once.
	PollInterval time.Duration

	// Retention keeps finished jobs this long for lookups and the dashboard
	// (default 24h); KeepFinished also caps how many the memory store keeps
	// (default 1000).
	Retention    time.Duration
	KeepFinished int

	// Logger receives job failures and lifecycle events. Default
	// slog.Default().
	Logger *slog.Logger
}

// NamedStore is a Store that names its driver ("db", "redis"), so
// Config.Driver can default to it and check it.
type NamedStore interface {
	Store
	Driver() string
}

func (c Config) resolve(store Store, broker Broker) (Config, Store, error) {
	bound := ""
	switch ns, ok := store.(NamedStore); {
	case store != nil && broker != nil:
		return c, nil, errors.New("jobs: both a store and a broker are bound — bind one driver")
	case broker != nil:
		bound = broker.Driver()
	case ok:
		bound = ns.Driver()
	case store != nil:
		bound = "custom"
	}
	if c.Driver == "" {
		def := "memory"
		if bound != "" {
			def = bound
		}
		c.Driver = config.Get("jobs.driver", def)
	}
	switch {
	case c.Driver == "memory":
		store = newMemoryStore()
	case bound == "":
		return c, nil, fmt.Errorf("jobs: driver %q needs its store bound — jobsdb.Bind[YourDB](), jobsredis.Bind(…) or jobsamqp.Bind(…)", c.Driver)
	case c.Driver != bound && bound != "custom":
		return c, nil, fmt.Errorf("jobs: driver is %q but the bound store is %q", c.Driver, bound)
	}
	if c.Queues == nil {
		c.Queues = map[string]int{}
		for q, v := range config.Get[map[string]any]("jobs.queues") {
			n, err := toInt(v)
			if err != nil || n < 1 {
				return c, nil, fmt.Errorf("jobs: [jobs.queues] %s = %v — want a worker count of 1 or more", q, v)
			}
			c.Queues[q] = n
		}
		if len(c.Queues) == 0 {
			c.Queues[DefaultQueue] = 4
		}
	}
	if !c.DisableWorkers {
		c.DisableWorkers = !config.Get("jobs.run", true)
	}
	if c.ShutdownGrace == 0 {
		def := 10 * time.Second
		if nexus.IsDev() {
			def = 0
		}
		c.ShutdownGrace = config.Get("jobs.shutdown_grace", def)
	}
	if c.Lease == 0 {
		c.Lease = config.Get("jobs.lease", 30*time.Second)
	}
	if c.PollInterval == 0 {
		c.PollInterval = config.Get("jobs.poll", time.Second)
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
	return c, store, nil
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
// shutdown, and a "jobs" resource on the dashboard. A jobs.Store in DI —
// jobsdb.Bind or jobsredis.Bind provides one — selects that driver.
func Module(cfg Config) nexus.Option {
	ctor := func(lc nexus.Lifecycle, store Store, broker Broker) (*Manager, error) {
		resolved, st, err := cfg.resolve(store, broker)
		if err != nil {
			return nil, err
		}
		if broker != nil {
			st = nil
		}
		m := newManager(resolved, st)
		m.broker = broker
		lc.Append(nexus.Hook{OnStart: m.start, OnStop: m.stop})
		return m, nil
	}
	return nexus.Options(
		nexus.Raw(di.Provide(di.Annotate(ctor, di.ParamTags("", `optional:"true"`, `optional:"true"`)))),
		nexus.Invoke(func(app *nexus.App, m *Manager) { app.Register(m.asResource()) }),
	)
}

// Manager runs and tracks jobs. Inject *jobs.Manager to look a job up or
// cancel it.
type Manager struct {
	cfg    Config
	store  Store  // nil with a broker
	broker Broker // a message-broker driver, or nil
	log    *slog.Logger
	worker string // this process, as recorded on the jobs it claims

	mu        sync.Mutex
	defs      map[string]*definition
	calls     map[string]callFunc // each job, bound to this app's receiver
	running   map[ID]*execution
	pending   map[ID]int // cancels that arrived between a claim here and its run starting: attempt
	wake      map[string]chan struct{}
	schedules []*schedule

	runCtx     context.Context // cancelled after the shutdown grace
	cancelRuns context.CancelFunc
	stopping   chan struct{}
	stopOnce   sync.Once
	workers    sync.WaitGroup
	background sync.WaitGroup // heartbeat, maintenance, scheduler

	counts     brokerCounts   // broker driver: what this process ran
	deliveries sync.WaitGroup // broker driver: deliveries being handled

	statsMu   sync.Mutex
	statsAt   time.Time
	statsLast Stats
}

type execution struct {
	rec       Record
	cancel    context.CancelFunc
	cancelled bool // by Manager.Cancel, not by shutdown
	lost      bool // another worker took the job over
}

func newManager(cfg Config, store Store) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		cfg:        cfg,
		store:      store,
		log:        cfg.Logger,
		worker:     workerName(),
		defs:       map[string]*definition{},
		calls:      map[string]callFunc{},
		running:    map[ID]*execution{},
		pending:    map[ID]int{},
		wake:       map[string]chan struct{}{},
		runCtx:     ctx,
		cancelRuns: cancel,
		stopping:   make(chan struct{}),
	}
	if ms, ok := store.(*memoryStore); ok {
		nexus.PreserveDev("jobs", ms)
	}
	return m
}

func workerName() string {
	host, _ := os.Hostname()
	var b [3]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}

// storeCtx bounds one store call.
func (m *Manager) storeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// register attaches a definition to this manager; the job's queue gets a
// wake channel (and a worker pool, when it starts).
func (m *Manager) register(d *definition, call callFunc) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, dup := m.defs[d.name]; dup && prev != d {
		return fmt.Errorf("jobs: two jobs are named %q — give one jobs.Name(...)", d.name)
	}
	if m.broker != nil && d.unique > 0 {
		return fmt.Errorf("jobs: %s uses jobs.Unique, which the %s driver can't honor (it keeps no per-job state)", d.name, m.broker.Driver())
	}
	m.defs[d.name] = d
	m.calls[d.name] = call
	if _, ok := m.wake[d.queue]; !ok {
		m.wake[d.queue] = make(chan struct{}, 1)
	}
	d.mu.Lock()
	d.mgr = m
	d.mu.Unlock()
	return nil
}

func (m *Manager) start(context.Context) error {
	if m.store != nil {
		m.background.Add(1)
		go m.maintain()
	}
	if m.cfg.DisableWorkers {
		if m.store != nil && !m.store.Shared() {
			m.log.Warn("jobs: workers disabled (run = false) with the memory driver — jobs enqueued here never run")
		}
		return nil
	}
	m.mu.Lock()
	queues := make(map[string]int, len(m.wake))
	for q := range m.wake {
		queues[q] = max(m.cfg.Queues[q], 1)
	}
	hasSchedules := len(m.schedules) > 0
	m.mu.Unlock()
	if m.broker != nil {
		for q, n := range queues {
			m.workers.Add(1)
			go m.consume(q, n)
		}
		if hasSchedules {
			m.log.Info("jobs: schedules run in this process — with a message-broker driver every process that runs them enqueues each tick; run them on one replica")
		}
	} else {
		for q, n := range queues {
			for range n {
				m.workers.Add(1)
				go m.work(q)
			}
		}
		m.background.Add(1)
		go m.heartbeat()
	}
	if hasSchedules {
		m.background.Add(1)
		go m.runSchedules()
	}
	return nil
}

// stop stops claiming, gives running jobs the grace period, then cancels
// them (they are requeued) and waits for them to return.
func (m *Manager) stop(ctx context.Context) error {
	m.stopOnce.Do(func() { close(m.stopping) })
	defer m.background.Wait()
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
// woken by an enqueue here, the poll interval (shared stores), the next
// delayed job, or shutdown.
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
		ctx, cancel := m.storeCtx()
		rec, ok, err := m.store.Claim(ctx, queue, m.worker, time.Now(), m.cfg.Lease)
		cancel()
		if err != nil {
			m.log.Warn("jobs: claim failed", "queue", queue, "error", err)
		}
		if ok {
			m.execute(rec)
			continue
		}
		wait := time.Minute
		if m.store.Shared() || err != nil {
			wait = m.cfg.PollInterval
		}
		ctx, cancel = m.storeCtx()
		if next, ok, _ := m.store.NextDue(ctx, queue); ok {
			wait = min(wait, max(time.Until(next), time.Millisecond))
		}
		cancel()
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
	d, call := m.defs[rec.Name], m.calls[rec.Name]
	m.mu.Unlock()
	if d == nil || call == nil {
		m.settle(rec, func(r *Record) {
			r.State, r.FinishedAt = StateFailed, time.Now()
			r.Error = fmt.Sprintf("no job named %q is registered in this process", rec.Name)
		})
		return
	}

	var ctx context.Context
	var cancel context.CancelFunc
	if d.timeout > 0 {
		ctx, cancel = context.WithTimeout(m.runCtx, d.timeout)
	} else {
		ctx, cancel = context.WithCancel(m.runCtx)
	}
	exec := &execution{rec: rec, cancel: cancel}
	m.mu.Lock()
	m.running[rec.ID] = exec
	if attempt, ok := m.pending[rec.ID]; ok {
		delete(m.pending, rec.ID)
		rec.CancelRequested = rec.CancelRequested || attempt == rec.Attempt
	}
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		delete(m.running, rec.ID)
		m.mu.Unlock()
	}()
	if rec.CancelRequested {
		m.markCancelled(exec)
	}

	run := &Run{m: m, id: rec.ID, attempt: rec.Attempt, worker: rec.Worker, actor: rec.Actor, ctx: ctx, exec: exec}
	err := m.call(call, ctx, run, rec.Args)

	m.mu.Lock()
	byCancel, lost := exec.cancelled, exec.lost
	m.mu.Unlock()
	switch {
	case lost:
		m.log.Warn("jobs: attempt abandoned — another worker took the job over", "job", rec.Name, "id", rec.ID)
	case err == nil:
		m.settle(rec, func(r *Record) { r.State, r.FinishedAt, r.Error = StateSucceeded, time.Now(), "" })
	case byCancel:
		m.settle(rec, func(r *Record) { r.State, r.FinishedAt, r.Error = StateCancelled, time.Now(), "cancelled" })
	case m.runCtx.Err() != nil:
		// Interrupted by shutdown: back to the queue, the attempt uncounted.
		m.settle(rec, func(r *Record) {
			r.State, r.Attempt, r.RunAt = StateQueued, r.Attempt-1, time.Now()
			r.StartedAt, r.Worker, r.LeaseUntil = time.Time{}, "", time.Time{}
		})
	default:
		m.fail(d, rec, err)
	}
}

// settle writes an attempt's outcome, if the attempt still owns the job.
func (m *Manager) settle(rec Record, fn func(*Record)) {
	ctx, cancel := m.storeCtx()
	defer cancel()
	_, ok, err := m.store.Update(ctx, rec.ID, func(r *Record) bool {
		if r.State != StateRunning || r.Attempt != rec.Attempt || r.Worker != rec.Worker {
			return false
		}
		fn(r)
		return true
	})
	m.invalidateStats()
	if err != nil {
		m.log.Error("jobs: recording the outcome failed", "job", rec.Name, "id", rec.ID, "error", err)
	} else if !ok {
		m.log.Warn("jobs: outcome not recorded — the job was taken over", "job", rec.Name, "id", rec.ID)
	}
}

// call runs the job, turning a panic into an error.
func (m *Manager) call(call callFunc, ctx context.Context, run *Run, args json.RawMessage) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = Permanent(fmt.Errorf("panic: %v\n%s", p, debug.Stack()))
		}
	}()
	return call(ctx, run, args)
}

func (m *Manager) fail(d *definition, rec Record, err error) {
	var perm permanentError
	if rec.Attempt < d.maxAttempts && !errors.As(err, &perm) {
		wait := d.backoff(rec.Attempt)
		m.settle(rec, func(r *Record) {
			r.State, r.Error, r.RunAt = StateQueued, err.Error(), time.Now().Add(wait)
			r.Worker, r.LeaseUntil = "", time.Time{}
		})
		m.log.Warn("jobs: attempt failed, retrying", "job", rec.Name, "id", rec.ID,
			"attempt", rec.Attempt, "retryIn", wait, "error", err)
		return
	}
	m.settle(rec, func(r *Record) { r.State, r.FinishedAt, r.Error = StateFailed, time.Now(), err.Error() })
	m.log.Error("jobs: job failed", "job", rec.Name, "id", rec.ID, "attempts", rec.Attempt, "error", err)
}

// heartbeat renews the leases of this process's running jobs, and relays
// cancel requests another process recorded.
func (m *Manager) heartbeat() {
	defer m.background.Done()
	tick := time.NewTicker(max(m.cfg.Lease/3, 10*time.Millisecond))
	defer tick.Stop()
	for {
		select {
		case <-m.stopping:
			// Keep renewing while jobs drain: stop only once workers are done.
			done := make(chan struct{})
			go func() { m.workers.Wait(); close(done) }()
			for {
				select {
				case <-done:
					return
				case <-tick.C:
					m.renewLeases()
				}
			}
		case <-tick.C:
		}
		m.renewLeases()
	}
}

func (m *Manager) renewLeases() {
	m.mu.Lock()
	execs := make([]*execution, 0, len(m.running))
	for _, e := range m.running {
		execs = append(execs, e)
	}
	m.mu.Unlock()
	for _, e := range execs {
		ctx, cancel := m.storeCtx()
		rec, ok, err := m.store.Update(ctx, e.rec.ID, func(r *Record) bool {
			if r.State != StateRunning || r.Attempt != e.rec.Attempt || r.Worker != e.rec.Worker {
				return false
			}
			r.LeaseUntil = time.Now().Add(m.cfg.Lease)
			return true
		})
		cancel()
		switch {
		case err != nil:
			m.log.Warn("jobs: lease renewal failed", "id", e.rec.ID, "error", err)
		case !ok:
			m.loseOwnership(e)
		case rec.CancelRequested:
			m.markCancelled(e)
		}
	}
}

func (m *Manager) markCancelled(e *execution) {
	m.mu.Lock()
	e.cancelled = true
	m.mu.Unlock()
	e.cancel()
}

func (m *Manager) loseOwnership(e *execution) {
	if e == nil {
		return
	}
	m.mu.Lock()
	e.lost = true
	m.mu.Unlock()
	e.cancel()
}

// maintain prunes finished jobs once a minute.
func (m *Manager) maintain() {
	defer m.background.Done()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		m.prune()
		select {
		case <-m.stopping:
			return
		case <-tick.C:
		}
	}
}

func (m *Manager) prune() {
	ctx, cancel := m.storeCtx()
	defer cancel()
	if err := m.store.Prune(ctx, time.Now().Add(-m.cfg.Retention), m.cfg.KeepFinished); err != nil {
		m.log.Warn("jobs: pruning finished jobs failed", "error", err)
	}
}

func (m *Manager) enqueue(ctx context.Context, d *definition, args json.RawMessage, ec enqueueConfig) (ID, error) {
	now := time.Now()
	rec := Record{
		ID:          ec.id,
		Name:        d.name,
		Queue:       d.queue,
		Args:        args,
		State:       StateQueued,
		MaxAttempts: d.maxAttempts,
		CreatedAt:   now,
		RunAt:       now,
	}
	if rec.ID == "" {
		rec.ID = newID()
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
	sctx, cancel := m.storeCtx()
	defer cancel()
	id := rec.ID
	var err error
	if m.broker != nil {
		err = m.broker.Publish(sctx, rec)
	} else {
		id, _, err = m.store.Insert(sctx, rec)
	}
	if err != nil {
		return "", fmt.Errorf("jobs: enqueue %s: %w", d.name, err)
	}
	m.invalidateStats()
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
func (m *Manager) Get(ctx context.Context, id ID) (Record, bool, error) {
	if m.broker != nil {
		return Record{}, false, ErrUnsupported
	}
	return m.store.Get(ctx, id)
}

// Cancel stops a job: a queued one never runs; a running one has its context
// cancelled — at once in this process, within a lease renewal in another.
// Finished jobs are left as they are; false means there was no such job.
func (m *Manager) Cancel(ctx context.Context, id ID) (bool, error) {
	if m.broker != nil {
		return false, ErrUnsupported
	}
	rec, ok, err := m.store.Update(ctx, id, func(r *Record) bool {
		switch r.State {
		case StateQueued:
			r.State, r.FinishedAt, r.Error = StateCancelled, time.Now(), "cancelled"
			return true
		case StateRunning:
			r.CancelRequested = true
			return true
		}
		return false
	})
	if err != nil {
		return false, err
	}
	if !ok {
		_, exists, err := m.store.Get(ctx, id)
		return exists, err
	}
	if rec.State == StateRunning {
		m.mu.Lock()
		e := m.running[id]
		if e == nil && rec.Worker == m.worker {
			// Claimed here, not started yet: the run picks this up as it starts.
			m.pending[id] = rec.Attempt
		}
		m.mu.Unlock()
		if e != nil && e.rec.Attempt == rec.Attempt {
			m.markCancelled(e)
		}
	}
	return true, nil
}

// List returns records, newest first.
func (m *Manager) List(ctx context.Context, f Filter) ([]Record, error) {
	if m.broker != nil {
		return nil, ErrUnsupported
	}
	return m.store.List(ctx, f)
}

// asResource shows the queues on the dashboard: per-queue counts and the
// latest failure, live.
func (m *Manager) asResource() resource.Resource {
	return resource.NewQueue("jobs", "Background jobs ("+m.cfg.Driver+")", nil,
		func() bool { _, err := m.stats(); return err == nil },
		resource.WithDetails(m.details))
}

// invalidateStats drops the cached counts after a change made here.
func (m *Manager) invalidateStats() {
	m.statsMu.Lock()
	m.statsAt = time.Time{}
	m.statsMu.Unlock()
}

// stats returns store counts, cached for 2s (changes made in this process
// clear it): the dashboard asks every frame.
func (m *Manager) stats() (Stats, error) {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	if time.Since(m.statsAt) < 2*time.Second {
		return m.statsLast, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var st Stats
	var err error
	if m.broker != nil {
		st, err = m.brokerStats(ctx)
	} else {
		st, err = m.store.Stats(ctx, time.Now())
	}
	if err != nil {
		return Stats{}, err
	}
	m.statsAt, m.statsLast = time.Now(), st
	return st, nil
}

func (m *Manager) details() map[string]any {
	m.mu.Lock()
	names := make([]string, 0, len(m.defs))
	queues := map[string]bool{}
	for n, d := range m.defs {
		names = append(names, n)
		queues[d.queue] = true
	}
	m.mu.Unlock()
	sort.Strings(names)
	details := map[string]any{
		"driver": m.cfg.Driver,
		"jobs":   strings.Join(names, ", "),
	}
	if m.cfg.DisableWorkers {
		details["workers"] = "off (run = false)"
	}
	st, err := m.stats()
	if err != nil {
		details["store"] = "unavailable: " + firstLine(err.Error())
		return details
	}
	for q := range st.Queues {
		queues[q] = true
	}
	for q := range queues {
		c := st.Queues[q]
		var parts []string
		for _, n := range []struct {
			v    int
			word string
		}{{c.Running, "running"}, {c.Queued, "queued"}, {c.Delayed, "delayed"}, {c.Failed, "failed"}, {c.Succeeded, "done"}, {c.Cancelled, "cancelled"}} {
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
	if m.broker != nil {
		details["this process"] = fmt.Sprintf("%d running · %d done · %d failed · %d retried",
			m.counts.running.Load(), m.counts.succeeded.Load(), m.counts.failed.Load(), m.counts.retried.Load())
	}
	if f := st.LastFailure; f != nil {
		details["last failure"] = fmt.Sprintf("%s (%s ago): %s", f.Name,
			time.Since(f.FinishedAt).Round(time.Second), firstLine(f.Error))
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
