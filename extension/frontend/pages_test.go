package frontend

import (
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/extension"
	"github.com/paulmanoni/nexus/registry"
)

// TestRender_SkipsInertiaPages: a route tagged registry.PageTag renders a
// page, so index.ts must not export a REST caller for it — while an ordinary
// REST route on the same service still gets one.
func TestRender_SkipsInertiaPages(t *testing.T) {
	reg := registry.New()
	reg.RegisterEndpoint(registry.Endpoint{
		Service: "users", Name: "listUsers", Transport: registry.REST, Method: "GET", Path: "/api/users",
	})
	reg.RegisterEndpoint(registry.Endpoint{
		Service: "users", Name: "usersPage", Transport: registry.REST, Method: "GET", Path: "/users",
		Tags: map[string]string{registry.PageTag: "Users/Index"},
	})
	files, err := Render(Config{Framework: None}, extension.GenerateContext{Registry: reg})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	idx := findFile(t, files, "index.ts")
	if !strings.Contains(idx, "'/api/users'") {
		t.Errorf("ordinary REST route missing from index.ts:\n%s", idx)
	}
	if strings.Contains(idx, "'/users'") {
		t.Errorf("Inertia page emitted as a REST call in index.ts:\n%s", idx)
	}
}
