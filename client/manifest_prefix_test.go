package client

import (
	"testing"

	"github.com/paulmanoni/nexus/registry"
)

// TestBuildManifestRelativePaths pins the double-prefix fix: stored endpoint
// paths are absolute (app.PrefixPath baked in the deployment route prefix),
// so the manifest must emit them RELATIVE to BasePath — otherwise every SDK
// consumer's `origin + basePath + path` doubles the prefix.
func TestBuildManifestRelativePaths(t *testing.T) {
	const prefix = "/api"
	reg := registry.New()
	// Paths as they are actually stored — already prefixed.
	reg.RegisterEndpoint(registry.Endpoint{
		Name: "GET /api/billing/charge", Transport: registry.REST,
		Method: "GET", Path: "/api/billing/charge", Service: "billing",
	})
	reg.RegisterEndpoint(registry.Endpoint{
		Name: "searchUsers", Transport: registry.GraphQL,
		Method: "query", Path: "/api/graphql", Service: "users",
	})
	reg.RegisterEndpoint(registry.Endpoint{
		Name: "chat.send", Transport: registry.WebSocket,
		Method: "chat.send", Path: "/api/events", Service: "chat",
	})

	m := buildManifest(reg, nil, nil, prefix)

	if m.BasePath != prefix {
		t.Fatalf("BasePath = %q, want %q", m.BasePath, prefix)
	}
	for _, e := range m.Endpoints {
		if len(e.Path) >= len(prefix) && e.Path[:len(prefix)] == prefix {
			t.Errorf("endpoint %q path %q still carries the prefix — origin+basePath+path would double it", e.Name, e.Path)
		}
	}
	// Spot-check the expected relative forms.
	want := map[string]string{
		"GET /api/billing/charge": "/billing/charge",
		"searchUsers":             "/graphql",
	}
	for _, e := range m.Endpoints {
		if w, ok := want[e.Name]; ok && e.Path != w {
			t.Errorf("%s: path = %q, want %q", e.Name, e.Path, w)
		}
	}
	if len(m.WS) != 1 || m.WS[0].Path != "/events" {
		t.Fatalf("WS path = %+v, want one group at /events", m.WS)
	}
}

// TestBuildManifestNoPrefixUnchanged confirms the fix is a no-op when no
// deployment route prefix is set (the common case).
func TestBuildManifestNoPrefixUnchanged(t *testing.T) {
	reg := registry.New()
	reg.RegisterEndpoint(registry.Endpoint{
		Name: "GET /pets", Transport: registry.REST,
		Method: "GET", Path: "/pets", Service: "pets",
	})
	m := buildManifest(reg, nil, nil, "")
	if m.BasePath != "" {
		t.Fatalf("BasePath = %q, want empty", m.BasePath)
	}
	if m.Endpoints[0].Path != "/pets" {
		t.Fatalf("path = %q, want /pets unchanged", m.Endpoints[0].Path)
	}
}
