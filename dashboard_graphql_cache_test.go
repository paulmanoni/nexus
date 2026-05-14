package nexus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDashboard_GraphQLCacheEndpoint exercises the full wiring path:
// nexus.newApp builds an App with autoMountGraphQL enrolling a
// DocumentCache in app.gqlStats, dashboard.Mount surfaces it via
// GET /__nexus/graphql/cache. After two identical queries the
// endpoint should report one hit and one miss for the /graphql
// mount. This guards the dashboard contract from regressions in
// the autoMount → stats-registry plumbing.
func TestDashboard_GraphQLCacheEndpoint(t *testing.T) {
	mod := Module("dashbench",
		AsQuery(func(a struct {
			Q string `graphql:"q"`
		}) (*string, error) {
			out := "hi " + a.Q
			return &out, nil
		}),
	)
	// Introspection: true lifts the dashboard group's gate so
	// /__nexus/* is reachable from any client. Without it the
	// gate 404s every dashboard route by default.
	app, err := newApp(Config{
		Server:        ServerConfig{Addr: "127.0.0.1:0"},
		Dashboard:     DashboardConfig{Enabled: true, Name: "T"},
		Introspection: true,
	}, mod)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Stop()

	srv := httptest.NewServer(app.Engine())
	defer srv.Close()

	// Drive two identical queries to populate hit/miss counters.
	body := strings.NewReader(`{"query":"{ noSvcArgsLike: __typename }"}`)
	resp, err := http.Post(srv.URL+"/graphql", "application/json", body)
	if err != nil {
		t.Fatalf("first POST: %v", err)
	}
	resp.Body.Close()
	body2 := strings.NewReader(`{"query":"{ noSvcArgsLike: __typename }"}`)
	resp, err = http.Post(srv.URL+"/graphql", "application/json", body2)
	if err != nil {
		t.Fatalf("second POST: %v", err)
	}
	resp.Body.Close()

	resp, err = http.Get(srv.URL + "/__nexus/graphql/cache")
	if err != nil {
		t.Fatalf("GET cache: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var env struct {
		Mounts []struct {
			Path     string `json:"path"`
			Hits     uint64 `json:"Hits"`
			Misses   uint64 `json:"Misses"`
			Size     int    `json:"Size"`
			Capacity int    `json:"Capacity"`
		} `json:"mounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d: %+v", len(env.Mounts), env)
	}
	m := env.Mounts[0]
	if m.Path != "/graphql" {
		t.Errorf("path = %q", m.Path)
	}
	if m.Hits != 1 || m.Misses != 1 {
		t.Errorf("hits=%d misses=%d, want 1/1", m.Hits, m.Misses)
	}
	if m.Capacity != 1024 {
		t.Errorf("capacity = %d, want 1024 (default)", m.Capacity)
	}
}
