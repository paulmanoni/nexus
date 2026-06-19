package stdrouter

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/paulmanoni/nexus/httpx"
)

// A "GET /" route (e.g. an Inertia/SPA home page) must match ONLY the root,
// not every path under it. ServeMux treats "/" as a subtree match, so without
// the {$} pin the home handler would shadow "GET /assets/app.js" and the
// NoRoute asset fallback would never run — the reported "no assets loaded".
func TestRootRouteIsExactNotSubtree(t *testing.T) {
	r := New()
	r.Handle("GET", "/", func(c *httpx.Ctx) { c.String(200, "home") })
	r.NoRoute(func(c *httpx.Ctx) { c.String(200, "asset:"+c.Request.URL.Path) })

	// exact root -> home page
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "home" {
		t.Fatalf("GET / = %q, want home", w.Body.String())
	}

	// asset path must fall through to NoRoute, NOT the home handler
	req = httptest.NewRequest("GET", "/assets/app.js", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "asset:/assets/app.js" {
		t.Fatalf("GET /assets/app.js = %q, want asset fallback (home route stole it)", w.Body.String())
	}
}

// A trailing-slash sub-path route ("/admin/") must also be exact, while a
// "/admin/*rest" wildcard still matches the subtree.
func TestTrailingSlashExactVsWildcard(t *testing.T) {
	r := New()
	r.Handle("GET", "/admin/", func(c *httpx.Ctx) { c.String(200, "admin-root") })
	r.Handle("GET", "/files/*rest", func(c *httpx.Ctx) { c.String(200, "file:"+c.Param("rest")) })
	r.NoRoute(func(c *httpx.Ctx) { c.String(404, "miss") })

	cases := map[string]string{
		"/admin/":       "admin-root",
		"/admin/users":  "miss",        // exact: sub-path is NOT swallowed
		"/files/a/b.js": "file:/a/b.js", // wildcard param keeps gin's leading slash
	}
	for path, want := range cases {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Body.String() != want {
			t.Fatalf("GET %s = %q, want %q", path, w.Body.String(), want)
		}
	}
}

// The wildcard param must carry gin's leading slash so the dashboard's
// `name := "assets" + c.Param("filepath")` resolves to "assets/index.js", not
// "assetsindex.js" (which 404s and serves the JS with an empty MIME type).
func TestWildcardParamLeadingSlash(t *testing.T) {
	r := New()
	var got string
	r.Handle("GET", "/assets/*filepath", func(c *httpx.Ctx) {
		got = "assets" + c.Param("filepath")
		c.String(200, got)
	})
	req := httptest.NewRequest("GET", "/assets/index-Cv7AL3WY.js", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)
	if got != "assets/index-Cv7AL3WY.js" {
		t.Fatalf(`c.Param("filepath") path = %q, want assets/index-Cv7AL3WY.js`, got)
	}
}

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
	r.Static("/media", dir)                                       // must not panic
	r.NoRoute(func(c *httpx.Ctx) { c.String(200, "spa") })        // real SPA fallback

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

	// HEAD on a static file works off the GET registration
	req = httptest.NewRequest("HEAD", "/media/logo.png", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD /media/logo.png = %d, want 200", w.Code)
	}
}
