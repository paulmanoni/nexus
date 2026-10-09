package orm

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/paulmanoni/nexus/v2/trace"
)

// DB is a database the ORM queries: a *sql.DB and its dialect.
type DB struct {
	sql     *sql.DB
	dialect Dialect
}

// Open is a DB over a *sql.DB of the given nexus/db driver ("postgres",
// "mysql", "sqlite"), for using the ORM outside nexus.Boot or in tests.
func Open(db *sql.DB, driver string) *DB { return &DB{sql: db, dialect: DialectFor(driver)} }

// SQL is the underlying *sql.DB.
func (d *DB) SQL() *sql.DB { return d.sql }

// Dialect is how the database speaks SQL.
func (d *DB) Dialect() Dialect { return d.dialect }

type dbKey struct{}

// WithDB has every model in ctx's queries use db, whatever database it is
// bound to: for tests and for code that runs outside nexus.Boot.
func WithDB(ctx context.Context, db *DB) context.Context {
	return context.WithValue(ctx, dbKey{}, db)
}

// DBFrom is the database WithDB put in ctx.
func DBFrom(ctx context.Context) (*DB, bool) {
	d, ok := ctx.Value(dbKey{}).(*DB)
	return d, ok
}

type usingKey struct{}

// Using sends ctx's queries to the database db.Bind registered as name,
// instead of each model's own.
func Using(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, usingKey{}, name)
}

// runner is what runs a statement: the database, or a transaction on it.
type runner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// conn is a resolved database for one statement: where to send it, and
// how it speaks.
type conn struct {
	run runner
	d   Dialect
	db  *sql.DB
}

type txKey struct{ db *sql.DB }

type txState struct {
	tx    *sql.Tx
	depth *atomic.Int64
	after *[]func(context.Context)
}

// on is db as ctx sees it: inside a transaction ctx opened on it, the
// transaction.
func on(ctx context.Context, db *DB) conn {
	c := conn{run: db.sql, d: db.dialect, db: db.sql}
	if t, ok := ctx.Value(txKey{db.sql}).(*txState); ok {
		c.run = t.tx
	}
	return c
}

// Atomic runs fn in a transaction on ctx's database (the one WithDB set,
// else the default db.Bind registered with the app a manager is bound to):
// committed when fn returns nil, rolled back when it returns an error or
// panics. Queries take part when they use the ctx fn is given. Inside
// another Atomic on the same database it is a savepoint, rolled back alone.
func Atomic(ctx context.Context, fn func(ctx context.Context) error) error {
	db, err := defaultDB(ctx)
	if err != nil {
		return err
	}
	return AtomicOn(ctx, db, fn)
}

// AtomicOn is Atomic on a given database.
func AtomicOn(ctx context.Context, db *DB, fn func(ctx context.Context) error) (err error) {
	if t, ok := ctx.Value(txKey{db.sql}).(*txState); ok {
		name := "nexus_sp_" + strconv.FormatInt(t.depth.Add(1), 10)
		if _, err := t.tx.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
			return err
		}
		// What the savepoint's writes asked to run after commit goes with
		// them when it rolls back: no signal for a row that never was.
		mark := len(*t.after)
		defer func() {
			if p := recover(); p != nil {
				_, _ = t.tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+name)
				*t.after = (*t.after)[:mark]
				panic(p)
			}
			if err != nil {
				_, _ = t.tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+name)
				*t.after = (*t.after)[:mark]
				return
			}
			_, err = t.tx.ExecContext(ctx, "RELEASE SAVEPOINT "+name)
		}()
		return fn(ctx)
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	return inTx(ctx, db.sql, tx, fn)
}

// inTx runs fn in tx, a transaction on db the queries of fn's ctx take
// part in: committed when fn returns nil, then what its writes asked to
// run after a commit; rolled back when it returns an error or panics.
func inTx(ctx context.Context, db *sql.DB, tx *sql.Tx, fn func(ctx context.Context) error) (err error) {
	var after []func(context.Context)
	state := &txState{tx: tx, depth: new(atomic.Int64), after: &after}
	inner := context.WithValue(ctx, txKey{db}, state)
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
			return
		}
		if err = tx.Commit(); err == nil {
			for _, f := range after {
				f(ctx)
			}
		}
	}()
	return fn(inner)
}

// afterCommit runs f once ctx's transaction on db commits, or now when
// there is none.
func afterCommit(ctx context.Context, db *sql.DB, f func(context.Context)) {
	if t, ok := ctx.Value(txKey{db}).(*txState); ok {
		*t.after = append(*t.after, f)
		return
	}
	f(ctx)
}

var errNoDB = errors.New("orm: no database: pass the model's orm.For to nexus.Boot (with a db.Bind), or use orm.WithDB")

// defaultDB is the database ctx names: WithDB's, else the default of the
// app the last manager was bound to.
func defaultDB(ctx context.Context) (*DB, error) {
	if db, ok := ctx.Value(dbKey{}).(*DB); ok {
		return db, nil
	}
	name, _ := ctx.Value(usingKey{}).(string)
	if b := ctxBinding(ctx); b != nil {
		return b.lookup(name)
	}
	if b := lastBinding.Load(); b != nil {
		return b.lookup(name)
	}
	return nil, errNoDB
}

// QueryInfo is a statement the ORM ran, as an Observer sees it.
type QueryInfo struct {
	SQL      string
	Args     int
	Duration time.Duration
	Err      error
}

// Observer sees each statement a context's queries run.
type Observer func(ctx context.Context, q QueryInfo)

type observerKey struct{}

// WithObserver has fn see every statement queries with ctx run: for
// logging, counting in tests, or catching repeated queries.
func WithObserver(ctx context.Context, fn Observer) context.Context {
	if prev, ok := ctx.Value(observerKey{}).(Observer); ok {
		next := fn
		fn = func(ctx context.Context, q QueryInfo) { prev(ctx, q); next(ctx, q) }
	}
	return context.WithValue(ctx, observerKey{}, fn)
}

// exec and query run a statement, as a span of ctx's trace when there is
// one, seen by ctx's observers and the dev-time repeat watch.
func (c conn) exec(ctx context.Context, q string, args []any) (sql.Result, error) {
	if err := argLimit(c.d, args); err != nil {
		return nil, err
	}
	w := watch(ctx, q, len(args))
	res, err := c.run.ExecContext(ctx, q, args...)
	w.done(err)
	return res, err
}

func (c conn) query(ctx context.Context, q string, args []any) (*sql.Rows, error) {
	if err := argLimit(c.d, args); err != nil {
		return nil, err
	}
	w := watch(ctx, q, len(args))
	rows, err := c.run.QueryContext(ctx, q, args...)
	w.done(err)
	return rows, err
}

// argLimit refuses a statement with more arguments than d's database
// takes in one (an __in of a list from a request, say), before it is
// sent: Postgres and MySQL take 65535, SQLite 32766.
func argLimit(d Dialect, args []any) error {
	limit := 65535
	if d.Name() == "sqlite" {
		limit = 32766
	}
	if len(args) > limit {
		return fmt.Errorf("orm: a statement of %d arguments: %s takes at most %d (an __in list that long belongs in a subquery or a table)", len(args), d.Name(), limit)
	}
	return nil
}

// watching is a statement being run, as watch started it: done ends it.
// A value, not a closure, so an unwatched statement allocates nothing.
type watching struct {
	ctx   context.Context
	q     string
	args  int
	start time.Time
	span  *trace.Span
	obs   Observer
}

func watch(ctx context.Context, q string, args int) watching {
	noteRun(ctx)
	noteRepeat(ctx, q)
	w := watching{ctx: ctx, q: q, args: args}
	w.obs, _ = ctx.Value(observerKey{}).(Observer)
	_, traced := trace.SpanFromCtx(ctx)
	if traced || w.obs != nil {
		w.start = time.Now()
	}
	if traced {
		_, w.span = trace.StartSpan(ctx, "sql", trace.Str("sql", q), trace.Int("args", int64(args)))
	}
	return w
}

func (w watching) done(err error) {
	if w.span != nil {
		w.span.Set("ms", fmt.Sprintf("%.2f", float64(time.Since(w.start).Microseconds())/1000))
		w.span.End(err)
	}
	if w.obs != nil {
		w.obs(w.ctx, QueryInfo{SQL: w.q, Args: w.args, Duration: time.Since(w.start), Err: err})
	}
}
