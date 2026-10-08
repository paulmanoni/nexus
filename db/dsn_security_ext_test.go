package db_test

import (
	"path/filepath"
	"testing"

	"github.com/paulmanoni/nexus/v2/db"
	_ "github.com/paulmanoni/nexus/v2/db/sqlite"
)

// A session value can't run statements on SQLite's connections.
func TestSQLitePragmaInjection(t *testing.T) {
	m, err := db.Open(db.Config{Driver: db.SQLite, Database: filepath.Join(t.TempDir(), "app.db"), LogLevel: "silent",
		Session: map[string]string{"busy_timeout": "1'); CREATE TABLE pwned(x); --"}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	var n int
	if err := m.GetDB().Raw("SELECT count(*) FROM sqlite_master WHERE name = 'pwned'").Scan(&n).Error; err != nil || n != 0 {
		t.Fatalf("the session value ran as SQL: %d tables, %v", n, err)
	}
}
