// Package routertest provides a backend-agnostic conformance suite for the
// httpx.Router seam. Every adapter (stdrouter, chirouter, ginrouter, or a future
// one) must pass RunConformance to be a drop-in substitute — the framework
// relies on canonical :id / *rest routing, exact root/sub-path matching, a
// NoRoute fallback, ordered middleware with Abort, group prefixes, and Static
// behaving identically regardless of the concrete router underneath.
package routertest

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/paulmanoni/nexus/httpx"
)

// RunConformance runs the full router contract against routers produced by
// newRouter. Each subtest gets a fresh router. Call it from each adapter's test
// package:
//
//	func TestConformance(t *testing.T) { routertest.RunConformance(t, func() httpx.Router { return New() }) }
func RunConformance(t *testing.T, newRouter func() httpx.Router) {
	t.Helper()
	t.Run("named_param", func(t *testing.T) { testNamedParam(t, newRouter()) })
	t.Run("wildcard_leading_slash", func(t *testing.T) { testWildcardLeadingSlash(t, newRouter()) })
	t.Run("root_is_exact_not_subtree", func(t *testing.T) { testRootExact(t, newRouter()) })
	t.Run("trailing_slash_exact_vs_wildcard", func(t *testing.T) { testTrailingSlashVsWildcard(t, newRouter()) })
	t.Run("method_dispatch", func(t *testing.T) { testMethodDispatch(t, newRouter()) })
	t.Run("noroute_fallback", func(t *testing.T) { testNoRouteFallback(t, newRouter()) })
	t.Run("middleware_order", func(t *testing.T) { testMiddlewareOrder(t, newRouter()) })
	t.Run("abort_stops_chain", func(t *testing.T) { testAbortStopsChain(t, newRouter()) })
	t.Run("group_prefix_and_middleware", func(t *testing.T) { testGroupPrefix(t, newRouter()) })
	t.Run("static_files", func(t *testing.T) { testStatic(t, newRouter()) })
}

// do drives one request through r and returns the recorder.
func do(r httpx.Router, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func testNamedParam(t *testing.T, r httpx.Router) {
	r.Handle("GET", "/users/:id", func(c *httpx.Ctx) { c.String(200, c.Param("id")) })
	if w := do(r, "GET", "/users/42"); w.Body.String() != "42" {
		t.Fatalf(`Param("id") via /users/42 = %q; want "42"`, w.Body.String())
	}
}

func testWildcardLeadingSlash(t *testing.T, r httpx.Router) {
	// The canonical contract: a *name wildcard captures the trailing path WITH a
	// leading slash, so `"assets" + Param("filepath")` joins cleanly.
	r.Handle("GET", "/assets/*filepath", func(c *httpx.Ctx) { c.String(200, c.Param("filepath")) })
	if w := do(r, "GET", "/assets/app-Cv7.js"); w.Body.String() != "/app-Cv7.js" {
		t.Fatalf(`Param("filepath") = %q; want "/app-Cv7.js"`, w.Body.String())
	}
}

func testRootExact(t *testing.T, r httpx.Router) {
	// "GET /" must match ONLY the root, not act as a subtree catch-all that
	// shadows asset paths (which must fall through to NoRoute).
	r.Handle("GET", "/", func(c *httpx.Ctx) { c.String(200, "home") })
	r.NoRoute(func(c *httpx.Ctx) { c.String(200, "fallback") })

	if w := do(r, "GET", "/"); w.Body.String() != "home" {
		t.Fatalf("GET / = %q; want home", w.Body.String())
	}
	if w := do(r, "GET", "/assets/app.js"); w.Body.String() != "fallback" {
		t.Fatalf("GET /assets/app.js = %q; want fallback (root must not be a subtree)", w.Body.String())
	}
}

func testTrailingSlashVsWildcard(t *testing.T, r httpx.Router) {
	r.Handle("GET", "/admin/", func(c *httpx.Ctx) { c.String(200, "admin-root") })
	r.Handle("GET", "/files/*rest", func(c *httpx.Ctx) { c.String(200, "file:"+c.Param("rest")) })
	r.NoRoute(func(c *httpx.Ctx) { c.String(404, "miss") })

	cases := map[string]string{
		"/admin/":       "admin-root", // exact trailing-slash route
		"/admin/users":  "miss",       // sub-path is NOT swallowed
		"/files/a/b.js": "file:/a/b.js",
	}
	for path, want := range cases {
		if w := do(r, "GET", path); w.Body.String() != want {
			t.Fatalf("GET %s = %q; want %q", path, w.Body.String(), want)
		}
	}
}

func testMethodDispatch(t *testing.T, r httpx.Router) {
	r.Handle("GET", "/x", func(c *httpx.Ctx) { c.String(200, "get") })
	r.Handle("POST", "/x", func(c *httpx.Ctx) { c.String(200, "post") })

	if w := do(r, "GET", "/x"); w.Body.String() != "get" {
		t.Fatalf("GET /x = %q; want get", w.Body.String())
	}
	if w := do(r, "POST", "/x"); w.Body.String() != "post" {
		t.Fatalf("POST /x = %q; want post", w.Body.String())
	}
}

func testNoRouteFallback(t *testing.T, r httpx.Router) {
	r.Handle("GET", "/known", func(c *httpx.Ctx) { c.String(200, "known") })
	r.NoRoute(func(c *httpx.Ctx) { c.String(404, "miss:"+c.Request.URL.Path) })

	if w := do(r, "GET", "/nope"); w.Code != 404 || w.Body.String() != "miss:/nope" {
		t.Fatalf("GET /nope = %d %q; want 404 miss:/nope", w.Code, w.Body.String())
	}
}

func testMiddlewareOrder(t *testing.T, r httpx.Router) {
	var seen []string
	r.Use(
		func(c *httpx.Ctx) { seen = append(seen, "mw1"); c.Next() },
		func(c *httpx.Ctx) { seen = append(seen, "mw2"); c.Next() },
	)
	r.Handle("GET", "/m", func(c *httpx.Ctx) { seen = append(seen, "handler"); c.String(200, "ok") })

	if w := do(r, "GET", "/m"); w.Body.String() != "ok" {
		t.Fatalf("GET /m = %q; want ok", w.Body.String())
	}
	want := []string{"mw1", "mw2", "handler"}
	if len(seen) != len(want) {
		t.Fatalf("chain order = %v; want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("chain order = %v; want %v", seen, want)
		}
	}
}

func testAbortStopsChain(t *testing.T, r httpx.Router) {
	reached := false
	r.Use(func(c *httpx.Ctx) { c.String(401, "denied"); c.Abort() })
	r.Handle("GET", "/guarded", func(c *httpx.Ctx) { reached = true; c.String(200, "secret") })

	w := do(r, "GET", "/guarded")
	if reached {
		t.Fatal("handler ran despite Abort() in middleware")
	}
	if w.Code != 401 || w.Body.String() != "denied" {
		t.Fatalf("GET /guarded = %d %q; want 401 denied", w.Code, w.Body.String())
	}
}

func testGroupPrefix(t *testing.T, r httpx.Router) {
	hit := false
	g := r.Group("/api", func(c *httpx.Ctx) { hit = true; c.Next() })
	g.Handle("GET", "/ping", func(c *httpx.Ctx) { c.String(200, "pong") })

	if w := do(r, "GET", "/api/ping"); w.Body.String() != "pong" {
		t.Fatalf("GET /api/ping = %q; want pong", w.Body.String())
	}
	if !hit {
		t.Fatal("group middleware did not run")
	}
}

func testStatic(t *testing.T, r httpx.Router) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "logo.png"), []byte("PNG"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Static("/media", dir)

	if w := do(r, "GET", "/media/logo.png"); w.Code != http.StatusOK || w.Body.String() != "PNG" {
		t.Fatalf("GET /media/logo.png = %d %q; want 200 PNG", w.Code, w.Body.String())
	}
	if w := do(r, "HEAD", "/media/logo.png"); w.Code != http.StatusOK {
		t.Fatalf("HEAD /media/logo.png = %d; want 200", w.Code)
	}
}
