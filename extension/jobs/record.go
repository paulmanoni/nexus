package jobs

import (
	"context"
	"encoding/json"
	"fmt"
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
	ID          ID              `json:"id"`
	Name        string          `json:"name"`
	Queue       string          `json:"queue"`
	Args        json.RawMessage `json:"args,omitempty"`
	State       State           `json:"state"`
	Attempt     int             `json:"attempt"`
	MaxAttempts int             `json:"maxAttempts"`
	Progress    Progress        `json:"progress"`
	Error       string          `json:"error,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Checkpoint  json.RawMessage `json:"checkpoint,omitempty"`
	Actor       string          `json:"actor,omitempty"`
	UniqueKey   string          `json:"uniqueKey,omitempty"`
	UniqueUntil time.Time       `json:"uniqueUntil,omitzero"`
	CreatedAt   time.Time       `json:"createdAt"`
	RunAt       time.Time       `json:"runAt"`
	StartedAt   time.Time       `json:"startedAt,omitzero"`
	FinishedAt  time.Time       `json:"finishedAt,omitzero"`
}

// Progress is a job's own account of how far it got.
type Progress struct {
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Message string `json:"message,omitempty"`
}

// Run is the running attempt's handle, passed to every job: progress,
// result, checkpoints, and who enqueued it.
type Run struct {
	m       *Manager
	id      ID
	attempt int
	actor   string
	ctx     context.Context
}

// ID is the job's ID.
func (r *Run) ID() ID { return r.id }

// Attempt counts this attempt, from 1.
func (r *Run) Attempt() int { return r.attempt }

// Actor is the user that enqueued the job, as nexus.RequestIdentity saw the
// enqueuing request ("" when there was none).
func (r *Run) Actor() string { return r.actor }

// Progress records how far the job got — done of total, and a message — for
// the dashboard and for pages polling the job. It returns the context's error
// once the job is cancelled, times out or the app shuts down, so a loop that
// reports progress also notices it should stop:
//
//	if err := run.Progress(i, n, "sending"); err != nil { return err }
func (r *Run) Progress(done, total int, message string) error {
	r.m.store.update(r.id, func(rec *Record) {
		rec.Progress = Progress{Done: done, Total: total, Message: message}
	})
	return r.ctx.Err()
}

// SetResult stores the job's result (JSON) for whoever looks the job up.
func (r *Run) SetResult(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("jobs: result doesn't encode as JSON: %w", err)
	}
	r.m.store.update(r.id, func(rec *Record) { rec.Result = raw })
	return nil
}

// Checkpoint saves state (JSON) that a later attempt of this job — after a
// failure or a shutdown — reads back with Resume, so a long job continues
// where it stopped rather than starting over.
func (r *Run) Checkpoint(state any) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("jobs: checkpoint doesn't encode as JSON: %w", err)
	}
	r.m.store.update(r.id, func(rec *Record) { rec.Checkpoint = raw })
	return nil
}

// Resume loads the last Checkpoint into state; false when there is none.
func (r *Run) Resume(state any) (bool, error) {
	rec, ok := r.m.store.get(r.id)
	if !ok || len(rec.Checkpoint) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(rec.Checkpoint, state); err != nil {
		return false, fmt.Errorf("jobs: checkpoint doesn't decode: %w", err)
	}
	return true, nil
}

// memoryStore keeps records in the process. Safe for concurrent use.
type memoryStore struct {
	mu      sync.Mutex
	records map[ID]*Record
	order   []ID // enqueue order: claims are FIFO among due jobs
}

func newMemoryStore() *memoryStore { return &memoryStore{records: map[ID]*Record{}} }

// insert adds rec, unless a live job with the same unique key exists — then
// it returns that job's ID instead.
func (s *memoryStore) insert(rec *Record, now time.Time) (ID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.UniqueKey != "" {
		for _, id := range s.order {
			other := s.records[id]
			if other.UniqueKey == rec.UniqueKey && !other.State.Finished() && now.Before(other.UniqueUntil) {
				return other.ID, false
			}
		}
	}
	s.records[rec.ID] = rec
	s.order = append(s.order, rec.ID)
	return rec.ID, true
}

// claim marks the oldest due job of queue running and returns a copy.
func (s *memoryStore) claim(queue string, now time.Time) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		rec := s.records[id]
		if rec.Queue == queue && rec.State == StateQueued && !rec.RunAt.After(now) {
			rec.State = StateRunning
			rec.Attempt++
			rec.StartedAt = now
			return *rec, true
		}
	}
	return Record{}, false
}

// nextDue is the earliest future RunAt among queued jobs of queue.
func (s *memoryStore) nextDue(queue string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next time.Time
	for _, id := range s.order {
		rec := s.records[id]
		if rec.Queue == queue && rec.State == StateQueued && (next.IsZero() || rec.RunAt.Before(next)) {
			next = rec.RunAt
		}
	}
	return next, !next.IsZero()
}

func (s *memoryStore) update(id ID, fn func(*Record)) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return Record{}, false
	}
	fn(rec)
	return *rec, true
}

func (s *memoryStore) get(id ID) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return Record{}, false
	}
	return *rec, true
}

// list returns copies of the records, newest first, that keep says to.
func (s *memoryStore) list(keep func(*Record) bool) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Record
	for i := len(s.order) - 1; i >= 0; i-- {
		rec := s.records[s.order[i]]
		if keep == nil || keep(rec) {
			out = append(out, *rec)
		}
	}
	return out
}

// prune drops finished records older than retention, and the oldest finished
// ones beyond limit.
func (s *memoryStore) prune(now time.Time, retention time.Duration, limit int) {
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
		if rec.State.Finished() && (now.Sub(rec.FinishedAt) > retention || finished > limit) {
			delete(s.records, id)
			finished--
			continue
		}
		kept = append(kept, id)
	}
	s.order = kept
}

// SnapshotDev and RestoreDev carry the queue across `nexus dev` rebuilds.
// A job running when the snapshot was taken was interrupted, so it comes
// back queued, its attempt not counted.
func (s *memoryStore) SnapshotDev() ([]byte, error) {
	return json.Marshal(s.list(nil))
}

func (s *memoryStore) RestoreDev(b []byte) error {
	var recs []Record
	if err := json.Unmarshal(b, &recs); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(recs) - 1; i >= 0; i-- { // list is newest first
		rec := recs[i]
		if _, exists := s.records[rec.ID]; exists {
			continue
		}
		if rec.State == StateRunning {
			rec.State = StateQueued
			rec.Attempt--
			rec.StartedAt = time.Time{}
		}
		s.records[rec.ID] = &rec
		s.order = append(s.order, rec.ID)
	}
	return nil
}
