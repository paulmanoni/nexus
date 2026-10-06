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
		defer func() {
			if p := recover(); p != nil {
				_, _ = t.tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+name)
				panic(p)
			}
			if err != nil {
				_, _ = t.tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+name)
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
	var after []func(context.Context)
	state := &txState{tx: tx, depth: new(atomic.Int64), after: &after}
	inner := context.WithValue(ctx, txKey{db.sql}, state)
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
	if b := lastBinding.Load(); b != nil {
		return b.lookup(name)
	}
	return nil, errNoDB
}

// exec and query run a statement, as a span of ctx's trace when there is
// one.
func (c conn) exec(ctx context.Context, q string, args []any) (sql.Result, error) {
	done := span(ctx, q, len(args))
	res, err := c.run.ExecContext(ctx, q, args...)
	done(err)
	return res, err
}

func (c conn) query(ctx context.Context, q string, args []any) (*sql.Rows, error) {
	done := span(ctx, q, len(args))
	rows, err := c.run.QueryContext(ctx, q, args...)
	done(err)
	return rows, err
}

func span(ctx context.Context, q string, args int) func(error) {
	if _, ok := trace.SpanFromCtx(ctx); !ok {
		return func(error) {}
	}
	start := time.Now()
	_, s := trace.StartSpan(ctx, "sql", trace.Str("sql", q), trace.Int("args", int64(args)))
	return func(err error) {
		s.Set("ms", fmt.Sprintf("%.2f", float64(time.Since(start).Microseconds())/1000))
		s.End(err)
	}
}
