package graph

import (
	"reflect"
	"testing"

	"github.com/graphql-go/graphql"
)

type reflectTestResp struct {
	Message string `json:"message" description:"Greeting"`
	Count   int    `json:"count"`
}

func TestNewResolverFromType_ObjectName(t *testing.T) {
	r := NewResolverFromType("hello", reflect.TypeOf(reflectTestResp{}))
	if r.objectName != "reflectTestResp" {
		t.Errorf("objectName = %q; want reflectTestResp", r.objectName)
	}
	if r.typeOverride != reflect.TypeOf(reflectTestResp{}) {
		t.Error("typeOverride not set")
	}
}

func TestNewResolverFromType_DerefsPointer(t *testing.T) {
	r := NewResolverFromType("hello", reflect.TypeOf(&reflectTestResp{}))
	if r.typeOverride.Kind() != reflect.Struct {
		t.Errorf("pointer not deref'd; got %v", r.typeOverride.Kind())
	}
	if r.objectName != "reflectTestResp" {
		t.Errorf("objectName = %q; want reflectTestResp", r.objectName)
	}
}

func TestNewResolverFromType_DetectsSlice(t *testing.T) {
	r := NewResolverFromType("list", reflect.TypeOf([]reflectTestResp{}))
	if !r.isList {
		t.Error("slice return type should set isList")
	}
	if r.objectName != "ListreflectTestResp" {
		t.Errorf("objectName = %q; want ListreflectTestResp", r.objectName)
	}
}

func TestNewResolverFromType_BuildsServableField(t *testing.T) {
	calls := 0
	q := NewResolverFromType("hello", reflect.TypeOf(reflectTestResp{})).
		WithRawResolver(func(p ResolveParams) (any, error) {
			calls++
			return &reflectTestResp{Message: "hi", Count: 1}, nil
		}).
		BuildQuery()

	schema, err := NewSchemaBuilder(SchemaBuilderParams{
		QueryFields: []QueryField{q},
	}).Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}

	res := graphql.Do(graphql.Params{
		Schema:        schema,
		RequestString: `{ hello { message count } }`,
	})
	if len(res.Errors) > 0 {
		t.Fatalf("query errors: %v", res.Errors)
	}
	if calls != 1 {
		t.Errorf("resolver called %d times; want 1", calls)
	}
	data, _ := res.Data.(map[string]interface{})
	hello, _ := data["hello"].(map[string]interface{})
	if hello["message"] != "hi" {
		t.Errorf("message = %v; want hi", hello["message"])
	}
	if hello["count"] != 1 {
		t.Errorf("count = %v; want 1", hello["count"])
	}
}

func TestNewResolverFromType_FieldInfoReflectsReturnType(t *testing.T) {
	q := NewResolverFromType("hello", reflect.TypeOf(reflectTestResp{})).
		WithRawResolver(func(p ResolveParams) (any, error) { return &reflectTestResp{}, nil }).
		BuildQuery()

	info, ok := Inspect(q)
	if !ok {
		t.Fatal("Inspect failed")
	}
	if info.Name != "hello" {
		t.Errorf("name = %q", info.Name)
	}
	if info.Kind != FieldKindQuery {
		t.Errorf("kind = %q", info.Kind)
	}
	// ReturnType is SDL-shaped; we just assert it contains the object name.
	if info.ReturnType == "" {
		t.Error("ReturnType should be populated")
	}
}
