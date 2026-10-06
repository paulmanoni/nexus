// Package ormtest opens a database for a test with the models' tables
// made: in-memory SQLite by default, or, with ORMTEST_DRIVER and
// ORMTEST_DSN set, a schema (Postgres) or database (MySQL) of its own on
// a real server, dropped when the test ends.
//
//	func TestSignup(t *testing.T) {
//		ctx := ormtest.Open(t, Users, Posts)
//		ormtest.Seed(t, ctx, Users, &User{Name: "Ali"})
//		…
//	}
//
// The app's tests import the driver of a real server they point it at
// (_ "github.com/paulmanoni/nexus/v2/db/postgres"); SQLite is linked here.
package ormtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/paulmanoni/nexus/v2/db"
	_ "github.com/paulmanoni/nexus/v2/db/sqlite"

	"github.com/paulmanoni/nexus/orm"
)

// Open is a context whose queries use a fresh database with the models'
// tables (and their many-to-many tables) made.
func Open(t testing.TB, models ...orm.Model) context.Context {
	t.Helper()
	d := open(t)
	ctx := orm.WithDB(context.Background(), d)
	if err := orm.CreateTables(ctx, models...); err != nil {
		t.Fatal(err)
	}
	return ctx
}

// Driver is the database Open gives: "sqlite", or ORMTEST_DRIVER's.
func Driver() string {
	if d := os.Getenv("ORMTEST_DRIVER"); d != "" {
		return d
	}
	return "sqlite"
}

func open(t testing.TB) *orm.DB {
	dsn := os.Getenv("ORMTEST_DSN")
	switch Driver() {
	case "postgres":
		return openPostgres(t, dsn)
	case "mysql":
		return openMySQL(t, dsn)
	}
	m, err := db.Open(db.Config{Driver: db.SQLite, Database: ":memory:", LogLevel: "silent"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	s, err := m.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	return orm.Open(s, "sqlite")
}

func name() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "ormtest_" + hex.EncodeToString(b)
}

// openPostgres makes a schema of the test's own and a pool whose
// connections search it.
func openPostgres(t testing.TB, dsn string) *orm.DB {
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := name()
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	s, err := sql.Open("pgx", dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		_ = admin.Close()
	})
	return orm.Open(s, "postgres")
}

// openMySQL makes a database of the test's own: the DSN's, renamed.
func openMySQL(t testing.TB, dsn string) *orm.DB {
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	dbName := name()
	if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
		t.Fatal(err)
	}
	slash := strings.LastIndex(dsn, "/")
	rest := dsn[slash+1:]
	params := ""
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		params = rest[i:]
	}
	if !strings.Contains(params, "parseTime") {
		if params == "" {
			params = "?parseTime=true"
		} else {
			params += "&parseTime=true"
		}
	}
	s, err := sql.Open("mysql", dsn[:slash+1]+dbName+params)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		_, _ = admin.Exec("DROP DATABASE " + dbName)
		_ = admin.Close()
	})
	return orm.Open(s, "mysql")
}

// Seed inserts rows, failing the test when it can't.
func Seed[T any](t testing.TB, ctx context.Context, m *orm.Manager[T], rows ...*T) {
	t.Helper()
	if err := m.BulkCreate(ctx, rows); err != nil {
		t.Fatal(err)
	}
}

// Exec runs SQL on ctx's database (test data the models can't write),
// failing the test when it can't.
func Exec(t testing.TB, ctx context.Context, query string, args ...any) {
	t.Helper()
	d, ok := orm.DBFrom(ctx)
	if !ok {
		t.Fatal("ormtest.Exec: ctx has no database: use the one Open returned")
	}
	if _, err := d.SQL().ExecContext(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

// CountQueries is ctx counting the statements its queries run, and how
// many so far.
func CountQueries(ctx context.Context) (context.Context, func() int64) {
	var n atomic.Int64
	ctx = orm.WithObserver(ctx, func(context.Context, orm.QueryInfo) { n.Add(1) })
	return ctx, n.Load
}
