package orm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/db"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/middleware"

	"github.com/paulmanoni/nexus/orm"
)

func ListBooks(ctx context.Context) ([]Book, error) { return Books.OrderBy("id").All(ctx) }

// TestGraphRelation asks a nested GraphQL query of the related rows of
// every book and author: one query a level, however many parents.
func TestGraphRelation(t *testing.T) {
	var queries atomic.Int64
	count := middleware.Middleware{Name: "count-queries", HTTP: func(c *httpx.Ctx) {
		c.SetRequestContext(orm.WithObserver(c.Request.Context(), func(context.Context, orm.QueryInfo) { queries.Add(1) }))
		c.Next()
	}}
	var mgr *db.Manager
	app, stop, err := nexus.InProcess(config.Runtime{},
		db.Bind[mainDB]("main", func() db.Config { return db.Config{Driver: db.SQLite, Database: ":memory:", LogLevel: "silent"} }, db.WithDefault()),
		Books, Authors, Tags,
		Books.GraphRelation[*Author]("author"),
		Authors.GraphRelation[[]Book]("books"),
		Books.GraphRelation[[]Tag]("tags"),
		nexus.AsQuery(ListBooks),
		nexus.Middleware(count),
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
	if err := orm.CreateTables(context.Background(), Profiles, Authors, Books, Tags); err != nil {
		t.Fatal(err)
	}
	for _, s := range relData {
		if err := mgr.GetDB().Exec(s).Error; err != nil {
			t.Fatal(err)
		}
	}

	body, _ := json.Marshal(map[string]string{"query": `{ listBooks { title author { name books { title } } tags { name } } }`})
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body)))
	var out struct {
		Data struct {
			ListBooks []struct {
				Title  string
				Author struct {
					Name  string
					Books []struct{ Title string }
				}
				Tags []struct{ Name string }
			}
		}
		Errors []struct{ Message string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Errors) > 0 {
		t.Fatalf("response %s: %v", rec.Body, err)
	}
	books := out.Data.ListBooks
	if len(books) != 3 || books[0].Author.Name != "Ali" || len(books[0].Author.Books) != 2 || books[2].Author.Name != "Neema" ||
		len(books[0].Tags) != 1 || books[0].Tags[0].Name != "golang" || len(books[1].Tags) != 1 {
		t.Fatalf("books = %+v", books)
	}
	// books, their authors, the authors' books, the books' tags (the
	// table between, then the tags): five, not one per book.
	if n := queries.Load(); n != 5 {
		t.Fatalf("%d queries for a three-level query of three books; want 5", n)
	}
	if !strings.Contains(rec.Body.String(), "Learning Go") {
		t.Fatal("missing a book")
	}
}

func TestGraphRelationChecksTypes(t *testing.T) {
	_, stop, err := nexus.InProcess(config.Runtime{}, Books.GraphRelation[[]Author]("author"))
	if stop != nil {
		t.Cleanup(func() { _ = stop(context.Background()) })
	}
	if err == nil || !strings.Contains(err.Error(), "the field is *orm_test.Author") {
		t.Fatalf("boot = %v", err)
	}
}
