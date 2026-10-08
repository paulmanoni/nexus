package db_test

import (
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/paulmanoni/nexus/v2/db"
)

func TestSession_MySQLDSN(t *testing.T) {
	c := db.Config{Driver: db.MySQL, Host: "h", Port: "3306", User: "u", Password: "p", Database: "app",
		Session: map[string]string{
			"foreign_key_checks": "0",
			"sql_mode":           "NO_ENGINE_SUBSTITUTION,STRICT_TRANS_TABLES",
			"time_zone":          "+00:00",
			"wait_timeout":       "DEFAULT",
			"init":               "it's",
		}}
	dsn := c.DSN()
	q, err := url.ParseQuery(dsn[strings.Index(dsn, "?")+1:])
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"foreign_key_checks": "0",
		"sql_mode":           "'NO_ENGINE_SUBSTITUTION,STRICT_TRANS_TABLES'",
		"time_zone":          "'+00:00'",
		"wait_timeout":       "DEFAULT",
		"init":               `'it\'s'`,
		"charset":            "utf8mb4",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q (dsn %s)", k, got, want, dsn)
		}
	}
}

func TestSession_PostgresDSN(t *testing.T) {
	c := db.Config{Driver: db.Postgres, Host: "h", Port: "5432", User: "u", Password: "p", Database: "app",
		Session: map[string]string{"search_path": "app, public", "statement_timeout": "5000"}}
	dsn := c.DSN()
	for _, want := range []string{" search_path='app, public'", " statement_timeout=5000"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("dsn %q lacks %q", dsn, want)
		}
	}
}

func TestSession_SQLiteDSN(t *testing.T) {
	for _, tc := range []struct{ database, want string }{
		{"app.db", "app.db?_pragma=" + url.QueryEscape("foreign_keys(on)")},
		{"app.db?_pragma=busy_timeout(5000)", "app.db?_pragma=busy_timeout(5000)&_pragma=" + url.QueryEscape("foreign_keys(on)")},
	} {
		c := db.Config{Driver: db.SQLite, Database: tc.database, Session: map[string]string{"foreign_keys": "on"}}
		if got := c.DSN(); got != tc.want {
			t.Errorf("DSN = %q, want %q", got, tc.want)
		}
	}
}

func TestSession_Validate(t *testing.T) {
	for _, tc := range []struct {
		driver db.Driver
		key    string
		ok     bool
	}{
		{db.MySQL, "foreign_key_checks", true},
		{db.MySQL, "parseTime", false},
		{db.MySQL, "tls", false},
		{db.MySQL, "a b", false},
		{db.MySQL, "x;drop", false},
		{db.Postgres, "search_path", true},
		{db.Postgres, "app.user_id", true},
		{db.Postgres, "sslmode", false},
		{db.Postgres, "TimeZone", false},
		{db.SQLite, "foreign_keys", true},
	} {
		err := db.Config{Driver: tc.driver, Session: map[string]string{tc.key: "1"}}.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%s %q: err = %v, want ok=%v", tc.driver, tc.key, err, tc.ok)
		}
	}
}

// The pragma must hold on every pooled connection, not just the first.
func TestSession_SQLiteAppliesToEveryConnection(t *testing.T) {
	m, err := db.Open(db.Config{Driver: db.SQLite, Database: filepath.Join(t.TempDir(), "s.db"),
		Session: map[string]string{"foreign_keys": "on"}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Stop()
	sqlDB, err := m.GetDB().DB()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := sqlDB.Begin()
			if err != nil {
				t.Error(err)
				return
			}
			defer tx.Rollback()
			<-start
			var on int
			if err := tx.QueryRow("PRAGMA foreign_keys").Scan(&on); err != nil || on != 1 {
				t.Errorf("foreign_keys = %d, %v", on, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if n := sqlDB.Stats().OpenConnections; n < 2 {
		t.Logf("only %d connection(s) opened", n)
	}
}

func TestSession_BadKeyFailsOpen(t *testing.T) {
	if _, err := db.Open(db.Config{Driver: db.SQLite, Database: ":memory:",
		Session: map[string]string{"bad key": "1"}}); err == nil {
		t.Fatal("Open accepted an invalid session key")
	}
}
