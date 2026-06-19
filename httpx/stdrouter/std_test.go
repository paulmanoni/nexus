package stdrouter

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/paulmanoni/nexus/httpx"
)

// Static must coexist with a catch-all "GET /" route. A method-less static
// registration ("/media/") panics against "GET /" under Go 1.22's ServeMux
// (more specific path but more methods → ambiguous). Scoping Static to GET
// fixes it.
func TestStaticCoexistsWithRootCatchAll(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "logo.png"), []byte("PNG"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := New()
	r.Handle("GET", "/", func(c *httpx.Ctx) { c.String(200, "spa") })
	r.Static("/media", dir) // must not panic

	// static file is served
	req := httptest.NewRequest("GET", "/media/logo.png", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "PNG" {
		t.Fatalf("GET /media/logo.png = %d %q, want 200 PNG", w.Code, w.Body.String())
	}

	// the catch-all still wins for other paths
	req = httptest.NewRequest("GET", "/anything", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "spa" {
		t.Fatalf("GET /anything = %d %q, want 200 spa", w.Code, w.Body.String())
	}

	// HEAD on a static file works off the GET registration
	req = httptest.NewRequest("HEAD", "/media/logo.png", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD /media/logo.png = %d, want 200", w.Code)
	}
}
