// Package jobs runs background work — jobs — outside the request that asks
// for it: queued, retried, cancellable, with progress a page can show.
//
// A job is a method (or function) with a fixed shape. Its receiver comes from
// DI, like a controller's, and its arguments are a JSON-serializable struct:
//
//	func (s *ReportService) Generate(ctx context.Context, run *jobs.Run, a ReportArgs) error {
//	    for i, part := range parts {
//	        if err := run.Progress(i, len(parts), "rendering "+part.Name); err != nil {
//	            return err // cancelled or shutting down
//	        }
//	        …
//	    }
//	    return run.SetResult(ReportFile{URL: url})
//	}
//
//	var GenerateReport = jobs.Define((*ReportService).Generate,
//	    jobs.Queue("low"), jobs.Timeout(2*time.Hour), jobs.Retry(3))
//
//	nexus.Boot(jobs.Module(jobs.Config{}), GenerateReport, …)
//
//	id, err := GenerateReport.Enqueue(ctx, ReportArgs{ReportID: 7})
//
// A defined job is a nexus.Option: pass it to Boot (or let the //@job
// annotation register it). jobs.Enqueue(ctx, (*ReportService).Generate, args)
// enqueues by method expression, for jobs registered by annotation.
//
// Delivery is at least once: a job interrupted by a shutdown runs again, so
// handlers should be idempotent (Checkpoint/Resume lets a long job continue
// where it stopped rather than start over). Phase one ships the memory
// driver: jobs live in the process (and survive `nexus dev` rebuilds), not
// across restarts or replicas.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/paulmanoni/nexus"
)

// ID identifies one enqueued job.
type ID string

// ErrNotInstalled is returned by Enqueue when the job was never registered in
// a running app: jobs.Module is missing, the job's Option was not passed to
// Boot, or the app has not booted yet.
var ErrNotInstalled = errors.New("jobs: job not registered with a running app — add jobs.Module(...) and pass the job to nexus.Boot")

// Job is a defined job whose arguments are A. It is a nexus.Option (pass it
// to Boot) and the handle to enqueue it.
type Job[A any] struct {
	nexus.Option
	def *definition
}

// Name is the job's stable name: what records carry, and what the dashboard
// shows. It derives from the method ("ReportService.Generate") unless
// jobs.Name sets it.
func (j *Job[A]) Name() string { return j.def.name }

// Enqueue queues the job with args. The job runs on a worker of its queue, as
// soon as one is free, or when jobs.Delay / jobs.At allow.
func (j *Job[A]) Enqueue(ctx context.Context, args A, opts ...EnqueueOption) (ID, error) {
	return j.def.enqueue(ctx, args, opts)
}

// Define defines a job from a method expression whose receiver S is resolved
// from DI when the app boots — a service, a controller, any provided type.
func Define[S, A any](fn func(S, context.Context, *Run, A) error, opts ...Option) *Job[A] {
	if fn == nil {
		panic("jobs.Define: fn is nil")
	}
	d := newDefinition(fn, reflect.TypeFor[A](), opts)
	d.recvType = reflect.TypeFor[S]()
	d.bind = func(recv reflect.Value) {
		s := recv.Interface().(S)
		d.call = func(ctx context.Context, run *Run, raw json.RawMessage) error {
			var a A
			if err := decodeArgs(raw, &a); err != nil {
				return Permanent(err)
			}
			return fn(s, ctx, run, a)
		}
	}
	return register[A](d)
}

// DefineFunc defines a job from a plain function; it needs no DI. A closure
// needs jobs.Name, since it has no stable name of its own.
func DefineFunc[A any](fn func(context.Context, *Run, A) error, opts ...Option) *Job[A] {
	if fn == nil {
		panic("jobs.DefineFunc: fn is nil")
	}
	d := newDefinition(fn, reflect.TypeFor[A](), opts)
	d.call = func(ctx context.Context, run *Run, raw json.RawMessage) error {
		var a A
		if err := decodeArgs(raw, &a); err != nil {
			return Permanent(err)
		}
		return fn(ctx, run, a)
	}
	return register[A](d)
}

// Enqueue queues the job defined from fn — the method expression a Define
// call or a //@job annotation registered — with args.
func Enqueue[S, A any](ctx context.Context, fn func(S, context.Context, *Run, A) error, args A, opts ...EnqueueOption) (ID, error) {
	defs := lookup(fn)
	switch len(defs) {
	case 0:
		return "", fmt.Errorf("jobs: %s is not a defined job — annotate it //@job or pass jobs.Define(...) to Boot", funcName(fn))
	case 1:
		return defs[0].enqueue(ctx, args, opts)
	}
	return "", fmt.Errorf("jobs: %s is defined %d times (with different options) — enqueue through the handle jobs.Define returned", funcName(fn), len(defs))
}

// Option configures a job definition.
type Option func(*definition)

// Name sets the job's stable name; records and the dashboard use it. Set it
// to keep queued jobs attached across a rename of the method.
func Name(name string) Option { return func(d *definition) { d.name = name } }

// Queue picks the queue the job runs on (default "default"). Each queue has
// its own worker pool, sized by Config.Queues.
func Queue(name string) Option { return func(d *definition) { d.queue = name } }

// Timeout bounds one attempt: its context is cancelled after d, and the
// attempt fails (and retries, if it may).
func Timeout(d time.Duration) Option { return func(def *definition) { def.timeout = d } }

// Retry lets a failed job run again up to n more times, waiting per the
// backoff (default: 5s doubling, capped at 1h). Jobs don't retry by default.
func Retry(n int) Option { return func(d *definition) { d.maxAttempts = n + 1 } }

// Backoff sets how long a failed attempt waits before the next; attempt
// counts from 1.
func Backoff(fn func(attempt int) time.Duration) Option {
	return func(d *definition) { d.backoff = fn }
}

// Unique makes Enqueue idempotent for ttl: enqueuing the same job with the
// same arguments while an earlier one is queued or running (and younger than
// ttl) returns the earlier job's ID instead of queuing another.
func Unique(ttl time.Duration) Option { return func(d *definition) { d.unique = ttl } }

// EnqueueOption configures one Enqueue call.
type EnqueueOption func(*enqueueConfig)

type enqueueConfig struct{ runAt time.Time }

// Delay runs the job no sooner than d from now.
func Delay(d time.Duration) EnqueueOption {
	return func(c *enqueueConfig) { c.runAt = time.Now().Add(d) }
}

// At runs the job no sooner than t.
func At(t time.Time) EnqueueOption { return func(c *enqueueConfig) { c.runAt = t } }

// permanentError marks an error a retry cannot fix.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent wraps err so the job fails now instead of retrying — bad input,
// a record that no longer exists.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// definition is one job's registration.
type definition struct {
	name        string
	queue       string
	timeout     time.Duration
	maxAttempts int
	backoff     func(attempt int) time.Duration
	unique      time.Duration

	fnPtr    uintptr
	argsType reflect.Type
	recvType reflect.Type             // nil for DefineFunc
	bind     func(recv reflect.Value) // Define: binds the DI receiver
	call     func(context.Context, *Run, json.RawMessage) error

	mu  sync.RWMutex
	mgr *Manager
	err error // a bad definition, reported at boot
}

func newDefinition(fn any, argsType reflect.Type, opts []Option) *definition {
	d := &definition{
		queue:       DefaultQueue,
		maxAttempts: 1,
		fnPtr:       reflect.ValueOf(fn).Pointer(),
		argsType:    argsType,
	}
	d.name = jobName(fn)
	for _, o := range opts {
		o(d)
	}
	switch {
	case d.name == "":
		d.err = fmt.Errorf("jobs: %s has no stable name (a closure?) — add jobs.Name(\"…\")", funcName(fn))
	case d.queue == "":
		d.err = fmt.Errorf("jobs: %s: queue name is empty", d.name)
	case d.maxAttempts < 1:
		d.err = fmt.Errorf("jobs: %s: Retry needs a count of 0 or more", d.name)
	}
	if d.backoff == nil {
		d.backoff = defaultBackoff
	}
	return d
}

// DefaultQueue is the queue jobs run on unless jobs.Queue names another.
const DefaultQueue = "default"

func defaultBackoff(attempt int) time.Duration {
	d := 5 * time.Second
	for i := 1; i < attempt && d < time.Hour; i++ {
		d *= 2
	}
	return min(d, time.Hour)
}

// definitions maps a job function to its definition, for jobs.Enqueue by
// method expression. Process-wide: the function is the same in every app.
var (
	definitionsMu sync.RWMutex
	definitions   = map[uintptr][]*definition{}
)

func register[A any](d *definition) *Job[A] {
	definitionsMu.Lock()
	definitions[d.fnPtr] = append(definitions[d.fnPtr], d)
	definitionsMu.Unlock()
	return &Job[A]{Option: d.option(), def: d}
}

func lookup(fn any) []*definition {
	definitionsMu.RLock()
	defer definitionsMu.RUnlock()
	return definitions[reflect.ValueOf(fn).Pointer()]
}

// option installs the definition in the app's Manager at boot, resolving the
// receiver from DI.
func (d *definition) option() nexus.Option {
	if d.err != nil {
		return nexus.Error(d.err)
	}
	managerType := reflect.TypeFor[*Manager]()
	in := []reflect.Type{managerType}
	if d.recvType != nil {
		in = append(in, d.recvType)
	}
	errType := reflect.TypeFor[error]()
	fn := reflect.MakeFunc(reflect.FuncOf(in, []reflect.Type{errType}, false), func(args []reflect.Value) []reflect.Value {
		m := args[0].Interface().(*Manager)
		if d.bind != nil {
			d.bind(args[1])
		}
		err := m.register(d)
		out := reflect.New(errType).Elem()
		if err != nil {
			out.Set(reflect.ValueOf(err))
		}
		return []reflect.Value{out}
	})
	return nexus.Invoke(fn.Interface())
}

func (d *definition) manager() *Manager {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.mgr
}

func (d *definition) enqueue(ctx context.Context, args any, opts []EnqueueOption) (ID, error) {
	m := d.manager()
	if m == nil {
		return "", ErrNotInstalled
	}
	var ec enqueueConfig
	for _, o := range opts {
		o(&ec)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("jobs: %s: arguments don't encode as JSON: %w", d.name, err)
	}
	return m.enqueue(ctx, d, raw, ec)
}

func decodeArgs(raw json.RawMessage, out any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("jobs: arguments don't decode: %w", err)
	}
	return nil
}

// jobName derives a stable name from a function: "ReportService.Generate" for
// a method expression, "SendDigest" for a function, "" for a closure.
func jobName(fn any) string {
	name := funcName(fn)
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.Index(name, "."); i >= 0 {
		name = name[i+1:] // drop the package
	}
	if strings.Contains(name, ".func") || strings.HasSuffix(name, "-fm") {
		return ""
	}
	name = strings.NewReplacer("(*", "", ")", "").Replace(name)
	return name
}

func funcName(fn any) string {
	f := runtime.FuncForPC(reflect.ValueOf(fn).Pointer())
	if f == nil {
		return fmt.Sprintf("%T", fn)
	}
	return f.Name()
}
