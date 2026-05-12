package openapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/client"
	"github.com/paulmanoni/nexus/registry"
)

// TestBuilder_RESTPathParamsBecomeOpenAPIParameters locks in the most
// visible bit of the spec generation: gin's ":id" syntax becomes
// OpenAPI's "{id}", and a Parameter object lands in the operation
// with in=path, required=true.
func TestBuilder_RESTPathParamsBecomeOpenAPIParameters(t *testing.T) {
	t.Parallel()
	s := &pluginState{cfg: Config{Title: "T", Version: "1"}}
	applyDefaults(&s.cfg)

	mf := client.Manifest{
		Endpoints: []client.EndpointInfo{
			{
				Service:   "pets",
				Transport: "rest",
				Method:    "GET",
				Path:      "/pets/:id",
				Name:      "getPet",
			},
		},
	}
	doc := s.buildDocument(mf)

	pi, ok := doc.Paths["/pets/{id}"]
	if !ok {
		t.Fatalf("paths missing /pets/{id}: %+v", doc.Paths)
	}
	if pi.Get == nil {
		t.Fatal("Get operation missing")
	}
	if len(pi.Get.Parameters) != 1 {
		t.Fatalf("want 1 parameter, got %d: %+v", len(pi.Get.Parameters), pi.Get.Parameters)
	}
	p := pi.Get.Parameters[0]
	if p.Name != "id" || p.In != "path" || !p.Required {
		t.Errorf("bad path param: %+v", p)
	}
}

// TestBuilder_RESTPostUsesRequestBody covers the typed-body path:
// POST handlers with an Args type land a requestBody with
// application/json + the args schema.
func TestBuilder_RESTPostUsesRequestBody(t *testing.T) {
	t.Parallel()
	s := &pluginState{cfg: Config{}}
	applyDefaults(&s.cfg)

	mf := client.Manifest{
		Endpoints: []client.EndpointInfo{
			{
				Service:   "pets",
				Transport: "rest",
				Method:    "POST",
				Path:      "/pets",
				Name:      "createPet",
				Args:      &registry.TypeRef{Kind: "ref", Ref: "PetInput"},
				Return:    &registry.TypeRef{Kind: "ref", Ref: "Pet"},
			},
		},
		Refs: map[string]registry.NamedType{
			"PetInput": {Fields: []registry.FieldSchema{
				{Name: "Name", JSONName: "name", Type: registry.TypeRef{Kind: "primitive", Primitive: "string"}},
			}},
			"Pet": {Fields: []registry.FieldSchema{
				{Name: "ID", JSONName: "id", Type: registry.TypeRef{Kind: "primitive", Primitive: "string"}},
				{Name: "Name", JSONName: "name", Type: registry.TypeRef{Kind: "primitive", Primitive: "string"}},
			}},
		},
	}
	doc := s.buildDocument(mf)

	op := doc.Paths["/pets"].Post
	if op == nil {
		t.Fatal("missing POST /pets operation")
	}
	if op.RequestBody == nil {
		t.Fatal("missing requestBody on POST")
	}
	mt := op.RequestBody.Content["application/json"]
	if mt.Schema == nil || mt.Schema.Ref != "#/components/schemas/PetInput" {
		t.Errorf("requestBody schema ref: got %+v, want $ref PetInput", mt.Schema)
	}

	// 201 response (POST), referencing Pet
	r, ok := op.Responses["201"]
	if !ok {
		t.Fatalf("missing 201 response: %+v", op.Responses)
	}
	if rs := r.Content["application/json"].Schema; rs == nil || rs.Ref != "#/components/schemas/Pet" {
		t.Errorf("201 schema ref: got %+v, want $ref Pet", rs)
	}

	// components.schemas has both Pet and PetInput
	if doc.Components == nil {
		t.Fatal("missing components")
	}
	if _, ok := doc.Components.Schemas["Pet"]; !ok {
		t.Errorf("components.schemas missing Pet")
	}
	if _, ok := doc.Components.Schemas["PetInput"]; !ok {
		t.Errorf("components.schemas missing PetInput")
	}
}

// TestBuilder_GETArgsBecomeQueryParameters exercises the field
// promotion from an Args struct into individual query parameters.
// Only scalar fields are promoted; nested objects are skipped on
// purpose (passing JSON in query strings is bad form).
func TestBuilder_GETArgsBecomeQueryParameters(t *testing.T) {
	t.Parallel()
	s := &pluginState{cfg: Config{}}
	applyDefaults(&s.cfg)

	mf := client.Manifest{
		Endpoints: []client.EndpointInfo{
			{
				Service:   "pets",
				Transport: "rest",
				Method:    "GET",
				Path:      "/pets",
				Name:      "listPets",
				Args: &registry.TypeRef{Kind: "object", Object: &registry.NamedType{
					Fields: []registry.FieldSchema{
						{Name: "Limit", JSONName: "limit", Type: registry.TypeRef{Kind: "primitive", Primitive: "integer"}, Optional: true},
						{Name: "Tag", JSONName: "tag", Type: registry.TypeRef{Kind: "primitive", Primitive: "string"}, Optional: true},
						// Nested object — should be skipped from query params.
						{Name: "Filter", JSONName: "filter", Type: registry.TypeRef{Kind: "ref", Ref: "PetFilter"}, Optional: true},
					},
				}},
			},
		},
	}
	doc := s.buildDocument(mf)
	op := doc.Paths["/pets"].Get
	if op == nil {
		t.Fatal("missing GET /pets")
	}

	names := map[string]Parameter{}
	for _, p := range op.Parameters {
		names[p.Name] = p
	}
	if _, ok := names["limit"]; !ok {
		t.Errorf("missing limit query param")
	}
	if _, ok := names["tag"]; !ok {
		t.Errorf("missing tag query param")
	}
	if _, ok := names["filter"]; ok {
		t.Errorf("filter (nested object) should NOT be promoted to query param")
	}
	for _, p := range op.Parameters {
		if p.In != "query" {
			continue
		}
		// Optional fields → required=false
		if p.Required {
			t.Errorf("query param %q marked required; want optional", p.Name)
		}
	}
}

// TestBuilder_AdminPathsExcludedByDefault is the core "customers
// don't see admin" guarantee. The dashboard and other built-in
// /__nexus/ surfaces stay out of the customer-facing spec.
func TestBuilder_AdminPathsExcludedByDefault(t *testing.T) {
	t.Parallel()
	s := &pluginState{cfg: Config{}}
	applyDefaults(&s.cfg)
	mf := client.Manifest{
		Endpoints: []client.EndpointInfo{
			{Transport: "rest", Method: "GET", Path: "/__nexus/health"},
			{Transport: "rest", Method: "GET", Path: "/pets"},
		},
	}
	doc := s.buildDocument(mf)
	if _, hit := doc.Paths["/__nexus/health"]; hit {
		t.Errorf("admin path should be excluded by default")
	}
	if _, hit := doc.Paths["/pets"]; !hit {
		t.Errorf("non-admin path missing from spec")
	}
}

// TestBuilder_GraphQLExcludedByDefault verifies the opposite policy
// for GraphQL — present in the registry, absent from the spec
// unless the operator opts in.
func TestBuilder_GraphQLExcludedByDefault(t *testing.T) {
	t.Parallel()
	s := &pluginState{cfg: Config{}}
	applyDefaults(&s.cfg)
	mf := client.Manifest{
		Endpoints: []client.EndpointInfo{
			{Transport: "graphql", Method: "query", Path: "/graphql", Name: "listPets"},
		},
	}
	doc := s.buildDocument(mf)
	if len(doc.Paths) > 0 {
		t.Errorf("graphql ops should be excluded by default; got paths=%v", keys(doc.Paths))
	}

	// Now opt in.
	yes := true
	s.cfg.IncludeGraphQL = &yes
	doc2 := s.buildDocument(mf)
	if _, ok := doc2.Paths["/graphql"]; !ok {
		t.Errorf("graphql op missing after opt-in: paths=%v", keys(doc2.Paths))
	}
}

// TestBuilder_AuthRequiredAttachesBearerSecurity verifies the auth
// signal makes it from the registry to OpenAPI's security[] in the
// expected scheme name. Permission requirements show up in the
// operation description (OpenAPI's vocabulary doesn't cover RBAC
// natively).
func TestBuilder_AuthRequiredAttachesBearerSecurity(t *testing.T) {
	t.Parallel()
	s := &pluginState{cfg: Config{}}
	applyDefaults(&s.cfg)
	mf := client.Manifest{
		Endpoints: []client.EndpointInfo{
			{
				Service:      "orders",
				Transport:    "rest",
				Method:       "POST",
				Path:         "/orders",
				AuthRequired: true,
				RequiresPerm: []string{"orders:create"},
			},
		},
		Auth: &client.AuthInfo{
			ExtractorInfo: client.ExtractorInfo{Strategy: "bearer", HeaderName: "Authorization"},
		},
	}
	doc := s.buildDocument(mf)

	op := doc.Paths["/orders"].Post
	if op == nil {
		t.Fatal("missing POST /orders")
	}
	if len(op.Security) != 1 {
		t.Fatalf("want 1 security requirement, got %d: %+v", len(op.Security), op.Security)
	}
	if _, ok := op.Security[0]["bearerAuth"]; !ok {
		t.Errorf("security ref name: want bearerAuth, got %+v", op.Security[0])
	}
	if !strings.Contains(op.Description, "orders:create") {
		t.Errorf("permission requirement not in description: %q", op.Description)
	}

	// Global components.securitySchemes
	if doc.Components == nil || doc.Components.SecuritySchemes == nil {
		t.Fatal("components.securitySchemes missing")
	}
	if doc.Components.SecuritySchemes["bearerAuth"].Type != "http" {
		t.Errorf("bearerAuth scheme type: want http, got %q", doc.Components.SecuritySchemes["bearerAuth"].Type)
	}
}

// TestOperationID_DeterministicShape pins the operationId
// generation so SDK consumers see stable method names across
// regenerations.
func TestOperationID_DeterministicShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ep   client.EndpointInfo
		want string
	}{
		{
			ep:   client.EndpointInfo{Service: "pets", Transport: "rest", Method: "GET", Path: "/pets/:id"},
			want: "pets_get_pets_byId",
		},
		{
			ep:   client.EndpointInfo{Service: "orders", Transport: "rest", Method: "POST", Path: "/orders"},
			want: "orders_post_orders",
		},
		{
			ep:   client.EndpointInfo{Transport: "graphql", Method: "query", Name: "listAdverts"},
			want: "listAdverts",
		},
	}
	for _, tc := range cases {
		if got := operationID(tc.ep); got != tc.want {
			t.Errorf("operationID(%+v) = %q, want %q", tc.ep, got, tc.want)
		}
	}
}

// TestDocument_JSONRoundTrip ensures the emitted document encodes as
// valid JSON and decodes back. Cheap insurance against introducing
// non-JSON-serializable types in the document model.
func TestDocument_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	s := &pluginState{cfg: Config{Title: "PetShop", Version: "1.0"}}
	applyDefaults(&s.cfg)
	doc := s.buildDocument(client.Manifest{
		Endpoints: []client.EndpointInfo{
			{Service: "pets", Transport: "rest", Method: "GET", Path: "/pets"},
		},
	})
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var back Document
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("round-trip decode: %v\n%s", err, b)
	}
	if back.Info.Title != "PetShop" {
		t.Errorf("Title round-trip: %q", back.Info.Title)
	}
}

func keys[K comparable, V any](m map[K]V) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
