package nexus

import (
	"context"
	"net/http/httptest"
	"testing"
)

// TestInProcess_NoListenerBindsNoSocket proves the quiet-boot seam: InProcess
// sets Config.Server.NoListener, so two apps configured for the SAME fixed
// address both boot without an "address already in use" conflict. If either
// app bound a real socket, the second Start would fail.
func TestInProcess_NoListenerBindsNoSocket(t *testing.T) {
	const addr = "127.0.0.1:8080" // a fixed, non-ephemeral port on purpose

	a1, stop1, err := InProcess(Config{Server: ServerConfig{Addr: addr}})
	if err != nil {
		t.Fatalf("first InProcess: %v", err)
	}
	defer stop1(context.Background())

	a2, stop2, err := InProcess(Config{Server: ServerConfig{Addr: addr}})
	if err != nil {
		t.Fatalf("second InProcess on the same addr: %v (a real bind would conflict)", err)
	}
	defer stop2(context.Background())

	if a1 == nil || a2 == nil {
		t.Fatal("InProcess returned a nil app")
	}
}

// TestInProcess_ServeHTTPStillRoutes confirms quiet-boot doesn't cost routing:
// the app is a fully wired http.Handler even with no listener.
func TestInProcess_ServeHTTPStillRoutes(t *testing.T) {
	type pingArgs struct{}
	newPing := func(p Params[pingArgs]) (string, error) { return "pong", nil }

	app, stop, err := InProcess(Config{},
		AsRest("GET", "/ping", newPing),
	)
	if err != nil {
		t.Fatalf("InProcess: %v", err)
	}
	defer stop(context.Background())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ping", nil)
	app.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("GET /ping: status %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}
