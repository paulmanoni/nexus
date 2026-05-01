package gorm_test

import (
	"context"
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/paulmanoni/nexus"
	nxgorm "github.com/paulmanoni/nexus/storage/gorm"
	gormpkg "gorm.io/gorm"
)

type Note struct {
	ID    string `gorm:"primaryKey"`
	Title string
	Body  string
}

func openDB(t *testing.T) *gormpkg.DB {
	t.Helper()
	db, err := gormpkg.Open(sqlite.Open(":memory:"), &gormpkg.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&Note{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestSaveAssignsIDOnCreate(t *testing.T) {
	store, err := nxgorm.New[Note](openDB(t))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	n := &Note{Title: "first"}
	if err := store.Save(context.Background(), n); err != nil {
		t.Fatalf("save: %v", err)
	}
	if n.ID == "" {
		t.Fatal("expected Save to assign ID, got empty")
	}
}

func TestSaveUpsertsExistingRow(t *testing.T) {
	store, err := nxgorm.New[Note](openDB(t))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	ctx := context.Background()
	n := &Note{Title: "v1"}
	if err := store.Save(ctx, n); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	id := n.ID
	n.Title = "v2"
	if err := store.Save(ctx, n); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	got, err := store.Find(ctx, id)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got.Title != "v2" {
		t.Errorf("want title v2, got %q", got.Title)
	}
}

func TestFindMissingReturnsCRUDNotFound(t *testing.T) {
	store, err := nxgorm.New[Note](openDB(t))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	_, err = store.Find(context.Background(), "nope")
	if !errors.Is(err, nexus.ErrCRUDNotFound) {
		t.Fatalf("want ErrCRUDNotFound, got %v", err)
	}
}

func TestRemoveMissingReturnsCRUDNotFound(t *testing.T) {
	store, err := nxgorm.New[Note](openDB(t))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	err = store.Remove(context.Background(), "nope")
	if !errors.Is(err, nexus.ErrCRUDNotFound) {
		t.Fatalf("want ErrCRUDNotFound, got %v", err)
	}
}

func TestRemoveExistingSucceeds(t *testing.T) {
	store, err := nxgorm.New[Note](openDB(t))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	ctx := context.Background()
	n := &Note{Title: "x"}
	if err := store.Save(ctx, n); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.Remove(ctx, n.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := store.Find(ctx, n.ID); !errors.Is(err, nexus.ErrCRUDNotFound) {
		t.Fatalf("want ErrCRUDNotFound after remove, got %v", err)
	}
}

func TestSearchPaginatesAndCounts(t *testing.T) {
	store, err := nxgorm.New[Note](openDB(t))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := store.Save(ctx, &Note{Title: "t"}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	items, total, err := store.Search(ctx, nexus.ListOptions{Limit: 2, Offset: 1})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total != 5 {
		t.Errorf("want total 5, got %d", total)
	}
	if len(items) != 2 {
		t.Errorf("want 2 items, got %d", len(items))
	}
}

func TestSearchSortsByKnownColumn(t *testing.T) {
	store, err := nxgorm.New[Note](openDB(t))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	ctx := context.Background()
	for _, title := range []string{"banana", "apple", "cherry"} {
		if err := store.Save(ctx, &Note{Title: title}); err != nil {
			t.Fatalf("save %q: %v", title, err)
		}
	}
	items, _, err := store.Search(ctx, nexus.ListOptions{Sort: []string{"-Title"}})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(items) != 3 || items[0].Title != "cherry" || items[2].Title != "apple" {
		t.Errorf("want cherry,banana,apple — got %+v", titlesOf(items))
	}
}

func TestSearchIgnoresUnknownSortField(t *testing.T) {
	store, err := nxgorm.New[Note](openDB(t))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	ctx := context.Background()
	if err := store.Save(ctx, &Note{Title: "a"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, _, err := store.Search(ctx, nexus.ListOptions{Sort: []string{"definitely_not_a_column"}}); err != nil {
		t.Fatalf("search should not error on unknown sort: %v", err)
	}
}

func TestWithSortableRestrictsAllowedColumns(t *testing.T) {
	store, err := nxgorm.New[Note](
		openDB(t),
		nxgorm.WithSortable[Note](map[string]string{"name": "title"}),
	)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	ctx := context.Background()
	for _, s := range []string{"b", "a", "c"} {
		if err := store.Save(ctx, &Note{Title: s}); err != nil {
			t.Fatalf("save %q: %v", s, err)
		}
	}
	// Allowed alias maps to title column.
	items, _, err := store.Search(ctx, nexus.ListOptions{Sort: []string{"name"}})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if items[0].Title != "a" {
		t.Errorf("want first 'a', got %q", items[0].Title)
	}
	// Schema field name should now be rejected since whitelist is set.
	items2, _, err := store.Search(ctx, nexus.ListOptions{Sort: []string{"Title"}})
	if err != nil {
		t.Fatalf("search 2: %v", err)
	}
	// No sort applied → sqlite returns insertion order; first row is "b".
	if items2[0].Title != "b" {
		t.Errorf("expected unsorted (insertion-order) result, got %q first", items2[0].Title)
	}
}

func TestWithIDGeneratorIsUsed(t *testing.T) {
	calls := 0
	store, err := nxgorm.New[Note](
		openDB(t),
		nxgorm.WithIDGenerator[Note](func() string {
			calls++
			return "fixed-id"
		}),
	)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	n := &Note{Title: "x"}
	if err := store.Save(context.Background(), n); err != nil {
		t.Fatalf("save: %v", err)
	}
	if n.ID != "fixed-id" || calls != 1 {
		t.Errorf("want id=fixed-id calls=1, got id=%q calls=%d", n.ID, calls)
	}
}

func titlesOf(ns []Note) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.Title
	}
	return out
}