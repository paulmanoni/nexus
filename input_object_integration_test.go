package nexus

import (
	"reflect"
	"testing"

	"github.com/graphql-go/graphql"

	"github.com/paulmanoni/nexus/graph"
)

// Rebuild what asGqlField does to a resolver, then verify the assembled
// r.args entry actually carries the input-object type rather than flat
// fields. This locks in the integration between detectInputObject and
// go-graph's WithInputObject.
func TestApplyArgsFromStruct_BuildsInputObject(t *testing.T) {
	type Response struct {
		Message string `json:"message"`
	}

	argsType := reflect.TypeOf(inputTestWrapper{})
	r := graph.NewResolverFromType("createThing", reflect.TypeOf(Response{}))

	inputName := applyArgsFromStruct(r, argsType)
	if inputName != "input" {
		t.Fatalf("applyArgsFromStruct returned %q; want input", inputName)
	}

	// Build the schema and run an introspection query to see what
	// graphql-go actually ended up with.
	r.WithRawResolver(func(p graph.ResolveParams) (any, error) {
		return &Response{Message: "ok"}, nil
	})
	m := r.BuildMutation()

	schema, err := graph.NewSchemaBuilder(graph.SchemaBuilderParams{
		QueryFields: []graph.QueryField{
			graph.NewResolverFromType("ping", reflect.TypeOf(Response{})).
				WithRawResolver(func(p graph.ResolveParams) (any, error) {
					return &Response{}, nil
				}).BuildQuery(),
		},
		MutationFields: []graph.MutationField{m},
	}).Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}

	// Ask the schema about the mutation field's args.
	res := graphql.Do(graphql.Params{
		Schema:        schema,
		RequestString: `{ __type(name: "Mutation") { fields { name args { name type { name kind ofType { name kind } } } } } }`,
	})
	if len(res.Errors) > 0 {
		t.Fatalf("introspect: %v", res.Errors)
	}

	// Walk the introspection result to find createThing's args.
	data, _ := res.Data.(map[string]interface{})
	typ, _ := data["__type"].(map[string]interface{})
	fields, _ := typ["fields"].([]interface{})
	var argsSlice []interface{}
	for _, f := range fields {
		fm := f.(map[string]interface{})
		if fm["name"] == "createThing" {
			argsSlice, _ = fm["args"].([]interface{})
			break
		}
	}
	if argsSlice == nil {
		t.Fatal("createThing not found in Mutation fields")
	}
	if len(argsSlice) != 1 {
		names := make([]string, len(argsSlice))
		for i, a := range argsSlice {
			names[i] = a.(map[string]interface{})["name"].(string)
		}
		t.Fatalf("expected 1 arg (input-object), got %d: %v", len(argsSlice), names)
	}
	a0 := argsSlice[0].(map[string]interface{})
	if a0["name"] != "input" {
		t.Errorf("arg name = %v; want input", a0["name"])
	}
}
