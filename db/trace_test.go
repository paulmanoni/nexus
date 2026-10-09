package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2/db"
	"github.com/paulmanoni/nexus/v2/trace"
)

func TestGormQueriesAreTraced(t *testing.T) {
	m, err := db.Open(db.Config{Driver: db.SQLite, Database: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	gdb := m.GetDB()
	if err := gdb.AutoMigrate(&user{}); err != nil {
		t.Fatal(err)
	}

	bus := trace.NewBus(100)
	ctx, root, end := trace.NewRootSpan(trace.WithBus(context.Background(), bus), "GET /users", "users", "/users", "rest")
	gdb.WithContext(ctx).Create(&user{Name: "Neema"})
	var u user
	gdb.WithContext(ctx).Where("name = ?", "Juma").First(&u) // not found: no error on the span
	gdb.Create(&user{Name: "untraced"})
	end(200, nil)

	var sqls []string
	for _, sp := range bus.Spans(root.TraceID) {
		if sp.Name != "sql" {
			continue
		}
		if sp.Error != "" || sp.Attrs["ms"] == nil {
			t.Errorf("span %+v", sp)
		}
		sqls = append(sqls, sp.Attrs["sql"].(string))
	}
	if len(sqls) != 2 || !strings.HasPrefix(sqls[0], "INSERT INTO `users`") || !strings.Contains(sqls[1], "WHERE name = ?") {
		t.Fatalf("traced SQL = %q", sqls)
	}
}
