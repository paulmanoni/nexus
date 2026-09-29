// Package jobsdb stores background jobs in a SQL database — the "db" driver
// of extension/jobs. Several processes share the queue: claims are atomic,
// leases hand a dead process's jobs to a live one, and cancels and unique
// enqueues work across processes. Postgres, MySQL and SQLite (any GORM
// dialect) work alike.
//
//	type DB struct{ *db.Manager }
//
//	nexus.Boot(
//	    db.BindFromConfig[DB]("main"),
//	    jobsdb.Bind[DB](),               // selects the db driver
//	    jobs.Module(jobs.Config{}),
//	    ExportReport,
//	)
//
// The tables (nexus_jobs, nexus_job_uniques) are created on first use;
// jobsdb.NoMigrate() leaves that to your migrations, which can call Migrate.
// Kept out of extension/jobs so an app on the memory driver links no GORM.
package jobsdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/db"
	"github.com/paulmanoni/nexus/extension/jobs"
	"github.com/paulmanoni/nexus/internal/bindutil"
)

// ErrUnavailable is returned while the database is not connected; workers
// keep polling and pick up where they left off once it is.
var ErrUnavailable = errors.New("jobsdb: database not connected")

// Bind provides a jobs.Store on database T — a type embedding *db.Manager,
// as db.Bind / db.BindFromConfig bind it. With it in DI, jobs.Module uses
// the db driver.
func Bind[T any](opts ...Option) nexus.Option {
	idx := bindutil.EmbeddedField[T]("jobsdb.Bind", reflect.TypeFor[*db.Manager](), "type DB struct{ *db.Manager }")
	return nexus.Provide(func(t *T) jobs.Store {
		m := bindutil.ManagerOf[*db.Manager](t, idx)
		return New(m.GetDB, opts...)
	})
}

// Option configures the store.
type Option func(*store)

// NoMigrate skips creating the tables on first use — run Migrate from your
// own migrations instead.
func NoMigrate() Option { return func(s *store) { s.migrate = false } }

// New returns a store on the database get returns (nil while disconnected).
func New(get func() *gorm.DB, opts ...Option) jobs.Store {
	s := &store{get: get, migrate: true}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Migrate creates or updates the job tables.
func Migrate(g *gorm.DB) error { return g.AutoMigrate(&jobRow{}, &uniqueRow{}) }

type store struct {
	get     func() *gorm.DB
	migrate bool

	mu       sync.Mutex
	migrated bool
}

func (s *store) Driver() string { return "db" }
func (s *store) Shared() bool   { return true }

// conn returns the database for one call, creating the tables first if due.
func (s *store) conn(ctx context.Context) (*gorm.DB, error) {
	g := s.get()
	if g == nil {
		return nil, ErrUnavailable
	}
	if s.migrate {
		s.mu.Lock()
		if !s.migrated {
			if err := Migrate(g.WithContext(ctx)); err != nil {
				s.mu.Unlock()
				return nil, fmt.Errorf("jobsdb: creating the job tables: %w", err)
			}
			s.migrated = true
		}
		s.mu.Unlock()
	}
	return g.WithContext(ctx), nil
}

// jobRow is a job in nexus_jobs. Version increments on every write; each
// write is conditional on the version it read, which makes claims and
// updates atomic on every SQL database without locking hints.
type jobRow struct {
	ID              string    `gorm:"primaryKey;size:32"`
	Name            string    `gorm:"size:200;not null;index:idx_nexus_jobs_name"`
	Queue           string    `gorm:"size:100;not null;index:idx_nexus_jobs_claim,priority:1"`
	State           string    `gorm:"size:16;not null;index:idx_nexus_jobs_claim,priority:2;index:idx_nexus_jobs_finished,priority:1"`
	RunAt           time.Time `gorm:"not null;index:idx_nexus_jobs_claim,priority:3"`
	Args            string    `gorm:"size:16777216"`
	Attempt         int       `gorm:"not null;default:0"`
	MaxAttempts     int       `gorm:"not null;default:1"`
	ProgressDone    int       `gorm:"not null;default:0"`
	ProgressTotal   int       `gorm:"not null;default:0"`
	ProgressMessage string    `gorm:"size:1000"`
	Error           string    `gorm:"size:16777216"`
	Result          string    `gorm:"size:16777216"`
	Checkpoint      string    `gorm:"size:16777216"`
	Actor           string    `gorm:"size:200;index:idx_nexus_jobs_actor"`
	UniqueKey       string    `gorm:"size:64"`
	UniqueUntil     *time.Time
	Worker          string `gorm:"size:128"`
	LeaseUntil      *time.Time
	CancelRequested bool      `gorm:"not null;default:false"`
	CreatedAt       time.Time `gorm:"not null;index:idx_nexus_jobs_created"`
	StartedAt       *time.Time
	FinishedAt      *time.Time `gorm:"index:idx_nexus_jobs_finished,priority:2"`
	Version         int64      `gorm:"not null;default:0"`
}

func (jobRow) TableName() string { return "nexus_jobs" }

// uniqueRow claims a unique key for the job holding it, until Until.
type uniqueRow struct {
	UniqueKey string    `gorm:"primaryKey;size:64"`
	JobID     string    `gorm:"size:32;not null"`
	Until     time.Time `gorm:"not null;index:idx_nexus_job_uniques_until"`
}

func (uniqueRow) TableName() string { return "nexus_job_uniques" }

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func timeVal(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func toRow(r jobs.Record) jobRow {
	return jobRow{
		ID: string(r.ID), Name: r.Name, Queue: r.Queue, State: string(r.State), RunAt: r.RunAt.UTC(),
		Args: string(r.Args), Attempt: r.Attempt, MaxAttempts: r.MaxAttempts,
		ProgressDone: r.Progress.Done, ProgressTotal: r.Progress.Total, ProgressMessage: r.Progress.Message,
		Error: r.Error, Result: string(r.Result), Checkpoint: string(r.Checkpoint), Actor: r.Actor,
		UniqueKey: r.UniqueKey, UniqueUntil: timePtr(r.UniqueUntil), Worker: r.Worker,
		LeaseUntil: timePtr(r.LeaseUntil), CancelRequested: r.CancelRequested, CreatedAt: r.CreatedAt.UTC(),
		StartedAt: timePtr(r.StartedAt), FinishedAt: timePtr(r.FinishedAt),
	}
}

func raw(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	return json.RawMessage(s)
}

func toRecord(r jobRow) jobs.Record {
	return jobs.Record{
		ID: jobs.ID(r.ID), Name: r.Name, Queue: r.Queue, State: jobs.State(r.State), RunAt: r.RunAt,
		Args: raw(r.Args), Attempt: r.Attempt, MaxAttempts: r.MaxAttempts,
		Progress: jobs.Progress{Done: r.ProgressDone, Total: r.ProgressTotal, Message: r.ProgressMessage},
		Error:    r.Error, Result: raw(r.Result), Checkpoint: raw(r.Checkpoint), Actor: r.Actor,
		UniqueKey: r.UniqueKey, UniqueUntil: timeVal(r.UniqueUntil), Worker: r.Worker,
		LeaseUntil: timeVal(r.LeaseUntil), CancelRequested: r.CancelRequested, CreatedAt: r.CreatedAt,
		StartedAt: timeVal(r.StartedAt), FinishedAt: timeVal(r.FinishedAt),
	}
}

// errRace marks a unique-key race another writer won; the insert retries.
var errRace = errors.New("jobsdb: unique key race")

func (s *store) Insert(ctx context.Context, rec jobs.Record) (jobs.ID, bool, error) {
	g, err := s.conn(ctx)
	if err != nil {
		return "", false, err
	}
	row := toRow(rec)
	if rec.UniqueKey == "" {
		res := g.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if res.Error != nil {
			return "", false, res.Error
		}
		return rec.ID, res.RowsAffected == 1, nil
	}
	now := rec.CreatedAt.UTC()
	for try := range 10 {
		if try > 0 {
			// Another writer holds the key (or, on SQLite, the write lock):
			// back off a little, with jitter, before looking again.
			select {
			case <-ctx.Done():
				return "", false, ctx.Err()
			case <-time.After(time.Duration(try*5+rand.IntN(10)) * time.Millisecond):
			}
		}
		var id jobs.ID
		inserted := false
		err := g.Transaction(func(tx *gorm.DB) error {
			var u uniqueRow
			switch err := tx.Where("unique_key = ?", rec.UniqueKey).Take(&u).Error; {
			case err == nil:
				if u.Until.After(now) {
					var holder jobRow
					if tx.Select("id", "state").Where("id = ?", u.JobID).Take(&holder).Error == nil &&
						!jobs.State(holder.State).Finished() {
						id = jobs.ID(u.JobID)
						return nil
					}
				}
				res := tx.Model(&uniqueRow{}).Where("unique_key = ? AND job_id = ?", u.UniqueKey, u.JobID).
					Updates(map[string]any{"job_id": row.ID, "until": rec.UniqueUntil.UTC()})
				if res.Error != nil || res.RowsAffected != 1 {
					return errRace
				}
			case errors.Is(err, gorm.ErrRecordNotFound):
				if tx.Create(&uniqueRow{UniqueKey: rec.UniqueKey, JobID: row.ID, Until: rec.UniqueUntil.UTC()}).Error != nil {
					return errRace
				}
			default:
				return err
			}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
			id, inserted = rec.ID, true
			return nil
		})
		if errors.Is(err, errRace) {
			continue
		}
		return id, inserted, err
	}
	return "", false, fmt.Errorf("jobsdb: unique key %s stayed contended", rec.UniqueKey)
}

func (s *store) Claim(ctx context.Context, queue, worker string, now time.Time, lease time.Duration) (jobs.Record, bool, error) {
	g, err := s.conn(ctx)
	if err != nil {
		return jobs.Record{}, false, err
	}
	now = now.UTC()
	for range 5 {
		var cands []jobRow
		err := g.Where("queue = ? AND ((state = ? AND run_at <= ?) OR (state = ? AND lease_until < ?))",
			queue, jobs.StateQueued, now, jobs.StateRunning, now).
			Order("run_at").Order("created_at").Limit(8).Find(&cands).Error
		if err != nil {
			return jobs.Record{}, false, err
		}
		if len(cands) == 0 {
			return jobs.Record{}, false, nil
		}
		for _, c := range cands {
			if c.State == string(jobs.StateRunning) && c.Attempt >= c.MaxAttempts {
				// Its worker died holding the last attempt: fail it.
				g.Model(&jobRow{}).Where("id = ? AND version = ?", c.ID, c.Version).Updates(map[string]any{
					"state": jobs.StateFailed, "finished_at": now, "version": c.Version + 1,
					"error": "the worker running it stopped renewing its lease (lost), with no attempts left",
				})
				continue
			}
			until := now.Add(lease)
			res := g.Model(&jobRow{}).Where("id = ? AND version = ?", c.ID, c.Version).Updates(map[string]any{
				"state": jobs.StateRunning, "attempt": c.Attempt + 1, "worker": worker,
				"lease_until": until, "started_at": now, "version": c.Version + 1,
			})
			if res.Error != nil {
				return jobs.Record{}, false, res.Error
			}
			if res.RowsAffected == 1 {
				c.State, c.Attempt, c.Worker = string(jobs.StateRunning), c.Attempt+1, worker
				c.LeaseUntil, c.StartedAt, c.Version = &until, &now, c.Version+1
				return toRecord(c), true, nil
			}
		}
	}
	return jobs.Record{}, false, nil
}

func (s *store) Update(ctx context.Context, id jobs.ID, fn func(*jobs.Record) bool) (jobs.Record, bool, error) {
	g, err := s.conn(ctx)
	if err != nil {
		return jobs.Record{}, false, err
	}
	for range 10 {
		var r jobRow
		if err := g.Where("id = ?", string(id)).Take(&r).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return jobs.Record{}, false, nil
			}
			return jobs.Record{}, false, err
		}
		rec := toRecord(r)
		if !fn(&rec) {
			return rec, false, nil
		}
		next := toRow(rec)
		next.Version = r.Version + 1
		res := g.Model(&jobRow{}).Where("id = ? AND version = ?", r.ID, r.Version).Select("*").Omit("id").Updates(&next)
		if res.Error != nil {
			return jobs.Record{}, false, res.Error
		}
		if res.RowsAffected == 1 {
			return rec, true, nil
		}
	}
	return jobs.Record{}, false, fmt.Errorf("jobsdb: job %s stayed contended", id)
}

func (s *store) Get(ctx context.Context, id jobs.ID) (jobs.Record, bool, error) {
	g, err := s.conn(ctx)
	if err != nil {
		return jobs.Record{}, false, err
	}
	var r jobRow
	if err := g.Where("id = ?", string(id)).Take(&r).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return jobs.Record{}, false, nil
		}
		return jobs.Record{}, false, err
	}
	return toRecord(r), true, nil
}

func (s *store) List(ctx context.Context, f jobs.Filter) ([]jobs.Record, error) {
	g, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	q := g.Model(&jobRow{})
	if f.Name != "" {
		q = q.Where("name = ?", f.Name)
	}
	if f.State != "" {
		q = q.Where("state = ?", string(f.State))
	}
	if f.Actor != "" {
		q = q.Where("actor = ?", f.Actor)
	}
	if f.Limit > 0 {
		q = q.Limit(f.Limit)
	}
	var rows []jobRow
	if err := q.Order("created_at DESC").Order("id DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]jobs.Record, len(rows))
	for i, r := range rows {
		out[i] = toRecord(r)
	}
	return out, nil
}

func (s *store) NextDue(ctx context.Context, queue string) (time.Time, bool, error) {
	g, err := s.conn(ctx)
	if err != nil {
		return time.Time{}, false, err
	}
	var r jobRow
	err = g.Select("run_at").Where("queue = ? AND state = ?", queue, jobs.StateQueued).Order("run_at").Take(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return r.RunAt, true, nil
}

func (s *store) Stats(ctx context.Context, now time.Time) (jobs.Stats, error) {
	g, err := s.conn(ctx)
	if err != nil {
		return jobs.Stats{}, err
	}
	var groups []struct {
		Queue   string
		State   string
		Delayed int
		N       int
	}
	err = g.Model(&jobRow{}).
		Select("queue, state, CASE WHEN state = ? AND run_at > ? THEN 1 ELSE 0 END AS delayed, COUNT(*) AS n", jobs.StateQueued, now.UTC()).
		Group("queue").Group("state").Group("delayed").Scan(&groups).Error
	if err != nil {
		return jobs.Stats{}, err
	}
	st := jobs.Stats{Queues: map[string]jobs.QueueStats{}}
	for _, grp := range groups {
		q := st.Queues[grp.Queue]
		switch jobs.State(grp.State) {
		case jobs.StateRunning:
			q.Running += grp.N
		case jobs.StateQueued:
			if grp.Delayed == 1 {
				q.Delayed += grp.N
			} else {
				q.Queued += grp.N
			}
		case jobs.StateFailed:
			q.Failed += grp.N
		case jobs.StateSucceeded:
			q.Succeeded += grp.N
		case jobs.StateCancelled:
			q.Cancelled += grp.N
		}
		st.Queues[grp.Queue] = q
	}
	var last jobRow
	err = g.Where("state = ?", jobs.StateFailed).Order("finished_at DESC").Take(&last).Error
	switch {
	case err == nil:
		rec := toRecord(last)
		st.LastFailure = &rec
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return jobs.Stats{}, err
	}
	return st, nil
}

// Prune deletes jobs finished before cutoff and expired unique keys; keep
// is not applied (retention bounds the table).
func (s *store) Prune(ctx context.Context, cutoff time.Time, _ int) error {
	g, err := s.conn(ctx)
	if err != nil {
		return err
	}
	if err := g.Where("state IN ? AND finished_at < ?",
		[]string{string(jobs.StateSucceeded), string(jobs.StateFailed), string(jobs.StateCancelled)}, cutoff.UTC()).
		Delete(&jobRow{}).Error; err != nil {
		return err
	}
	return g.Where("until < ?", time.Now().UTC()).Delete(&uniqueRow{}).Error
}
