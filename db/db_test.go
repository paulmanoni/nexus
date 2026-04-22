package db_test

import (
	"context"
	"testing"
	"time"

	"nexus/db"
)

type user struct {
	ID   uint
	Name string
}

func TestOpen_SQLiteInMemory_RoundTrip(t *testing.T) {
	m, err := db.Open(db.Config{Driver: db.SQLite, Database: ":memory:"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Stop()

	gdb := m.GetDB()
	if gdb == nil {
		t.Fatal("GetDB() returned nil after Open")
	}
	if err := gdb.AutoMigrate(&user{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := gdb.Create(&user{Name: "Amara"}).Error; err != nil {
		t.Fatalf("Create: %v", err)
	}
	var got user
	if err := gdb.First(&got, "name = ?", "Amara").Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.Name != "Amara" {
		t.Fatalf("wrong row: %+v", got)
	}
	if !m.IsConnected() {
		t.Fatal("IsConnected should be true")
	}
	if m.Driver() != db.SQLite {
		t.Fatalf("driver: %s", m.Driver())
	}
}

func TestDSN_AllDrivers(t *testing.T) {
	cases := []struct {
		name string
		cfg  db.Config
		want string
	}{
		{"postgres defaults", db.Config{
			Driver: db.Postgres, Host: "h", User: "u", Password: "p",
			Database: "d", Port: "5432",
		}, "host=h user=u password=p dbname=d port=5432 sslmode=disable TimeZone=UTC"},

		{"mysql", db.Config{
			Driver: db.MySQL, User: "u", Password: "p", Host: "h", Port: "3306", Database: "d",
		}, "u:p@tcp(h:3306)/d?charset=utf8mb4&parseTime=True&loc=Local"},

		{"sqlite path", db.Config{Driver: db.SQLite, Database: "/tmp/app.db"}, "/tmp/app.db"},
		{"sqlite memory", db.Config{Driver: db.SQLite, Database: ":memory:"}, ":memory:"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cfg.DSN(); got != c.want {
				t.Fatalf("DSN: %q, want %q", got, c.want)
			}
		})
	}
}

func TestPing_AfterStop(t *testing.T) {
	m, err := db.Open(db.Config{Driver: db.SQLite, Database: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	m.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := m.Ping(ctx); err == nil {
		t.Fatal("Ping should fail after Stop")
	}
	if m.IsConnected() {
		t.Fatal("IsConnected should be false after Stop")
	}
}

func TestStart_InitialConnectViaBackgroundLoop(t *testing.T) {
	m := db.NewManager(db.Config{Driver: db.SQLite, Database: ":memory:"})
	m.Start()
	defer m.Stop()
	// Background loop connects on first tick OR immediately in maintain().
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.IsConnected() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Start() did not establish connection within 2s")
}
