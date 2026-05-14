package nexus

import (
	"reflect"
	"testing"
)

// GraphQL list args arrive as []interface{} regardless of the declared element
// type. The flat-args bind path must coerce them into the destination's typed
// slice (e.g. []string, []int) instead of failing with "cannot assign".

func TestBindGqlArgs_StringSlice(t *testing.T) {
	var args struct {
		InterviewUids []string `graphql:"interviewUids"`
	}
	if err := bindGqlArgs(&args, map[string]any{
		"interviewUids": []interface{}{"a", "b", "c"},
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if !reflect.DeepEqual(args.InterviewUids, []string{"a", "b", "c"}) {
		t.Fatalf("got %#v", args.InterviewUids)
	}
}

func TestBindGqlArgs_IntSlice(t *testing.T) {
	var args struct {
		IDs []int `graphql:"ids"`
	}
	// graphql-go decodes Int as int; mirror that here.
	if err := bindGqlArgs(&args, map[string]any{
		"ids": []interface{}{1, 2, 3},
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if !reflect.DeepEqual(args.IDs, []int{1, 2, 3}) {
		t.Fatalf("got %#v", args.IDs)
	}
}

func TestBindGqlArgs_EmptySlice(t *testing.T) {
	var args struct {
		Uids []string `graphql:"uids"`
	}
	if err := bindGqlArgs(&args, map[string]any{
		"uids": []interface{}{},
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if len(args.Uids) != 0 {
		t.Fatalf("expected empty slice, got %#v", args.Uids)
	}
}

func TestBindGqlArgs_NonSliceStillFails(t *testing.T) {
	// Guard the recursion: a scalar destination given a struct value should
	// still report "cannot assign" rather than panic.
	var args struct {
		Name string `graphql:"name"`
	}
	err := bindGqlArgs(&args, map[string]any{
		"name": map[string]any{"oops": true},
	})
	if err == nil {
		t.Fatal("expected error for incompatible types")
	}
}