package gql

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/registry"
)

// TestStatsRegistry_DashboardEndpoint runs traffic through a mounted
// schema with a cache + stats registry, then asserts that
// GET /__nexus/graphql/cache reflects the live hit/miss counters.
// This is the contract the dashboard relies on.
func TestStatsRegistry_DashboardEndpoint(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)

	schema := buildEchoSchema(t)
	reg := NewStatsRegistry()

	e := gin.New()
	Mount(e, registry.New(), nil, "test", "/graphql", &schema,
		WithDocumentCache(8),
		WithStatsRegistry(reg),
	)
	// Mount the dashboard endpoint under a router group mirroring
	// dashboard.Mount's wiring.
	MountDashboard(e.Group("/__nexus"), reg)

	body := []byte(`{"query":"{ echo(q:\"hi\") }"}`)
	// First request → miss, second → hit.
	doPost(t, e, "/graphql", body)
	doPost(t, e, "/graphql", body)

	// Pull stats over the wire.
	req, _ := http.NewRequest("GET", "/__nexus/graphql/cache", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var env struct {
		Mounts []MountCacheStats `json:"mounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if len(env.Mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d: %s", len(env.Mounts), rec.Body.String())
	}
	m := env.Mounts[0]
	if m.Path != "/graphql" {
		t.Errorf("path = %q, want /graphql", m.Path)
	}
	if m.Hits != 1 {
		t.Errorf("hits = %d, want 1", m.Hits)
	}
	if m.Misses != 1 {
		t.Errorf("misses = %d, want 1", m.Misses)
	}
	if m.Size != 1 {
		t.Errorf("size = %d, want 1", m.Size)
	}
	if m.Capacity != 8 {
		t.Errorf("capacity = %d, want 8", m.Capacity)
	}
}

// TestStatsRegistry_EmptyWhenNoMounts confirms the endpoint surfaces a
// well-formed empty list when the registry hasn't seen any caches —
// the dashboard polls unconditionally so this case has to be tidy.
func TestStatsRegistry_EmptyWhenNoMounts(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	reg := NewStatsRegistry()
	e := gin.New()
	MountDashboard(e.Group("/__nexus"), reg)

	req, _ := http.NewRequest("GET", "/__nexus/graphql/cache", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"mounts":[]`)) {
		t.Errorf("expected empty mounts list, got %s", rec.Body.String())
	}
}
