package secure

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/httpx"
	"github.com/paulmanoni/nexus/httpx/stdrouter"
)

func newHeadersEngine(t *testing.T, cfg HeadersConfig) httpx.Router {
	t.Helper()
	ApplyHeaderDefaults(&cfg)
	r := stdrouter.New()
	r.Use(HeadersHandler(&cfg))
	r.GET("/ping", func(c *httpx.Ctx) { c.JSON(200, httpx.H{"ok": true}) })
	return r
}

func newCSRFEngine(t *testing.T, cfg CSRFConfig) httpx.Router {
	t.Helper()
	ApplyCSRFDefaults(&cfg)
	if err := ValidateCSRF(&cfg); err != nil {
		t.Fatalf("ValidateCSRF: %v", err)
	}
	r := stdrouter.New()
	r.Use(CSRFHandler(&cfg))
	r.GET("/ping", func(c *httpx.Ctx) { c.JSON(200, httpx.H{"ok": true}) })
	r.POST("/ping", func(c *httpx.Ctx) { c.JSON(200, httpx.H{"ok": true}) })
	return r
}

// --- headers ----------------------------------------------------------------

func TestHeadersDefaults(t *testing.T) {
	t.Parallel()
	r := newHeadersEngine(t, HeadersConfig{})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ping", nil))

	want := map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}
	for k, v := range want {
		if got := w.Header().Get(k); got != v {
			t.Errorf("%s: got %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{"Strict-Transport-Security", "Content-Security-Policy", "Permissions-Policy", "Cross-Origin-Opener-Policy"} {
		if got := w.Header().Get(k); got != "" {
			t.Errorf("%s should be absent by default, got %q", k, got)
		}
	}
}

func TestHeadersOmitAndDisableNosniff(t *testing.T) {
	t.Parallel()
	no := false
	r := newHeadersEngine(t, HeadersConfig{
		FrameOptions:       Omit,
		ReferrerPolicy:     Omit,
		ContentTypeNosniff: &no,
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ping", nil))
	for _, k := range []string{"X-Frame-Options", "Referrer-Policy", "X-Content-Type-Options"} {
		if got := w.Header().Get(k); got != "" {
			t.Errorf("%s should be omitted, got %q", k, got)
		}
	}
}

func TestHeadersOptIn(t *testing.T) {
	t.Parallel()
	r := newHeadersEngine(t, HeadersConfig{
		HSTS:                    &HSTSConfig{MaxAge: 100, IncludeSubdomains: true, Preload: true},
		ContentSecurityPolicy:   "default-src 'self'",
		PermissionsPolicy:       "geolocation=()",
		CrossOriginOpenerPolicy: "same-origin",
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ping", nil))

	if got, want := w.Header().Get("Strict-Transport-Security"), "max-age=100; includeSubDomains; preload"; got != want {
		t.Errorf("HSTS: got %q, want %q", got, want)
	}
	if got := w.Header().Get("Content-Security-Policy"); got != "default-src 'self'" {
		t.Errorf("CSP: got %q", got)
	}
	if got := w.Header().Get("Permissions-Policy"); got != "geolocation=()" {
		t.Errorf("Permissions-Policy: got %q", got)
	}
	if got := w.Header().Get("Cross-Origin-Opener-Policy"); got != "same-origin" {
		t.Errorf("COOP: got %q", got)
	}
}

func TestBuildHSTSDefaultMaxAge(t *testing.T) {
	t.Parallel()
	if got := BuildHSTS(&HSTSConfig{}); got != "max-age=31536000" {
		t.Errorf("got %q, want default max-age", got)
	}
}

// --- CSRF --------------------------------------------------------------------

func csrfCookie(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestCSRFSafeMethodSeedsCookie(t *testing.T) {
	t.Parallel()
	r := newCSRFEngine(t, CSRFConfig{})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if w.Code != 200 {
		t.Fatalf("GET status: got %d, want 200", w.Code)
	}
	ck := csrfCookie(t, w, DefaultCSRFCookie)
	if ck == nil || ck.Value == "" {
		t.Fatal("expected a csrftoken cookie to be seeded on GET")
	}
	if ck.HttpOnly {
		t.Error("CSRF cookie must be readable by JS (HttpOnly=false)")
	}
}

func TestCSRFRejectsUnsafeWithoutToken(t *testing.T) {
	t.Parallel()
	r := newCSRFEngine(t, CSRFConfig{})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/ping", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST without token: got %d, want 403", w.Code)
	}
}

func TestCSRFAcceptsMatchingToken(t *testing.T) {
	t.Parallel()
	r := newCSRFEngine(t, CSRFConfig{})
	req := httptest.NewRequest(http.MethodPost, "/ping", nil)
	req.AddCookie(&http.Cookie{Name: DefaultCSRFCookie, Value: "tok-123"})
	req.Header.Set(DefaultCSRFHeader, "tok-123")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("matching token: got %d, want 200", w.Code)
	}
}

func TestCSRFRejectsMismatch(t *testing.T) {
	t.Parallel()
	r := newCSRFEngine(t, CSRFConfig{})
	req := httptest.NewRequest(http.MethodPost, "/ping", nil)
	req.AddCookie(&http.Cookie{Name: DefaultCSRFCookie, Value: "cookie-tok"})
	req.Header.Set(DefaultCSRFHeader, "header-tok")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("mismatched token: got %d, want 403", w.Code)
	}
}

func TestCSRFFormFieldFallback(t *testing.T) {
	t.Parallel()
	r := newCSRFEngine(t, CSRFConfig{})
	body := strings.NewReader("csrf_token=form-tok&name=x")
	req := httptest.NewRequest(http.MethodPost, "/ping", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: DefaultCSRFCookie, Value: "form-tok"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("form-field token: got %d, want 200", w.Code)
	}
}

func TestCSRFDefaultSkipsBearerAuth(t *testing.T) {
	t.Parallel()
	r := newCSRFEngine(t, CSRFConfig{})
	req := httptest.NewRequest(http.MethodPost, "/ping", nil)
	req.Header.Set("Authorization", "Bearer abc")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("bearer-auth POST should skip CSRF: got %d, want 200", w.Code)
	}
}

func TestCSRFCustomSkip(t *testing.T) {
	t.Parallel()
	r := newCSRFEngine(t, CSRFConfig{
		Skip: func(c *httpx.Ctx) bool { return c.GetHeader("X-Internal") == "1" },
	})
	req := httptest.NewRequest(http.MethodPost, "/ping", nil)
	req.Header.Set("X-Internal", "1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("custom skip should bypass: got %d, want 200", w.Code)
	}
}

func TestCSRFCheckOrigin(t *testing.T) {
	t.Parallel()
	r := newCSRFEngine(t, CSRFConfig{CheckOrigin: true, TrustedOrigins: []string{"trusted.example.com"}})
	mk := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/ping", nil)
		req.Host = "app.example.com"
		req.AddCookie(&http.Cookie{Name: DefaultCSRFCookie, Value: "t"})
		req.Header.Set(DefaultCSRFHeader, "t")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	if w := mk("https://evil.example.com"); w.Code != http.StatusForbidden {
		t.Errorf("cross-origin: got %d, want 403", w.Code)
	}
	if w := mk("https://app.example.com"); w.Code != 200 {
		t.Errorf("same-origin: got %d, want 200", w.Code)
	}
	if w := mk("https://trusted.example.com"); w.Code != 200 {
		t.Errorf("trusted origin: got %d, want 200", w.Code)
	}
	if w := mk(""); w.Code != 200 {
		t.Errorf("absent origin should fall through to token: got %d, want 200", w.Code)
	}
}

func TestCSRFCookieSecureAuto(t *testing.T) {
	t.Parallel()
	r := newCSRFEngine(t, CSRFConfig{})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if ck := csrfCookie(t, w, DefaultCSRFCookie); ck == nil || ck.Secure {
		t.Errorf("http request: Secure should be false, got %+v", ck)
	}
	w = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	r.ServeHTTP(w, req)
	if ck := csrfCookie(t, w, DefaultCSRFCookie); ck == nil || !ck.Secure {
		t.Errorf("forwarded https: Secure should be true, got %+v", ck)
	}
}

func TestValidateCSRFTokenTooSmall(t *testing.T) {
	t.Parallel()
	cfg := CSRFConfig{TokenBytes: 8}
	if err := ValidateCSRF(&cfg); err == nil || !strings.Contains(err.Error(), "too small") {
		t.Fatalf("want 'too small' error, got %v", err)
	}
}

func TestGenerateTokenUniqueAndSized(t *testing.T) {
	t.Parallel()
	a, b := GenerateToken(32), GenerateToken(32)
	if a == b {
		t.Fatal("two tokens collided — not random")
	}
	if len(a) < 32 {
		t.Fatalf("token too short: %d chars", len(a))
	}
}
