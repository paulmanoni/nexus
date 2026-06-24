package client

import (
	"testing"

	"github.com/paulmanoni/nexus/internal/golden"
	"github.com/paulmanoni/nexus/registry"
)

// goldenManifest is a representative manifest exercising every renderer branch:
// named refs (with optional + documented fields), REST endpoints (inline-object
// args, ref args/return, array return), GraphQL query+mutation, and a WS path
// with a typed message + a no-arg message.
func goldenManifest() Manifest {
	str := func(p string) *registry.TypeRef { return &registry.TypeRef{Kind: "primitive", Primitive: p} }
	intRef := func() *registry.TypeRef { return &registry.TypeRef{Kind: "primitive", Primitive: "integer"} }
	arrayOf := func(inner *registry.TypeRef) *registry.TypeRef {
		return &registry.TypeRef{Kind: "array", Of: inner}
	}
	ref := func(name string) *registry.TypeRef { return &registry.TypeRef{Kind: "ref", Ref: name} }

	return Manifest{
		Version:  SchemaVersion,
		BasePath: "/api/v1",
		Services: []ServiceInfo{{Name: "pets"}, {Name: "events"}},
		Refs: map[string]registry.NamedType{
			"Pet": {
				Description: "A pet record",
				Fields: []registry.FieldSchema{
					{Name: "ID", JSONName: "id", Type: *str("string")},
					{Name: "Name", JSONName: "name", Type: *str("string"), Description: "Display name"},
					{Name: "Age", JSONName: "age", Type: *intRef(), Optional: true},
					{Name: "Tags", JSONName: "tags", Type: *arrayOf(str("string"))},
				},
			},
			"Owner": {
				Fields: []registry.FieldSchema{
					{Name: "ID", JSONName: "id", Type: *str("string")},
					{Name: "Pets", JSONName: "pets", Type: *arrayOf(ref("Pet"))},
				},
			},
		},
		Endpoints: []EndpointInfo{
			{
				Service: "pets", Transport: "rest", Method: "GET", Path: "/pets",
				Args: &registry.TypeRef{Kind: "object", Object: &registry.NamedType{
					Fields: []registry.FieldSchema{
						{Name: "Limit", JSONName: "limit", Type: *intRef(), Optional: true},
					},
				}},
				Return: arrayOf(ref("Pet")),
			},
			{
				Service: "pets", Transport: "rest", Method: "POST", Path: "/pets",
				Args: ref("Pet"), Return: ref("Pet"),
			},
			{
				Service: "pets", Transport: "graphql", Method: "query", Name: "listPets",
				Path: "/graphql", Return: arrayOf(ref("Pet")),
			},
			{
				Service: "pets", Transport: "graphql", Method: "mutation", Name: "createPet",
				Path: "/graphql", Args: ref("Pet"), Return: ref("Pet"),
			},
		},
		WS: []WSPathInfo{
			{
				Path: "/events", Service: "events",
				Messages: []WSMessage{
					{Type: "chat.send", Args: &registry.TypeRef{Kind: "object", Object: &registry.NamedType{
						Fields: []registry.FieldSchema{
							{Name: "Text", JSONName: "text", Type: *str("string")},
						},
					}}},
					{Type: "chat.typing"},
				},
			},
		},
	}
}

// TestGenerateDTS_Golden snapshots the FULL .d.ts output for all three flavors.
// Substring tests live in generator_test.go; these pin the entire byte stream so
// any unintended drift (spacing, ordering, a renamed export) shows up as a diff.
// Regenerate intentionally with: UPDATE_GOLDEN=1 go test ./client/...
func TestGenerateDTS_Golden(t *testing.T) {
	m := goldenManifest()
	golden.AssertString(t, GenerateClientDTS(m), "client.d.ts")
	golden.AssertString(t, GenerateVueDTS(m), "vue.d.ts")
	golden.AssertString(t, GenerateReactDTS(m), "react.d.ts")
}
