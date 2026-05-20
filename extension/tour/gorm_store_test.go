package tour

import (
	"context"
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	gormpkg "gorm.io/gorm"
)

// openGormDB stands up an in-memory sqlite + AutoMigrates the
// tour tables. Test helper — shared by every gorm_store test.
//
// Note: sqlite uses double-quotes for identifiers (matching the
// store's `Order("\"order\" ASC")` literal). Postgres + MySQL
// accept the same syntax for reserved-keyword columns, so the
// store query stays portable.
func openGormDB(t *testing.T) *gormpkg.DB {
	t.Helper()
	db, err := gormpkg.Open(sqlite.Open(":memory:"), &gormpkg.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	return db
}

// TestGormStore_RoundTrip parallels the memory-store round-trip
// test against a real gorm.DB. Confirms the schema migrates, the
// transaction-wrapped save persists, and the flat→tree hydration
// reads back what was written.
func TestGormStore_RoundTrip(t *testing.T) {
	gs, err := NewGormStore(openGormDB(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	in := &Tour{
		ID:    "t1",
		Name:  "Demo",
		Route: "/admin",
		Steps: []*Step{
			{ID: "s1", Selector: ".a", Title: "First"},
			{ID: "s2", Selector: ".b", Title: "Second", Children: []*Step{
				{ID: "s2a", Selector: ".b1", Title: "Drill"},
			}},
		},
	}
	if err := gs.UpsertTour(ctx, in); err != nil {
		t.Fatal(err)
	}
	out, err := gs.GetTour(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Steps) != 2 || out.Steps[1].ID != "s2" {
		t.Fatalf("got tree %+v, want 2 roots ending in s2", out.Steps)
	}
	if len(out.Steps[1].Children) != 1 || out.Steps[1].Children[0].ID != "s2a" {
		t.Fatalf("subtree wrong: %+v", out.Steps[1].Children)
	}
}

// TestGormStore_NotFound exercises the typed-error path so
// handlers can map ErrNotFound to HTTP 404 cleanly.
func TestGormStore_NotFound(t *testing.T) {
	gs, err := NewGormStore(openGormDB(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = gs.GetTour(context.Background(), "ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
	if err := gs.DeleteTour(context.Background(), "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteTour ghost: want ErrNotFound, got %v", err)
	}
}

// TestGormStore_UpsertTourOverwritesSteps proves the
// editor-is-authoritative contract: re-saving a tour with fewer
// steps drops the missing rows, doesn't keep them around.
func TestGormStore_UpsertTourOverwritesSteps(t *testing.T) {
	gs, err := NewGormStore(openGormDB(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := gs.UpsertTour(ctx, &Tour{
		ID: "t1", Name: "v1",
		Steps: []*Step{
			{ID: "a", Selector: ".a"},
			{ID: "b", Selector: ".b"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Second save drops step "b" entirely.
	if err := gs.UpsertTour(ctx, &Tour{
		ID: "t1", Name: "v2",
		Steps: []*Step{
			{ID: "a", Selector: ".a-renamed"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	out, err := gs.GetTour(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Steps) != 1 || out.Steps[0].Selector != ".a-renamed" {
		t.Fatalf("got %+v, want single step .a-renamed", out.Steps)
	}
}