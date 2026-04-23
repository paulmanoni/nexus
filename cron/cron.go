// Package cron registers and runs scheduled jobs alongside the app's HTTP
// surface. Jobs run bare — no middleware chain applies — but the scheduler
// publishes request-lifecycle events to trace.Bus so the dashboard shows each
// run like any other traced call.
//
// Jobs are identified by name. Re-registering a name replaces the prior job
// (its history is dropped). Schedules use robfig cron 5-field syntax; @every
// and @hourly style descriptors are also accepted.
package cron

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	rc "github.com/robfig/cron/v3"

	"github.com/paulmanoni/nexus/trace"
)

// HandlerFunc is the user-supplied work. Return an error to mark the run
// failed; it surfaces on the dashboard and the next scheduled tick still fires.
type HandlerFunc func(ctx context.Context) error

// Job is the user-facing registration value. Service is optional — when set,
// the dashboard can group crons under the service node.
type Job struct {
	Name        string
	Schedule    string
	Description string
	Service     string
	Handler     HandlerFunc
}

// RunRecord is one execution's outcome.
type RunRecord struct {
	Started    time.Time `json:"started"`
	DurationMs int64     `json:"durationMs"`
	Success    bool      `json:"success"`
	Error      string    `json:"error,omitempty"`
	Manual     bool      `json:"manual,omitempty"`
}

// Snapshot is the serializable view the dashboard reads.
type Snapshot struct {
	Name        string      `json:"name"`
	Schedule    string      `json:"schedule"`
	Description string      `json:"description,omitempty"`
	Service     string      `json:"service,omitempty"`
	Paused      bool        `json:"paused"`
	Running     bool        `json:"running"`
	NextRun     *time.Time  `json:"nextRun,omitempty"`
	LastRun     *RunRecord  `json:"lastRun,omitempty"`
	History     []RunRecord `json:"history,omitempty"`
}

const defaultHistory = 20

type scheduled struct {
	job     Job
	entryID rc.EntryID
	paused  atomic.Bool
	running atomic.Bool

	mu      sync.Mutex
	history []RunRecord // newest first
}

// Scheduler owns the underlying cron and the set of registered jobs. One per
// App; safe for concurrent use.
type Scheduler struct {
	mu         sync.RWMutex
	cron       *rc.Cron
	bus        *trace.Bus
	historyCap int
	jobs       map[string]*scheduled
	started    atomic.Bool
}

// NewScheduler builds a Scheduler. historyCap is the per-job run buffer; pass
// 0 for the default of 20.
func NewScheduler(bus *trace.Bus, historyCap int) *Scheduler {
	if historyCap <= 0 {
		historyCap = defaultHistory
	}
	return &Scheduler{
		cron:       rc.New(),
		bus:        bus,
		historyCap: historyCap,
		jobs:       map[string]*scheduled{},
	}
}

// Register adds or replaces a job. If the scheduler has already been Started,
// the job is scheduled immediately; otherwise it schedules on Start.
func (s *Scheduler) Register(j Job) error {
	if j.Name == "" {
		return fmt.Errorf("cron: job name required")
	}
	if j.Schedule == "" {
		return fmt.Errorf("cron: job %q missing schedule", j.Name)
	}
	if j.Handler == nil {
		return fmt.Errorf("cron: job %q missing handler", j.Name)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.jobs[j.Name]; ok {
		s.cron.Remove(existing.entryID)
	}

	sj := &scheduled{job: j}
	id, err := s.cron.AddFunc(j.Schedule, func() { s.run(sj, false) })
	if err != nil {
		return fmt.Errorf("cron: job %q bad schedule %q: %w", j.Name, j.Schedule, err)
	}
	sj.entryID = id
	s.jobs[j.Name] = sj
	return nil
}

// Start begins dispatching ticks. Safe to call once; subsequent calls no-op.
func (s *Scheduler) Start() {
	if s.started.Swap(true) {
		return
	}
	s.cron.Start()
}

// Stop halts the dispatcher and waits for any in-flight run to finish.
func (s *Scheduler) Stop() {
	if !s.started.Load() {
		return
	}
	ctx := s.cron.Stop()
	<-ctx.Done()
}

// Trigger runs the named job immediately in a goroutine, marked as a manual
// run. Returns false if the name is unknown.
func (s *Scheduler) Trigger(name string) bool {
	s.mu.RLock()
	sj, ok := s.jobs[name]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	go s.run(sj, true)
	return true
}

// SetPaused toggles the pause bit on a job. Paused jobs still appear in the
// snapshot (with Paused: true) but the scheduler skips their tick fires.
// Manual Trigger bypasses the pause flag on purpose — operators often pause
// to then run ad-hoc.
func (s *Scheduler) SetPaused(name string, paused bool) bool {
	s.mu.RLock()
	sj, ok := s.jobs[name]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	sj.paused.Store(paused)
	return true
}

// Snapshots returns the current view of every registered job, sorted by name.
func (s *Scheduler) Snapshots() []Snapshot {
	s.mu.RLock()
	jobs := make([]*scheduled, 0, len(s.jobs))
	for _, sj := range s.jobs {
		jobs = append(jobs, sj)
	}
	s.mu.RUnlock()

	out := make([]Snapshot, 0, len(jobs))
	for _, sj := range jobs {
		out = append(out, s.snapshot(sj))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Snapshot returns the view of one named job, or false if unknown.
func (s *Scheduler) Snapshot(name string) (Snapshot, bool) {
	s.mu.RLock()
	sj, ok := s.jobs[name]
	s.mu.RUnlock()
	if !ok {
		return Snapshot{}, false
	}
	return s.snapshot(sj), true
}

func (s *Scheduler) snapshot(sj *scheduled) Snapshot {
	sj.mu.Lock()
	history := make([]RunRecord, len(sj.history))
	copy(history, sj.history)
	sj.mu.Unlock()

	snap := Snapshot{
		Name:        sj.job.Name,
		Schedule:    sj.job.Schedule,
		Description: sj.job.Description,
		Service:     sj.job.Service,
		Paused:      sj.paused.Load(),
		Running:     sj.running.Load(),
		History:     history,
	}
	if len(history) > 0 {
		last := history[0]
		snap.LastRun = &last
	}
	if entry := s.cron.Entry(sj.entryID); entry.ID != 0 && !snap.Paused {
		next := entry.Next
		if !next.IsZero() {
			snap.NextRun = &next
		}
	}
	return snap
}

func (s *Scheduler) run(sj *scheduled, manual bool) {
	if !manual && sj.paused.Load() {
		return
	}
	if !sj.running.CompareAndSwap(false, true) {
		// Previous run still in flight. Drop this tick; manual presses
		// also get ignored rather than stacking up.
		return
	}
	defer sj.running.Store(false)

	traceID := trace.NewTraceID()
	start := time.Now()

	if s.bus != nil {
		msg := "cron.tick"
		if manual {
			msg = "cron.manual"
		}
		s.bus.Publish(trace.Event{
			TraceID:   traceID,
			Kind:      trace.KindRequestStart,
			Service:   sj.job.Service,
			Endpoint:  sj.job.Name,
			Transport: "cron",
			Method:    sj.job.Schedule,
			Message:   msg,
		})
	}

	err := safeInvoke(sj.job.Handler)
	dur := time.Since(start)

	rec := RunRecord{
		Started:    start,
		DurationMs: dur.Milliseconds(),
		Success:    err == nil,
		Manual:     manual,
	}
	if err != nil {
		rec.Error = err.Error()
	}

	sj.mu.Lock()
	sj.history = append([]RunRecord{rec}, sj.history...)
	if len(sj.history) > s.historyCap {
		sj.history = sj.history[:s.historyCap]
	}
	sj.mu.Unlock()

	if s.bus != nil {
		ev := trace.Event{
			TraceID:    traceID,
			Kind:       trace.KindRequestEnd,
			Service:    sj.job.Service,
			Endpoint:   sj.job.Name,
			Transport:  "cron",
			Method:     sj.job.Schedule,
			DurationMs: dur.Milliseconds(),
		}
		if err != nil {
			ev.Error = err.Error()
			ev.Status = 500
		} else {
			ev.Status = 200
		}
		s.bus.Publish(ev)
	}
}

func safeInvoke(h HandlerFunc) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return h(context.Background())
}