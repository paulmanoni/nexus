package nexus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
)

// TestServeFrontend covers the four behaviors that matter:
//   - index.html serves at "/"
//   - top-level files (favicon.ico) serve at "/<file>"
//   - subdirectory files (assets) serve at "/<dir>/<file>"
//   - SPA fallback: unknown paths return index.html (so client-
//     side routers like React Router work)
//
// REST routes registered alongside still win — the fallback only
// runs when nothing else claimed the path.
func TestServeFrontend(t *testing.T) {
	t.Setenv("GIN_MODE", "test")
	fsys := fstest.MapFS{
		"index.html":              {Data: []byte("<html>app</html>")},
		"favicon.ico":              {Data: []byte("favicon-bytes")},
		"assets/main-abc.js":       {Data: []byte("console.log(1)")},
		"assets/nested/deep.css":   {Data: []byte(".x{}")},
	}

	app := New(Config{})

	// Add a sentinel REST route that must keep winning over the
	// SPA fallback. Mount it directly on the engine since AsRest's
	// fx.Invoke path needs a full Run.
	app.engine.GET("/api/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })

	if err := mountFrontend(app, fsys); err != nil {
		t.Fatalf("mountFrontend: %v", err)
	}

	cases := []struct {
		name   string
		path   string
		status int
		body   string
		ct     string
	}{
		{"root serves index", "/", 200, "<html>app</html>", "text/html"},
		{"index alias", "/index.html", 200, "<html>app</html>", "text/html"},
		{"top-level favicon", "/favicon.ico", 200, "favicon-bytes", ""},
		{"asset file", "/assets/main-abc.js", 200, "console.log(1)", "javascript"},
		{"nested asset", "/assets/nested/deep.css", 200, ".x{}", "css"},
		{"unknown asset 404 inside the asset dir", "/assets/missing.js", 404, "", ""},
		{"SPA fallback for client route", "/users/123", 200, "<html>app</html>", "text/html"},
		{"REST route wins over fallback", "/api/ping", 200, "pong", ""},
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
	err := mountFrontend(app, fsys)
	if err == nil {
		t.Fatal("expected error for missing index.html, got nil")
	}
	if !strings.Contains(err.Error(), "index.html") {
		t.Fatalf("error should mention index.html: %v", err)
	}
}

// TestServeFrontendWithRoutePrefix confirms that the deployment
// route prefix wraps the SPA mount the same way it wraps REST
// and GraphQL — so a binary fronted by /oats-uaa serves the SPA
// at /oats-uaa/, not at /.
func TestServeFrontendWithRoutePrefix(t *testing.T) {
	app := New(Config{Server: ServerConfig{RoutePrefix: "/v1/api"}})
	fsys := fstest.MapFS{
		"index.html":         {Data: []byte("<html>app</html>")},
		"assets/main.js":     {Data: []byte("x=1")},
	}
	if err := mountFrontend(app, fsys); err != nil {
		t.Fatalf("mountFrontend: %v", err)
	}

	cases := []struct{ path, body string }{
		{"/v1/api/", "<html>app</html>"},
		{"/v1/api/assets/main.js", "x=1"},
		{"/v1/api/spa-route/123", "<html>app</html>"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", tc.path, nil)
		app.engine.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: status %d body=%q", tc.path, rec.Code, rec.Body.String())
		}
		if rec.Body.String() != tc.body {
			t.Fatalf("%s: body %q want %q", tc.path, rec.Body.String(), tc.body)
		}
	}
}
