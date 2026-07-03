package security

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/paulmanoni/nexus/httpx"
	"github.com/paulmanoni/nexus/httpx/stdrouter"
)

// TestNewCSRFMiddlewareValidationError confirms a bad per-route config
// yields an always-500 guard rather than a silent pass-through. The
// header/CSRF behaviour itself is covered in middleware/secure.
func TestNewCSRFMiddlewareValidationError(t *testing.T) {
	t.Parallel()
	mw := NewCSRFMiddleware(CSRFConfig{TokenBytes: 4})
	r := stdrouter.New()
	r.Use(mw.Gin)
	r.POST("/x", func(c *httpx.Ctx) { c.JSON(200, httpx.H{"ok": true}) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/x", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("misconfigured middleware: got %d, want 500", w.Code)
	}
}

// TestNewHeadersMiddlewareSetsDefaults confirms the per-route bundle
// applies the safe defaults.
func TestNewHeadersMiddlewareSetsDefaults(t *testing.T) {
	t.Parallel()
	mw := NewHeadersMiddleware(HeadersConfig{})
	r := stdrouter.New()
	r.Use(mw.Gin)
	r.GET("/x", func(c *httpx.Ctx) { c.JSON(200, httpx.H{"ok": true}) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options: got %q, want DENY", got)
	}
}
