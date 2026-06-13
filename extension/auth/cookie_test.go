package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestSessionCookie_SetClearExtract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sc := SessionCookie{Name: "access_token", MaxAge: time.Hour}

	// Set writes an HttpOnly cookie with the token and a positive Max-Age.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	sc.Set(c, "tok-123")
	set := w.Header().Get("Set-Cookie")
	for _, want := range []string{"access_token=tok-123", "HttpOnly", "Max-Age=3600", "Path=/"} {
		if !strings.Contains(set, want) {
			t.Fatalf("Set-Cookie %q missing %q", set, want)
		}
	}

	// The paired extractor reads exactly what Set wrote.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "access_token", Value: "tok-123"})
	if v, ok := sc.Extractor().Extract(req); !ok || v != "tok-123" {
		t.Fatalf("extractor got (%q, %v), want (tok-123, true)", v, ok)
	}

	// Clear expires it.
	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	sc.Clear(c2)
	if got := w2.Header().Get("Set-Cookie"); !strings.Contains(got, "access_token=") || !strings.Contains(got, "Max-Age=0") {
		t.Fatalf("Clear should expire the cookie, got %q", got)
	}
}

func TestSessionCookie_Defaults(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var sc SessionCookie // zero value
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	sc.Set(c, "x")
	set := w.Header().Get("Set-Cookie")
	if !strings.Contains(set, DefaultSessionCookieName+"=x") {
		t.Fatalf("default name not applied: %q", set)
	}
	// Default MaxAge is 7 days.
	if !strings.Contains(set, "Max-Age=604800") {
		t.Fatalf("default max-age not applied: %q", set)
	}
}
