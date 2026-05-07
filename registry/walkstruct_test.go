package registry

import (
	"reflect"
	"testing"
)

// TestWalkStruct_GraphqlOnlyTagWiresName pins the contract that
// graphql-only-tagged args structs surface their graphql tag's name
// on the wire. Without this, a struct like:
//
//	type LoginArgs struct {
//	    Username string `graphql:"username,required"`
//	}
//
// produces TS `Username: string` and the SDK builds a GraphQL document
// with `Username:` — which the registered schema (whose arg is named
// `username` from the same tag) rejects. See nexus-client.js:506
// (buildGqlDocument) for the consumer.
func TestWalkStruct_GraphqlOnlyTagWiresName(t *testing.T) {
	type LoginArgs struct {
		Username string `graphql:"username,required"`
		Password string `graphql:"password,required"`
	}
	refs := map[string]NamedType{}
	WalkType(reflect.TypeOf(LoginArgs{}), refs)

	nt, ok := findRef(refs, "LoginArgs")
	if !ok {
		t.Fatalf("expected LoginArgs in refs; got %v", keys(refs))
	}
	got := map[string]string{}
	for _, f := range nt.Fields {
		got[f.Name] = f.JSONName
		if f.GraphQLName == "" {
			t.Errorf("field %q: GraphQLName empty; want from graphql tag", f.Name)
		}
	}
	want := map[string]string{
		"Username": "username",
		"Password": "password",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("field %q: JSONName=%q, want %q", k, got[k], v)
		}
	}
}

// TestWalkStruct_JsonTagWinsOverGraphqlTag covers the dual-tag case.
// When both tags are present and disagree, json wins for JSONName
// (drives REST + TS) and GraphQLName preserves the graphql tag for
// any future per-transport projection.
func TestWalkStruct_JsonTagWinsOverGraphqlTag(t *testing.T) {
	type Args struct {
		Username string `json:"user_name" graphql:"username"`
	}
	refs := map[string]NamedType{}
	WalkType(reflect.TypeOf(Args{}), refs)
	nt, _ := findRef(refs, "Args")
	if len(nt.Fields) != 1 {
		t.Fatalf("want 1 field, got %d", len(nt.Fields))
	}
	f := nt.Fields[0]
	if f.JSONName != "user_name" {
		t.Errorf("JSONName=%q, want user_name (json tag should win)", f.JSONName)
	}
	if f.GraphQLName != "username" {
		t.Errorf("GraphQLName=%q, want username", f.GraphQLName)
	}
}

// TestWalkStruct_NoTagsKeepsGoFieldName guards against the simple-fix
// fallback accidentally rewriting plain Go field names when neither
// tag is present.
func TestWalkStruct_NoTagsKeepsGoFieldName(t *testing.T) {
	type Args struct {
		Plain string
	}
	refs := map[string]NamedType{}
	WalkType(reflect.TypeOf(Args{}), refs)
	nt, _ := findRef(refs, "Args")
	f := nt.Fields[0]
	if f.JSONName != "" {
		t.Errorf("JSONName=%q, want empty (caller falls back to Name)", f.JSONName)
	}
	if f.GraphQLName != "" {
		t.Errorf("GraphQLName=%q, want empty", f.GraphQLName)
	}
}

func findRef(refs map[string]NamedType, suffix string) (NamedType, bool) {
	for k, v := range refs {
		if k == suffix || hasSuffix(k, "."+suffix) || hasSuffix(k, "/"+suffix) {
			return v, true
		}
	}
	// Try lowerFirst — the registry typically lowercases the first
	// letter when keying refs.
	lower := lowerFirst(suffix)
	for k, v := range refs {
		if k == lower {
			return v, true
		}
	}
	return NamedType{}, false
}

func keys(m map[string]NamedType) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'A' && r[0] <= 'Z' {
		r[0] += 'a' - 'A'
	}
	return string(r)
}