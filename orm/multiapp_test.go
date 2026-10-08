package orm_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
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

type secondDB struct{ *db.Manager }

func countOrErr(ctx context.Context, _ countArgs) (string, error) {
	n, err := Users.Count(ctx)
	if err != nil {
		return err.Error(), nil
	}
	return strconv.FormatInt(n, 10), nil
}

// TestOnlyDatabase: with no db.WithDefault, a model finds the app's only
// database, and none when there are two.
func TestOnlyDatabase(t *testing.T) {
	sqlite := func() db.Config { return db.Config{Driver: db.SQLite, Database: ":memory:", LogLevel: "silent"} }
	boot := func(opts ...nexus.Option) *nexus.App {
		app, stop, err := nexus.InProcess(config.Runtime{}, append(opts, Users, nexus.AsRest("GET", "/count", countOrErr))...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stop(context.Background()) })
		return app
	}
	count := func(app *nexus.App) string {
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/count", nil))
		return rec.Body.String()
	}
	var mgr *db.Manager
	one := boot(db.Bind[mainDB]("main", sqlite), nexus.Invoke(func(m *mainDB) { mgr = m.Manager }))
	for end := time.Now().Add(5 * time.Second); !mgr.IsConnected(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("no database")
		}
	}
	s, _ := mgr.GetDB().DB()
	if err := orm.CreateTables(orm.WithDB(context.Background(), orm.Open(s, "sqlite")), Users); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(count(one)); got != `"0"` {
		t.Fatalf("one database: %s", got)
	}
	two := boot(db.Bind[mainDB]("main", sqlite), db.Bind[secondDB]("other", sqlite))
	if got := count(two); !strings.Contains(got, "no default database") {
		t.Fatalf("two databases: %s", got)
	}
}

type routedDB struct{ *db.Manager }

// TestRoutePerApp: nexus.toml's route of a table belongs to the app booted
// with it. Another app, booted after the config changed, queries its own
// database, and the first keeps its route.
func TestRoutePerApp(t *testing.T) {
	if _, err := config.Parse([]byte("[orm.models.users]\ndb = \"routed\"\n"), "nexus.toml"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = config.Parse(nil, "nexus.toml") })
	sqlite := func() db.Config { return db.Config{Driver: db.SQLite, Database: ":memory:", LogLevel: "silent"} }
	boot := func(rows int, opts ...nexus.Option) *nexus.App {
		var mgrs []*db.Manager
		app, stop, err := nexus.InProcess(config.Runtime{}, append(opts, Users, nexus.AsRest("GET", "/count", countOrErr),
			nexus.Invoke(func(app *nexus.App) {
				for _, name := range []string{"main", "routed"} {
					if m, ok := db.Lookup(app, name); ok {
						mgrs = append(mgrs, m)
					}
				}
			}))...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = stop(context.Background()) })
		for i, m := range mgrs {
			for end := time.Now().Add(5 * time.Second); !m.IsConnected(); time.Sleep(10 * time.Millisecond) {
				if time.Now().After(end) {
					t.Fatal("no database")
				}
			}
			s, _ := m.GetDB().DB()
			ctx := orm.WithDB(context.Background(), orm.Open(s, "sqlite"))
			if err := orm.CreateTables(ctx, Users); err != nil {
				t.Fatal(err)
			}
			// The last database (routed, when there is one) gets the rows.
			for j := range rows * (i + 1) / len(mgrs) {
				if err := Users.Create(ctx, &User{Name: "u", Email: strings.Repeat("y", j+1)}); err != nil {
					t.Fatal(err)
				}
			}
		}
		return app
	}
	routed := boot(1, db.Bind[mainDB]("main", sqlite, db.WithDefault()), db.Bind[routedDB]("routed", sqlite))
	if _, err := config.Parse(nil, "nexus.toml"); err != nil {
		t.Fatal(err)
	}
	plain := boot(2, db.Bind[mainDB]("main", sqlite, db.WithDefault()))
	for _, c := range []struct {
		app  *nexus.App
		want string
	}{{plain, `"2"`}, {routed, `"1"`}} {
		rec := httptest.NewRecorder()
		c.app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/count", nil))
		if got := strings.TrimSpace(rec.Body.String()); got != c.want {
			t.Fatalf("count = %s, want %s", got, c.want)
		}
	}
}
