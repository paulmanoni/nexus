package orm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/db"

	"github.com/paulmanoni/nexus/orm"
)

type countArgs struct{}

func CountUsers(ctx context.Context, _ countArgs) (int64, error) { return Users.Count(ctx) }

// TestTwoApps passes one manager to two apps in a process: each request
// queries the database of the app serving it.
func TestTwoApps(t *testing.T) {
	boot := func(rows int) *nexus.App {
		var mgr *db.Manager
		app, stop, err := nexus.InProcess(config.Runtime{},
			db.Bind[mainDB]("main", func() db.Config { return db.Config{Driver: db.SQLite, Database: ":memory:", LogLevel: "silent"} }, db.WithDefault()),
			Users,
			nexus.AsRest("GET", "/count", CountUsers),
			nexus.Invoke(func(m *mainDB) { mgr = m.Manager }),
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stop(context.Background()) })
		for end := time.Now().Add(5 * time.Second); !mgr.IsConnected(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatal("no database")
			}
		}
		s, _ := mgr.GetDB().DB()
		ctx := orm.WithDB(context.Background(), orm.Open(s, "sqlite"))
		if err := orm.CreateTables(ctx, Users); err != nil {
			t.Fatal(err)
		}
		for i := range rows {
			if err := Users.Create(ctx, &User{Name: "u", Email: strings.Repeat("x", i+1)}); err != nil {
				t.Fatal(err)
			}
		}
		return app
	}
	one, two := boot(1), boot(2)
	for _, c := range []struct {
		app  *nexus.App
		want string
	}{{one, "1"}, {two, "2"}, {one, "1"}} {
		rec := httptest.NewRecorder()
		c.app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/count", nil))
		if got := strings.TrimSpace(rec.Body.String()); got != c.want {
			t.Fatalf("count = %s, want %s", got, c.want)
		}
	}
}
