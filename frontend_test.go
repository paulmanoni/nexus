package nexus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
)

// helper for tests — keeps the empty-config call sites readable.
var noFrontendCfg = &frontendConfig{}

// TestServeFrontend covers the dispatch shape of the SPA mount:
//   - extensionless paths get index.html (SPA routing) with
//     no-cache headers
//   - files under /assets/ get the immutable far-future cache header
//   - other dotted paths (favicon.ico, robots.txt) get served
//     without the immutable header
//   - REST routes registered alongside still win — NoRoute only
//     fires when nothing else claimed the path
func TestServeFrontend(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	fsys := fstest.MapFS{
		"index.html":             {Data: []byte("<html>app</html>")},
		"favicon.ico":             {Data: []byte("favicon-bytes")},
		"assets/main-abc.js":      {Data: []byte("console.log(1)")},
		"assets/nested/deep.css":  {Data: []byte(".x{}")},
	}

	app := New(Config{})
	app.engine.GET("/api/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })

	if err := mountFrontend(app, fsys, noFrontendCfg); err != nil {
		t.Fatalf("mountFrontend: %v", err)
	}

	type expect struct {
		status int
		body   string
		// substr in Content-Type, "" to skip
		ct string
		// substr expected in Cache-Control, "" to skip
		cache string
	}
	cases := []struct {
		name string
		path string
		expect
	}{
		{"root → index", "/", expect{200, "<html>app</html>", "text/html", "no-cache"}},
		{"index alias", "/index.html", expect{200, "<html>app</html>", "text/html", "no-cache"}},
		{"top-level favicon", "/favicon.ico", expect{200, "favicon-bytes", "", ""}},
		{"hashed asset", "/assets/main-abc.js", expect{200, "console.log(1)", "javascript", "immutable"}},
		{"nested asset", "/assets/nested/deep.css", expect{200, ".x{}", "css", "immutable"}},
		{"asset that doesn't exist", "/assets/missing.js", expect{404, "", "", ""}},
		{"SPA client route", "/users/123", expect{200, "<html>app</html>", "text/html", "no-cache"}},
		{"REST route wins", "/api/ping", expect{200, "pong", "", ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req, _ := http.NewRequest("GET", tc.path, nil)
			app.engine.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status: got %d want %d body=%q", rec.Code, tc.status, rec.Body.String())
			}
			if tc.body != "" && rec.Body.String() != tc.body {
				t.Fatalf("body: got %q want %q", rec.Body.String(), tc.body)
			}
			if tc.ct != "" && !strings.Contains(rec.Header().Get("Content-Type"), tc.ct) {
				t.Fatalf("content-type: got %q want substr %q", rec.Header().Get("Content-Type"), tc.ct)
			}
			if tc.cache != "" && !strings.Contains(rec.Header().Get("Cache-Control"), tc.cache) {
				t.Fatalf("cache-control: got %q want substr %q", rec.Header().Get("Cache-Control"), tc.cache)
			}
		})
	}
}

// TestServeFrontendMissingIndex verifies the boot-time guardrail:
// without an index.html the mount fails fast so a stale or
// unbuilt bundle surfaces at compile/start time, not at first
// request.
func TestServeFrontendMissingIndex(t *testing.T) {
	app := New(Config{})
	fsys := fstest.MapFS{"assets/main.js": {Data: []byte("x")}}
	err := mountFrontend(app, fsys, noFrontendCfg)
	if err == nil {
		t.Fatal("expected error for missing index.html, got nil")
	}
	if !strings.Contains(err.Error(), "index.html") {
		t.Fatalf("error should mention index.html: %v", err)
	}
}

// TestServeFrontendAtSubPath confirms FrontendAt nests the SPA
// under a sub-path while leaving sibling paths free for other
// handlers — the standard "REST at /api, SPA at /admin" shape.
func TestServeFrontendAtSubPath(t *testing.T) {
	app := New(Config{})
	app.engine.GET("/api/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })

	fsys := fstest.MapFS{
		"index.html":     {Data: []byte("<html>app</html>")},
		"assets/main.js": {Data: []byte("x=2")},
	}
	if err := mountFrontend(app, fsys, &frontendConfig{mountPath: "/admin"}); err != nil {
		t.Fatalf("mountFrontend: %v", err)
	}

	cases := []struct {
		path   string
		status int
		body   string
	}{
		{"/admin/", 200, "<html>app</html>"},
		{"/admin/assets/main.js", 200, "x=2"},
		{"/admin/users/123", 200, "<html>app</html>"},
		{"/api/ping", 200, "pong"},
		// Outside the SPA mount path — should not serve index.html.
		{"/", 404, ""},
		{"/users/123", 404, ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req, _ := http.NewRequest("GET", tc.path, nil)
			app.engine.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status: got %d want %d body=%q", rec.Code, tc.status, rec.Body.String())
			}
			if tc.body != "" && rec.Body.String() != tc.body {
				t.Fatalf("body: got %q want %q", rec.Body.String(), tc.body)
			}
		})
	}
}

// TestServeFrontendNormalizesMountPath verifies that FrontendAt
// accepts loose input (no leading slash, trailing slash, "/")
// and normalizes to a canonical "/seg" form internally so the
// dispatcher's prefix-strip stays straightforward.
func TestServeFrontendNormalizesMountPath(t *testing.T) {
	cases := []struct {
		in  string
		out string
	}{
		{"", ""},
		{"/", ""},
		{"admin", "/admin"},
		{"/admin", "/admin"},
		{"/admin/", "/admin"},
		{"admin/", "/admin"},
		{"/a/b/", "/a/b"},
	}
	for _, tc := range cases {
		got := normalizeRoutePrefix(tc.in)
		if got != tc.out {
			t.Errorf("normalize %q: got %q want %q", tc.in, got, tc.out)
		}
	}
}

// TestServeFrontendWithRoutePrefix confirms the SPA mount honors
// the deployment route prefix and refuses to serve unprefixed
// requests when a prefix is set — otherwise the SPA would swallow
// requests destined for a different mount sharing the listener.
func TestServeFrontendWithRoutePrefix(t *testing.T) {
	app := New(Config{Server: ServerConfig{RoutePrefix: "/v1/api"}})
	fsys := fstest.MapFS{
		"index.html":     {Data: []byte("<html>app</html>")},
		"assets/main.js": {Data: []byte("x=1")},
	}
	if err := mountFrontend(app, fsys, noFrontendCfg); err != nil {
		t.Fatalf("mountFrontend: %v", err)
	}

	cases := []struct {
		path   string
		status int
		body   string
	}{
		{"/v1/api/", 200, "<html>app</html>"},
		{"/v1/api/assets/main.js", 200, "x=1"},
		{"/v1/api/spa-route/123", 200, "<html>app</html>"},
		// Unprefixed requests should NOT serve the SPA — they 404
		// so a separate mount on the same listener can stay
		// independent.
		{"/spa-route/123", 404, ""},
		{"/assets/main.js", 404, ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req, _ := http.NewRequest("GET", tc.path, nil)
			app.engine.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status: got %d want %d body=%q", rec.Code, tc.status, rec.Body.String())
			}
			if tc.body != "" && rec.Body.String() != tc.body {
				t.Fatalf("body: got %q want %q", rec.Body.String(), tc.body)
			}
		})
	}
}
