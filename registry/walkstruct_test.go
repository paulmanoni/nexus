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

// unexportedBase is an unexported embedded base struct. Go's
// encoding/json promotes its EXPORTED fields onto the embedding type's
// wire shape, so the SDK types must too.
type unexportedBase struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"createdAt"`
	secret    string // unexported — never on the wire
}

// TestWalkStruct_UnexportedEmbeddedStructPromotes pins that an anonymous
// embedded struct whose TYPE is unexported still contributes its exported
// fields (matching encoding/json), and that the internal type name does
// not leak into refs as a generated interface.
func TestWalkStruct_UnexportedEmbeddedStructPromotes(t *testing.T) {
	type User struct {
		unexportedBase
		Name string `json:"name"`
	}
	refs := map[string]NamedType{}
	WalkType(reflect.TypeOf(User{}), refs)

	nt, ok := findRef(refs, "User")
	if !ok {
		t.Fatalf("expected User in refs; got %v", keys(refs))
	}
	got := map[string]bool{}
	for _, f := range nt.Fields {
		got[f.Name] = true
	}
	for _, want := range []string{"ID", "CreatedAt", "Name"} {
		if !got[want] {
			t.Errorf("field %q not promoted; have %v", want, got)
		}
	}
	if got["secret"] {
		t.Errorf("unexported field 'secret' leaked onto the wire")
	}
	// The internal base type must not surface as its own SDK interface.
	if _, leaked := findRef(refs, "unexportedBase"); leaked {
		t.Errorf("unexported embedded type leaked into refs: %v", keys(refs))
	}
}

// wireNames returns the effective wire name of each field in declaration
// order (JSONName when set, else the Go field name).
func wireNames(nt NamedType) []string {
	out := make([]string, len(nt.Fields))
	for i, f := range nt.Fields {
		if f.JSONName != "" {
			out[i] = f.JSONName
		} else {
			out[i] = f.Name
		}
	}
	return out
}

func hasWire(nt NamedType, name string) bool {
	for _, w := range wireNames(nt) {
		if w == name {
			return true
		}
	}
	return false
}

// TestWalkStruct_ShallowFieldShadowsEmbedded pins encoding/json's rule
// that a field declared directly on the struct (depth 0) dominates a
// same-named field promoted from an embedded type (depth 1) — the
// embedded one is dropped, not duplicated.
func TestWalkStruct_ShallowFieldShadowsEmbedded(t *testing.T) {
	type base struct {
		Name string `json:"name"`
		ID   int64  `json:"id"`
	}
	type User struct {
		base
		Name string `json:"name"` // shadows base.Name
	}
	refs := map[string]NamedType{}
	WalkType(reflect.TypeOf(User{}), refs)
	nt, _ := findRef(refs, "User")

	count := 0
	for _, w := range wireNames(nt) {
		if w == "name" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("wire name \"name\" appears %d times, want 1 (parent shadows embedded); fields=%v", count, wireNames(nt))
	}
	if !hasWire(nt, "id") {
		t.Errorf("embedded non-colliding field \"id\" missing; fields=%v", wireNames(nt))
	}
}

// TestWalkStruct_AmbiguousSiblingEmbedsDropped pins that two fields
// promoted from sibling embeds at the SAME depth with the same wire name
// and the same tag-presence are a genuine tie and both get dropped —
// matching encoding/json's ambiguous-selector behavior.
func TestWalkStruct_AmbiguousSiblingEmbedsDropped(t *testing.T) {
	// Untagged fields collide on the Go field name "Conflict" with equal
	// depth and equal (absent) tag-presence — a genuine tie.
	type left struct {
		Conflict string
		OnlyL    string
	}
	type right struct {
		Conflict string
		OnlyR    string
	}
	type Merged struct {
		left
		right
	}
	refs := map[string]NamedType{}
	WalkType(reflect.TypeOf(Merged{}), refs)
	nt, _ := findRef(refs, "Merged")

	if hasWire(nt, "Conflict") {
		t.Errorf("ambiguous \"Conflict\" should be dropped; fields=%v", wireNames(nt))
	}
	if !hasWire(nt, "OnlyL") || !hasWire(nt, "OnlyR") {
		t.Errorf("non-colliding sibling fields should survive; fields=%v", wireNames(nt))
	}
}

// TestWalkStruct_TaggedBeatsUntaggedAtSameDepth pins the tiebreak: at
// equal depth a field whose wire name came from an explicit tag wins
// over an untagged one that happens to share that wire name.
func TestWalkStruct_TaggedBeatsUntaggedAtSameDepth(t *testing.T) {
	type untaggedSide struct {
		Dup string // wire name "Dup" (untagged)
	}
	type taggedSide struct {
		Other string `json:"Dup"` // wire name "Dup" (tagged) — collides
	}
	type Holder struct {
		untaggedSide
		taggedSide
	}
	refs := map[string]NamedType{}
	WalkType(reflect.TypeOf(Holder{}), refs)
	nt, _ := findRef(refs, "Holder")
	if !hasWire(nt, "Dup") {
		t.Fatalf("tagged field should win the \"Dup\" collision; fields=%v", wireNames(nt))
	}
	// The survivor must be the tagged one (Go name "Other").
	for _, f := range nt.Fields {
		wire := f.Name
		if f.JSONName != "" {
			wire = f.JSONName
		}
		if wire == "Dup" && f.Name != "Other" {
			t.Errorf("untagged field won; want tagged (Other), got %q", f.Name)
		}
	}
}

// TestWalkStruct_CyclicEmbedTerminates guards that a pointer-cyclic embed
// (`type cyclicNode struct{ *cyclicNode }`) doesn't recurse forever.
func TestWalkStruct_CyclicEmbedTerminates(t *testing.T) {
	refs := map[string]NamedType{}
	WalkType(reflect.TypeOf(cyclicNode{}), refs)
	nt, _ := findRef(refs, "cyclicNode")
	if !hasWire(nt, "value") {
		t.Errorf("expected \"value\" field on cyclic node; fields=%v", wireNames(nt))
	}
}

// cyclicNode embeds a pointer to itself — exercises the cycle guard in
// collectStructFields.
type cyclicNode struct {
	*cyclicNode
	Value string `json:"value"`
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