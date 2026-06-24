package stdrouter

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/paulmanoni/nexus/httpx"
	"github.com/paulmanoni/nexus/httpx/routertest"
)

// The stdlib adapter must satisfy the shared router seam contract (named/
// wildcard params, exact root + sub-path matching, NoRoute, middleware order +
// Abort, groups, Static). The cross-backend spec lives in routertest.
func TestConformance(t *testing.T) {
	routertest.RunConformance(t, func() httpx.Router { return New() })
}

// TestStaticCoexistsWithRootCatchAll is stdlib-specific: a method-less Static
// registration ("/media/") panics against a "GET /" catch-all under Go 1.22's
// ServeMux (more specific path but more methods → ambiguous). Scoping Static to
// GET fixes it. Kept here because it pins a ServeMux quirk the other backends
// don't have.
func TestStaticCoexistsWithRootCatchAll(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "logo.png"), []byte("PNG"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := New()
	r.Handle("GET", "/", func(c *httpx.Ctx) { c.String(200, "spa") })
	r.Static("/media", dir)                                // must not panic
	r.NoRoute(func(c *httpx.Ctx) { c.String(200, "spa") }) // real SPA fallback

	// static file is served
	req := httptest.NewRequest("GET", "/media/logo.png", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "PNG" {
		t.Fatalf("GET /media/logo.png = %d %q, want 200 PNG", w.Code, w.Body.String())
	}

	// non-asset client routes fall to the NoRoute SPA fallback
	req = httptest.NewRequest("GET", "/anything", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "spa" {
		t.Fatalf("GET /anything = %d %q, want 200 spa", w.Code, w.Body.String())
	}
}
