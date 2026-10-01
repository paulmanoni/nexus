package client

import (
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/registry"
)

// Islands recorded in the registry become Manifest.Islands and the
// NexusIslandProps interface — and nothing when there are none.
func TestIslandTypes(t *testing.T) {
	reg := registry.New()
	reg.SetIsland("Chart", registry.TypeRef{Kind: "ref", Ref: "ChartProps"})
	reg.SetIsland("admin/Editor", registry.TypeRef{Kind: "any"})
	m := buildManifest(reg, nil, nil, "")
	if c := m.Islands["Chart"]; c == nil || c.Ref != "ChartProps" {
		t.Fatalf("Islands = %v", m.Islands)
	}
	out := GenerateClientDTS(m)
	for _, want := range []string{
		"export interface NexusIslandProps {\n",
		"  'Chart': ChartProps\n",
		"  'admin/Editor': { [key: string]: unknown }\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("client.d.ts missing %q", want)
		}
	}
	plain := buildManifest(registry.New(), nil, nil, "")
	if plain.Islands != nil || strings.Contains(GenerateClientDTS(plain), "NexusIslandProps") {
		t.Error("no islands must emit no NexusIslandProps")
	}
}
