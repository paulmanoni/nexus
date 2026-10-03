package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/view/example/v2/pets"
)

// The registry is built from the view/ui kit: its assets are served and
// the table renders its server state with the kit's behaviour attributes.
func TestRegistry(t *testing.T) {
	h := boot(t)
	code, page := do(t, h, "GET", "/registry", "")
	if code != 200 {
		t.Fatalf("GET /registry = %d: %s", code, page)
	}
	for _, want := range []string{
		`/_view/ui/ui.css?v=`, `/_view/ui/ui.js?v=`,
		`data-ui-tabs`, `id="kind-all"`, `nxui.loading(this,[&#34;pets&#34;])`,
		`<div id="pets"`, `name="q"`, `aria-sort="ascending"`,
		`id="row-Biscuit"`, `data-ui-href="/board"`, `data-ui-menu-trigger`, `data-ui-copy="/board#pet-Biscuit"`,
		`>1–5</b> of <b`, `aria-label="Next page"`,
		`data-ui-hotkey="mod+b"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("registry lacks %s", want)
		}
	}
	for path, typ := range map[string]string{"/_view/ui/ui.js": "javascript", "/_view/ui/ui.css": "css"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), typ) {
			t.Errorf("GET %s = %d %s", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}

// The registry's events page, sort, search, filter and remove through a
// confirmation.
func TestRegistryEvents(t *testing.T) {
	ctx := context.Background()
	store := pets.NewStore()
	r := pets.NewRegistry()
	if err := r.Mount(ctx, store); err != nil {
		t.Fatal(err)
	}
	names := func() string {
		var out []string
		for _, p := range r.Rows {
			out = append(out, p.Name)
		}
		return strings.Join(out, " ")
	}
	if got := names(); got != "Biscuit Bubbles Kiwi Luna Mochi" || r.Total != 8 {
		t.Fatalf("first page = %s (%d)", got, r.Total)
	}
	_ = r.SetPage(ctx, store, 9)
	if got := names(); got != "Nala Pepper Rex" || r.Page != 2 {
		t.Fatalf("clamped last page = %s (page %d)", got, r.Page)
	}
	_ = r.SortBy(ctx, store, "name")
	if got := names(); r.Page != 2 || !r.Desc || got != "Kiwi Bubbles Biscuit" {
		t.Fatalf("descending page 2 = %s", got)
	}
	if err := r.SortBy(ctx, store, "secret"); err == nil {
		t.Fatal("an unknown column was accepted")
	}
	_ = r.Search(ctx, store, pets.RegistryQuery{Q: "cat", Size: 10})
	if got := names(); got != "Nala Mochi" || r.Size != 10 || r.Page != 1 {
		t.Fatalf("cats = %s", got)
	}
	_ = r.SetKind(ctx, store, "dog")
	if got := names(); got != "" || r.Total != 0 {
		t.Fatalf("dogs matching cat = %s", got)
	}
	_ = r.Search(ctx, store, pets.RegistryQuery{Size: 3})
	if r.Size != 10 || r.Total != 2 {
		t.Fatalf("an unoffered size must be ignored: size %d, total %d", r.Size, r.Total)
	}
	if err := r.Remove(ctx, store); err == nil {
		t.Fatal("removed without a confirmation")
	}
	_ = r.AskRemove(ctx, store, "Pepper")
	if err := r.Remove(ctx, store); err != nil || store.Has("Pepper") || r.Confirm != "" || r.Flash != "Pepper was removed" {
		t.Fatalf("remove: %v, flash %q", err, r.Flash)
	}
}
