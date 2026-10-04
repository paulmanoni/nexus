package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// State is where a job is in its life.
type State string

const (
	StateQueued    State = "queued"    // waiting for a worker (or for RunAt, after a delay or a failed attempt)
	StateRunning   State = "running"   // an attempt is executing
	StateSucceeded State = "succeeded" // the job returned nil
	StateFailed    State = "failed"    // the last attempt failed and no retries remain
	StateCancelled State = "cancelled" // Manager.Cancel stopped it
)

// Finished reports whether the state is final.
func (s State) Finished() bool {
	return s == StateSucceeded || s == StateFailed || s == StateCancelled
}

// Record is one enqueued job: what to run, and everything known about it.
type Record struct {
	ID              ID              `json:"id"`
	Name            string          `json:"name"`
	Queue           string          `json:"queue"`
	Args            json.RawMessage `json:"args,omitempty"`
	State           State           `json:"state"`
	Attempt         int             `json:"attempt"`
	MaxAttempts     int             `json:"maxAttempts"`
	Progress        Progress        `json:"progress"`
	Error           string          `json:"error,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	Checkpoint      json.RawMessage `json:"checkpoint,omitempty"`
	Actor           string          `json:"actor,omitempty"`
	UniqueKey       string          `json:"uniqueKey,omitempty"`
	UniqueUntil     time.Time       `json:"uniqueUntil,omitzero"`
	Worker          string          `json:"worker,omitempty"`    // the process running the current attempt
	LeaseUntil      time.Time       `json:"leaseUntil,omitzero"` // its claim, renewed while it runs
	CancelRequested bool            `json:"cancelRequested,omitempty"`
	CreatedAt       time.Time       `json:"createdAt"`
	RunAt           time.Time       `json:"runAt"`
	StartedAt       time.Time       `json:"startedAt,omitzero"`
	FinishedAt      time.Time       `json:"finishedAt,omitzero"`
}

// Progress is a job's own account of how far it got.
type Progress struct {
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Message string `json:"message,omitempty"`
}

// Store keeps jobs. The memory store is built in; jobsdb provides one on a
// SQL database, which several processes can share. Implementations are safe
// for concurrent use.
type Store interface {
	// Insert adds rec. When a job with rec.ID exists already, or rec.UniqueKey
	// is set and a job with that key is still pending and inside its
	// UniqueUntil, it returns that job's ID and false instead.
	Insert(ctx context.Context, rec Record) (ID, bool, error)

	// Claim hands worker the oldest due job of queue — a queued one whose
	// RunAt has passed or, in a shared store, a running one whose lease
	// expired — marking it running with Attempt+1 and a lease until
	// now+lease. False when nothing is due.
	Claim(ctx context.Context, queue, worker string, now time.Time, lease time.Duration) (Record, bool, error)

	// Update reads the record, lets fn change it, and writes it back when fn
	// returns true — atomically with respect to other writers. False when
	// there is no such record or fn declined.
	Update(ctx context.Context, id ID, fn func(*Record) bool) (Record, bool, error)

	Get(ctx context.Context, id ID) (Record, bool, error)
	List(ctx context.Context, f Filter) ([]Record, error)

	// NextDue is the earliest RunAt among queued jobs of queue.
	NextDue(ctx context.Context, queue string) (time.Time, bool, error)

	// Stats counts jobs per queue and state, for the dashboard.
	Stats(ctx context.Context, now time.Time) (Stats, error)

	// Prune deletes finished jobs that finished before cutoff. keep, when
	// positive, also caps how many finished jobs remain (stores may ignore
	// it; the memory store honors it).
	Prune(ctx context.Context, cutoff time.Time, keep int) error

	// Shared reports whether other processes use this store too: workers
	// then poll for work and lease what they claim.
	Shared() bool
}

// Stats is a store's count of jobs.
type Stats struct {
	Queues      map[string]QueueStats
	LastFailure *Record
}

// QueueStats counts one queue's jobs by state; Delayed are queued jobs whose
// RunAt is still ahead.
type QueueStats struct {
	Running, Queued, Delayed, Failed, Succeeded, Cancelled int
}

// Filter narrows List.
type Filter struct {
	Name  string // one job's records
	State State  // one state
	Actor string // enqueued by one user
	Limit int    // at most this many (0: all)
}

// Matches reports whether rec passes the filter (Limit aside) — for stores
// that filter in Go.
func (f Filter) Matches(rec *Record) bool {
	return (f.Name == "" || rec.Name == f.Name) && (f.State == "" || rec.State == f.State) &&
		(f.Actor == "" || rec.Actor == f.Actor)
}

// ErrLostOwnership is what a job sees (from Progress, SetResult, Checkpoint)
// once another worker has taken it over — its lease expired, say, after a
// long pause. The job should stop; the new owner runs it.
var ErrLostOwnership = errors.New("jobs: this attempt no longer owns the job")

// Run is the running attempt's handle, passed to every job: progress,
// result, checkpoints, and who enqueued it.
type Run struct {
	m       *Manager
	id      ID
	attempt int
	worker  string
	actor   string
	system  bool // AsSystem: runs without the enqueuer's identity
	ctx     context.Context
	exec    *execution
	local   *Record // broker driver: the delivered job, changed in place

	mu           sync.Mutex
	lastProgress time.Time
}

// ID is the job's ID.
func (r *Run) ID() ID { return r.id }

// Attempt counts this attempt, from 1.
func (r *Run) Attempt() int { return r.attempt }

// Actor is the user that enqueued the job, as nexus.RequestIdentity saw the
// enqueuing request ("" when there was none).
func (r *Run) Actor() string { return r.actor }

// progressEvery bounds how often Progress writes to the store.
const progressEvery = 250 * time.Millisecond

// Progress records how far the job got — done of total, and a message — for
// the dashboard and for pages polling the job. Writes are coalesced (at most
// one per 250ms, plus the final one). It returns the context's error once the
// job is cancelled, times out or the app shuts down, so a loop that reports
// progress also notices it should stop:
//
//	if err := run.Progress(i, n, "sending"); err != nil { return err }
func (r *Run) Progress(done, total int, message string) error {
	now := time.Now()
	r.mu.Lock()
	due := now.Sub(r.lastProgress) >= progressEvery || (total > 0 && done >= total)
	if due {
		r.lastProgress = now
	}
	r.mu.Unlock()
	if due {
		if err := r.write(func(rec *Record) {
			rec.Progress = Progress{Done: done, Total: total, Message: message}
		}); err != nil {
			return err
		}
	}
	return r.ctx.Err()
}

// SetResult stores the job's result (JSON) for whoever looks the job up.
func (r *Run) SetResult(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("jobs: result doesn't encode as JSON: %w", err)
	}
	return r.write(func(rec *Record) { rec.Result = raw })
}

// Checkpoint saves state (JSON) that a later attempt of this job — after a
// failure or a shutdown — reads back with Resume, so a long job continues
// where it stopped rather than starting over.
func (r *Run) Checkpoint(state any) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("jobs: checkpoint doesn't encode as JSON: %w", err)
	}
	return r.write(func(rec *Record) { rec.Checkpoint = raw })
}

// Resume loads the last Checkpoint into state; false when there is none.
func (r *Run) Resume(state any) (bool, error) {
	if r.local != nil {
		r.mu.Lock()
		cp := r.local.Checkpoint
		r.mu.Unlock()
		if len(cp) == 0 {
			return false, nil
		}
		if err := json.Unmarshal(cp, state); err != nil {
			return false, fmt.Errorf("jobs: checkpoint doesn't decode: %w", err)
		}
		return true, nil
	}
	ctx, cancel := r.m.storeCtx()
	defer cancel()
	rec, ok, err := r.m.store.Get(ctx, r.id)
	if err != nil || !ok || len(rec.Checkpoint) == 0 {
		return false, err
	}
	if err := json.Unmarshal(rec.Checkpoint, state); err != nil {
		return false, fmt.Errorf("jobs: checkpoint doesn't decode: %w", err)
	}
	return true, nil
}

// owns reports whether rec is still this attempt's to write.
func (r *Run) owns(rec *Record) bool {
	return rec.State == StateRunning && rec.Attempt == r.attempt && rec.Worker == r.worker
}

// write applies fn to the record if this attempt still owns it; when it
// doesn't, the run is cancelled and ErrLostOwnership returned.
func (r *Run) write(fn func(*Record)) error {
	if r.local != nil {
		r.mu.Lock()
		fn(r.local)
		r.mu.Unlock()
		return nil
	}
	ctx, cancel := r.m.storeCtx()
	defer cancel()
	_, ok, err := r.m.store.Update(ctx, r.id, func(rec *Record) bool {
		if !r.owns(rec) {
			return false
		}
		fn(rec)
		return true
	})
	if err != nil {
		return err
	}
	if !ok {
		r.m.loseOwnership(r.exec)
		return ErrLostOwnership
	}
	return nil
}

// memoryStore keeps records in the process.
type memoryStore struct {
	mu      sync.Mutex
	records map[ID]*Record
	order   []ID // enqueue order: claims are FIFO among due jobs
}

// NewMemoryStore returns the in-process store the memory driver uses.
func NewMemoryStore() Store { return newMemoryStore() }

func newMemoryStore() *memoryStore { return &memoryStore{records: map[ID]*Record{}} }

func (s *memoryStore) Shared() bool { return false }

func (s *memoryStore) Insert(_ context.Context, rec Record) (ID, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.records[rec.ID]; exists {
		return rec.ID, false, nil
	}
	if rec.UniqueKey != "" {
		for _, id := range s.order {
			other := s.records[id]
			if other.UniqueKey == rec.UniqueKey && !other.State.Finished() && rec.CreatedAt.Before(other.UniqueUntil) {
				return other.ID, false, nil
			}
		}
	}
	s.records[rec.ID] = &rec
	s.order = append(s.order, rec.ID)
	return rec.ID, true, nil
}

// Claim takes the oldest due queued job. Running jobs are never reclaimed:
// in one process, a running job's worker is alive.
func (s *memoryStore) Claim(_ context.Context, queue, worker string, now time.Time, lease time.Duration) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		rec := s.records[id]
		if rec.Queue == queue && rec.State == StateQueued && !rec.RunAt.After(now) {
			rec.State = StateRunning
			rec.Attempt++
			rec.StartedAt = now
			rec.Worker = worker
			rec.LeaseUntil = now.Add(lease)
			return *rec, true, nil
		}
	}
	return Record{}, false, nil
}

func (s *memoryStore) NextDue(_ context.Context, queue string) (time.Time, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next time.Time
	for _, id := range s.order {
		rec := s.records[id]
		if rec.Queue == queue && rec.State == StateQueued && (next.IsZero() || rec.RunAt.Before(next)) {
			next = rec.RunAt
		}
	}
	return next, !next.IsZero(), nil
}

func (s *memoryStore) Update(_ context.Context, id ID, fn func(*Record) bool) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return Record{}, false, nil
	}
	next := *rec
	if !fn(&next) {
		return *rec, false, nil
	}
	*rec = next
	return next, true, nil
}

func (s *memoryStore) Get(_ context.Context, id ID) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return Record{}, false, nil
	}
	return *rec, true, nil
}

// List returns matching records, newest first.
func (s *memoryStore) List(_ context.Context, f Filter) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for i := len(s.order) - 1; i >= 0; i-- {
		rec := s.records[s.order[i]]
		if f.Matches(rec) {
			out = append(out, *rec)
			if f.Limit > 0 && len(out) == f.Limit {
				break
			}
		}
	}
	return out, nil
}

func (s *memoryStore) Stats(_ context.Context, now time.Time) (Stats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{Queues: map[string]QueueStats{}}
	for i := len(s.order) - 1; i >= 0; i-- {
		rec := s.records[s.order[i]]
		q := st.Queues[rec.Queue]
		countState(&q, rec, now)
		st.Queues[rec.Queue] = q
		if rec.State == StateFailed && (st.LastFailure == nil || rec.FinishedAt.After(st.LastFailure.FinishedAt)) {
			r := *rec
			st.LastFailure = &r
		}
	}
	return st, nil
}

// countState adds rec to q.
func countState(q *QueueStats, rec *Record, now time.Time) {
	switch rec.State {
	case StateRunning:
		q.Running++
	case StateQueued:
		if rec.RunAt.After(now) {
			q.Delayed++
		} else {
			q.Queued++
		}
	case StateFailed:
		q.Failed++
	case StateSucceeded:
		q.Succeeded++
	case StateCancelled:
		q.Cancelled++
	}
}

func (s *memoryStore) Prune(_ context.Context, cutoff time.Time, keep int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	finished := 0
	for _, id := range s.order {
		if s.records[id].State.Finished() {
			finished++
		}
	}
	kept := s.order[:0]
	for _, id := range s.order {
		rec := s.records[id]
		if rec.State.Finished() && (rec.FinishedAt.Before(cutoff) || (keep > 0 && finished > keep)) {
			delete(s.records, id)
			finished--
			continue
		}
		kept = append(kept, id)
	}
	s.order = kept
	return nil
}

// SnapshotDev and RestoreDev carry the queue across `nexus dev` rebuilds.
// A job running when the snapshot was taken was interrupted, so it comes
// back queued, its attempt not counted.
func (s *memoryStore) SnapshotDev() ([]byte, error) {
	recs, _ := s.List(context.Background(), Filter{})
	return json.Marshal(recs)
}

func (s *memoryStore) RestoreDev(b []byte) error {
	var recs []Record
	if err := json.Unmarshal(b, &recs); err != nil {
		return err
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].CreatedAt.Before(recs[j].CreatedAt) })
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range recs {
		if _, exists := s.records[rec.ID]; exists {
			continue
		}
		if rec.State == StateRunning {
			rec.State, rec.Attempt, rec.StartedAt, rec.Worker, rec.LeaseUntil = StateQueued, rec.Attempt-1, time.Time{}, "", time.Time{}
		}
		s.records[rec.ID] = &rec
		s.order = append(s.order, rec.ID)
	}
	return nil
}
