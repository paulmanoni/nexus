package nexus

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// newCORSEngine wires the CORS middleware onto a bare engine with one
// stub handler. Tests inspect the response headers + status to verify
// the middleware behavior end-to-end (handler reach + preflight
// short-circuit are both observable from outside).
func newCORSEngine(cfg CORSConfig) *gin.Engine {
	e := gin.New()
	e.Use(corsMiddleware(cfg))
	e.Any("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	return e
}

// TestCORS_NoOriginPassesThrough verifies that requests without an
// Origin header skip CORS entirely — same-origin browsers and curl
// scripts shouldn't be affected.
func TestCORS_NoOriginPassesThrough(t *testing.T) {
	e := newCORSEngine(CORSConfig{AllowOrigins: []string{"https://app.example"}})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	e.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("ACAO without Origin: want empty, got %q", got)
	}
}

// TestCORS_WildcardAllowsAnyOrigin echoes the request's Origin when
// AllowOrigins is "*". Wildcard with credentials is a CORS-spec
// pitfall — the matcher echoes Origin so it stays compatible.
func TestCORS_WildcardAllowsAnyOrigin(t *testing.T) {
	e := newCORSEngine(CORSConfig{}) // empty AllowOrigins → "*"
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Origin", "https://random.example")
	e.ServeHTTP(w, req)
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://random.example" {
		t.Errorf("ACAO: want echoed origin, got %q", got)
	}
}

// TestCORS_AllowlistMatchesExact accepts listed origins and (silently)
// passes others through without setting the ACAO header — the browser
// blocks the response, the framework doesn't 403.
func TestCORS_AllowlistMatchesExact(t *testing.T) {
	e := newCORSEngine(CORSConfig{AllowOrigins: []string{"https://app.example"}})

	for _, c := range []struct {
		origin     string
		wantHeader string
	}{
		{"https://app.example", "https://app.example"},
		{"https://evil.example", ""},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/x", nil)
		req.Header.Set("Origin", c.origin)
		e.ServeHTTP(w, req)
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != c.wantHeader {
			t.Errorf("origin %q: want ACAO %q, got %q", c.origin, c.wantHeader, got)
		}
	}
}

// TestCORS_PreflightShortCircuits confirms an OPTIONS preflight
// returns 204 with the full preflight header set, without ever
// reaching the actual handler. The test handler returns "ok" — we
// verify the body is empty (handler skipped).
func TestCORS_PreflightShortCircuits(t *testing.T) {
	e := newCORSEngine(CORSConfig{
		AllowOrigins:     []string{"*"},
		AllowCredentials: true,
		MaxAge:           5 * time.Minute,
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("OPTIONS", "/x", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	e.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight status: want 204, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Error("preflight should set Access-Control-Allow-Methods")
	}
	if got := w.Header().Get("Access-Control-Max-Age"); got != "300" {
		t.Errorf("Access-Control-Max-Age: want 300 (5m), got %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("ACAC: want true, got %q", got)
	}
	if w.Body.String() != "" {
		t.Errorf("preflight should not reach handler; body=%q", w.Body.String())
	}
}
