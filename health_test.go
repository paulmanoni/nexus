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
		fxBootOptions(Config{Addr: "127.0.0.1:0"}),
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
		fxBootOptions(Config{Addr: "127.0.0.1:0"}),
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

// TestReady_PeerDownProducesNotReady verifies that /__nexus/ready returns
// 503 while a declared peer is unreachable, and flips to 200 once the
// peer answers. This is the load-bearing readiness contract for split
// deployments — keeps a pod out of the LB rotation until its hard
// dependencies are up.
func TestReady_PeerDownProducesNotReady(t *testing.T) {
	// Mock peer that only flips to ready after the test releases it.
	peerReady := make(chan struct{})
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__nexus/health" {
			http.NotFound(w, r)
			return
		}
		select {
		case <-peerReady:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer peer.Close()

	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{
			Addr:       "127.0.0.1:0",
			Deployment: "checkout-svc",
			Topology: Topology{Peers: map[string]Peer{
				"checkout-svc": {},
				"users-svc":    {URLs: []string{peer.URL}},
			}},
		}),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	// First probe runs synchronously inside run() so by the time the
	// goroutine has scheduled the tracker's ready state should reflect
	// the peer's down status. Allow up to 1s for goroutine scheduling.
	notReady := waitFor(1*time.Second, func() bool {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest("GET", "/__nexus/ready", nil))
		return w.Code == http.StatusServiceUnavailable
	})
	if !notReady {
		t.Fatal("ready should be 503 while peer is down")
	}

	// Flip the peer to ready and wait for the next probe interval to
	// pick it up. Probe interval is 5s; bump the wait slightly so a
	// slow CI box doesn't flake.
	close(peerReady)
	ready := waitFor(7*time.Second, func() bool {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest("GET", "/__nexus/ready", nil))
		return w.Code == http.StatusOK
	})
	if !ready {
		t.Fatal("ready should be 200 after peer becomes reachable")
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
