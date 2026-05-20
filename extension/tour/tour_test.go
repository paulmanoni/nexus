package tour

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestBuildTree_RootsAndChildren covers the happy path: a flat
// slice with mixed roots + children rebuilds into the expected
// nested shape, with children sorted by Order within each parent.
func TestBuildTree_RootsAndChildren(t *testing.T) {
	parent := "p1"
	flat := []*Step{
		{ID: "p1", Order: 0},
		{ID: "p2", Order: 1},
		{ID: "c2", ParentStepID: &parent, Order: 1},
		{ID: "c1", ParentStepID: &parent, Order: 0},
	}
	roots := buildTree(flat)
	if len(roots) != 2 {
		t.Fatalf("got %d roots, want 2", len(roots))
	}
	if roots[0].ID != "p1" || roots[1].ID != "p2" {
		t.Fatalf("root order wrong: %s, %s", roots[0].ID, roots[1].ID)
	}
	if len(roots[0].Children) != 2 {
		t.Fatalf("p1 children = %d, want 2", len(roots[0].Children))
	}
	// Children must come out sorted by Order, not insertion order.
	if roots[0].Children[0].ID != "c1" || roots[0].Children[1].ID != "c2" {
		t.Fatalf("child order wrong: %s, %s",
			roots[0].Children[0].ID, roots[0].Children[1].ID)
	}
}

// TestBuildTree_OrphansAreSkipped proves the defensive path:
// a row pointing at a non-existent parent is dropped, not crashed
// on. Keeps a stale row from blocking the whole tour from loading.
func TestBuildTree_OrphansAreSkipped(t *testing.T) {
	ghost := "missing"
	flat := []*Step{
		{ID: "real", Order: 0},
		{ID: "orphan", ParentStepID: &ghost, Order: 0},
	}
	roots := buildTree(flat)
	if len(roots) != 1 {
		t.Fatalf("got %d roots, want 1 (orphan should be skipped)", len(roots))
	}
}

// TestFlattenTree_AssignsParentAndOrder proves the inverse: a
// nested tree the editor sends gets re-flattened with Order
// renumbered from slice position + ParentStepID set from
// recursion depth.
func TestFlattenTree_AssignsParentAndOrder(t *testing.T) {
	roots := []*Step{
		{ID: "p1", Children: []*Step{
			{ID: "c1"},
			{ID: "c2"},
		}},
		{ID: "p2"},
	}
	flat := flattenTree("t1", roots)
	if len(flat) != 4 {
		t.Fatalf("got %d rows, want 4", len(flat))
	}
	idToOrder := map[string]int{}
	idToParent := map[string]string{}
	for _, s := range flat {
		idToOrder[s.ID] = s.Order
		if s.ParentStepID != nil {
			idToParent[s.ID] = *s.ParentStepID
		}
		if s.TourID != "t1" {
			t.Errorf("step %s tour_id=%q, want t1", s.ID, s.TourID)
		}
	}
	if idToOrder["p1"] != 0 || idToOrder["p2"] != 1 {
		t.Errorf("root orders wrong: p1=%d p2=%d", idToOrder["p1"], idToOrder["p2"])
	}
	if idToOrder["c1"] != 0 || idToOrder["c2"] != 1 {
		t.Errorf("child orders wrong: c1=%d c2=%d", idToOrder["c1"], idToOrder["c2"])
	}
	if idToParent["c1"] != "p1" || idToParent["c2"] != "p1" {
		t.Errorf("child parents wrong: c1=%s c2=%s",
			idToParent["c1"], idToParent["c2"])
	}
	if _, ok := idToParent["p1"]; ok {
		t.Errorf("root p1 should have nil parent")
	}
}

// TestMemoryStore_RoundTrip exercises the full upsert → get loop
// to confirm hydration is symmetric: what goes in as a tree
// comes back out as the same tree.
func TestMemoryStore_RoundTrip(t *testing.T) {
	ms := NewMemoryStore()
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
	if err := ms.UpsertTour(ctx, in); err != nil {
		t.Fatal(err)
	}
	out, err := ms.GetTour(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Steps) != 2 {
		t.Fatalf("got %d roots, want 2", len(out.Steps))
	}
	if out.Steps[1].ID != "s2" || len(out.Steps[1].Children) != 1 {
		t.Fatalf("second root subtree wrong: %#v", out.Steps[1])
	}
	if out.Steps[1].Children[0].ID != "s2a" {
		t.Errorf("child id = %q, want s2a", out.Steps[1].Children[0].ID)
	}
}

// TestMemoryStore_DeleteStepReparentsChildren proves the reparent
// path: removing a middle node moves its children up to its
// parent rather than orphaning them.
func TestMemoryStore_DeleteStepReparentsChildren(t *testing.T) {
	ms := NewMemoryStore()
	ctx := context.Background()
	if err := ms.UpsertTour(ctx, &Tour{
		ID: "t1", Name: "x",
		Steps: []*Step{
			{ID: "root", Children: []*Step{
				{ID: "mid", Children: []*Step{
					{ID: "leaf"},
				}},
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ms.DeleteStep(ctx, "mid"); err != nil {
		t.Fatal(err)
	}
	out, err := ms.GetTour(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	// leaf should have reparented to root.
	if len(out.Steps) != 1 || len(out.Steps[0].Children) != 1 {
		t.Fatalf("got tree %#v, want root with 1 child (the reparented leaf)", out.Steps)
	}
	if out.Steps[0].Children[0].ID != "leaf" {
		t.Errorf("reparented step = %q, want leaf",
			out.Steps[0].Children[0].ID)
	}
}

// TestHandlers_UpsertAndFetchTour drives the gin handlers through
// the full POST → GET cycle. Confirms IDs are minted, badge
// numbers assigned, and the response carries the canonical tree.
func TestHandlers_UpsertAndFetchTour(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &handlers{store: NewMemoryStore()}
	r := gin.New()
	r.POST("/tours", h.upsertTour)
	r.GET("/tours/:id", h.getTour)
	r.GET("/active", h.activeForRoute)

	body := `{"name":"Demo","route":"/admin","steps":[
        {"selector":".x","title":"One"},
        {"selector":".y","title":"Two","children":[
            {"selector":".y1","title":"Drill"}
        ]}
    ]}`
	req := httptest.NewRequest("POST", "/tours", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /tours: status %d, body %s", rec.Code, rec.Body.String())
	}
	var saved Tour
	if err := json.Unmarshal(rec.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.ID == "" {
		t.Fatal("saved tour has no ID")
	}
	if len(saved.Steps) != 2 {
		t.Fatalf("got %d root steps, want 2", len(saved.Steps))
	}
	// Badge numbers are 1-based, DFS order.
	if saved.Steps[0].BadgeNumber != 1 {
		t.Errorf("step1 badge = %d, want 1", saved.Steps[0].BadgeNumber)
	}
	if saved.Steps[1].Children[0].BadgeNumber == 0 {
		t.Errorf("child badge not assigned: %d",
			saved.Steps[1].Children[0].BadgeNumber)
	}

	// /active?route= must hydrate the tree (not the bare list).
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/active?route=/admin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /active: status %d", rec.Code)
	}
	var active struct {
		Tours []*Tour `json:"tours"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &active); err != nil {
		t.Fatal(err)
	}
	if len(active.Tours) != 1 || len(active.Tours[0].Steps) != 2 {
		t.Fatalf("active payload wrong: %s", rec.Body.String())
	}
}

// TestAutoInjectMiddleware confirms the script tag splices into
// the right spot for HTML responses and leaves non-HTML alone.
func TestAutoInjectMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(autoInjectMiddleware())
	r.GET("/page", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8",
			[]byte("<html><body>hi</body></html>"))
	})
	r.GET("/api", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/page", nil))
	if !bytes.Contains(rec.Body.Bytes(), []byte(`/__nexus/tour/inject.js`)) {
		t.Errorf("HTML response missing injected script: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("</body>")) {
		t.Errorf("body close tag missing post-inject: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/api", nil))
	if bytes.Contains(rec.Body.Bytes(), []byte(`inject.js`)) {
		t.Errorf("JSON response should NOT carry the script: %s", rec.Body.String())
	}
}