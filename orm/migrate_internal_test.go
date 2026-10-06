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
