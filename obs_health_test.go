package nexus

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
)

// TestHealth_AliveFlagsToggle verifies /__nexus/health returns 200 once
// fx Start completes and reverts to 503 after Stop. This is the basic
// liveness contract orchestrators rely on.
func TestHealth_AliveFlagsToggle(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
		fx.Populate(&app),
	)
	fxApp.RequireStart()

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/__nexus/health", nil))
	if w.Code != http.StatusOK {
		t.Errorf("alive after Start: want 200, got %d", w.Code)
	}

	fxApp.RequireStop()
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/__nexus/health", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("alive after Stop: want 503, got %d", w.Code)
	}
}

// TestReady_MonolithReadyImmediately verifies a deployment with no peers
// becomes ready as soon as it's alive — the monolith case.
func TestReady_MonolithReadyImmediately(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/__nexus/ready", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("monolith ready: want 200, got %d", w.Code)
	}
}

// waitFor polls cond every 50ms until it returns true or timeout
// expires. Returns whether cond became true within the budget.
func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}
