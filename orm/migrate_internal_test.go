package orm

import (
	"slices"
	"testing"
)

func TestSplitSQL(t *testing.T) {
	got := splitSQL(`-- a comment; not split
CREATE TABLE "a;b" (x TEXT DEFAULT 'it''s; fine');
/* block; comment */ INSERT INTO t VALUES ('a\'; b');
CREATE FUNCTION f() RETURNS int AS $body$ SELECT 1; $body$ LANGUAGE sql;
;
SELECT $$;$$`, true)
	want := []string{
		`CREATE TABLE "a;b" (x TEXT DEFAULT 'it''s; fine')`,
		`INSERT INTO t VALUES ('a\'; b')`,
		`CREATE FUNCTION f() RETURNS int AS $body$ SELECT 1; $body$ LANGUAGE sql`,
		`SELECT $$;$$`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	// Postgres: a backslash is an ordinary character in a string.
	got = splitSQL(`INSERT INTO t VALUES ('C:\'); SELECT 1;`, false)
	if want := []string{`INSERT INTO t VALUES ('C:\')`, `SELECT 1`}; !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestPlaceholders(t *testing.T) {
	src := "SELECT '?', \"a?\", data ?? 'k' FROM t -- a ?\nWHERE a = ? AND b IN (?, ?)"
	got, args, err := placeholders(DialectFor("postgres"), src, []any{1, "x", 3})
	if want := "SELECT '?', \"a?\", data ? 'k' FROM t \nWHERE a = $1 AND b IN ($2, $3)"; err != nil || got != want || !slices.Equal(args, []any{1, "x", 3}) {
		t.Fatalf("postgres:\n got %q, %v, %v\nwant %q", got, args, err, want)
	}
	if got, _, _ := placeholders(DialectFor("mysql"), `SELECT 'it\'s ?' WHERE a = ?`, []any{1}); got != `SELECT 'it\'s ?' WHERE a = ?` {
		t.Fatalf("mysql: %q", got)
	}
	if _, _, err := placeholders(DialectFor("sqlite"), "SELECT ?", nil); err == nil {
		t.Fatal("a ? with no argument")
	}
}
