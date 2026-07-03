package security_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/paulmanoni/nexus"
)

type echoArgs struct {
	Name string `json:"name"`
}

func newEcho(p nexus.Params[echoArgs]) (string, error) { return "hi " + p.Args.Name, nil }

// TestCoreSecurityEndToEnd boots a real (listener-less) nexus app with
// the built-in security middleware configured via Config.Middleware.
// Security (the zero-code path) and proves the whole wiring: headers are
// applied by default, and enabling CSRF enforces it — a token-less POST
// is rejected while a seeded token passes.
func TestCoreSecurityEndToEnd(t *testing.T) {
	app, stop, err := nexus.InProcess(nexus.Config{
		Middleware: nexus.MiddlewareConfig{
			// CSRF is opt-in (it matters only for cookie/session auth);
			// enable it explicitly for this end-to-end check. Headers
			// are on by default with no config at all.
			Security: &nexus.SecurityConfig{EnableCSRF: true},
		},
	},
		nexus.AsRest("GET", "/thing", newEcho),
		nexus.AsRest("POST", "/thing", newEcho),
	)
	if err != nil {
		t.Fatalf("InProcess: %v", err)
	}
	defer stop(context.Background())

	// 1. GET carries the security headers and seeds a CSRF cookie.
	get := httptest.NewRecorder()
	app.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/thing", nil))
	if get.Code != 200 {
		t.Fatalf("GET status %d, want 200 (body %s)", get.Code, get.Body.String())
	}
	if got := get.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options: got %q, want DENY", got)
	}
	if got := get.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options: got %q, want nosniff", got)
	}
	var token string
	for _, c := range get.Result().Cookies() {
		if c.Name == "csrftoken" {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("GET did not seed a csrftoken cookie")
	}

	// 2. POST without the token is rejected.
	bad := httptest.NewRecorder()
	app.ServeHTTP(bad, httptest.NewRequest(http.MethodPost, "/thing", nil))
	if bad.Code != http.StatusForbidden {
		t.Fatalf("POST without token: status %d, want 403", bad.Code)
	}

	// 3. POST echoing the seeded token succeeds (any 2xx — nexus uses
	// 201 for POST by convention).
	ok := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/thing", nil)
	req.AddCookie(&http.Cookie{Name: "csrftoken", Value: token})
	req.Header.Set("X-CSRFToken", token)
	app.ServeHTTP(ok, req)
	if ok.Code < 200 || ok.Code >= 300 {
		t.Fatalf("POST with token: status %d, want 2xx (body %s)", ok.Code, ok.Body.String())
	}
}

// TestHeadersOnByDefault confirms an app with NO security config still
// gets the safe headers — secure by default.
func TestHeadersOnByDefault(t *testing.T) {
	app, stop, err := nexus.InProcess(nexus.Config{},
		nexus.AsRest("GET", "/thing", newEcho),
	)
	if err != nil {
		t.Fatalf("InProcess: %v", err)
	}
	defer stop(context.Background())

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/thing", nil))
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("headers should be on by default: X-Content-Type-Options=%q", got)
	}
	// CSRF is OFF by default: a token-less POST is not rejected here
	// (there's no POST route; the header check above is the proof).
}
