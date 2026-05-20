package peer

import (
	"reflect"
	"testing"
)

// TestReflectSchema_Primitives walks the basic Go type → JSON
// Schema type mapping. Catches mistakes like int→"number" (should
// be integer) or string→"text" (no such JSON-Schema type).
func TestReflectSchema_Primitives(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{true, "boolean"},
		{int(0), "integer"},
		{int64(0), "integer"},
		{uint32(0), "integer"},
		{float64(0), "number"},
		{float32(0), "number"},
		{"", "string"},
	}
	for _, c := range cases {
		got := ReflectSchema(reflect.TypeOf(c.in))
		if got.Type != c.want {
			t.Errorf("%T → Type=%q, want %q", c.in, got.Type, c.want)
		}
	}
}

// TestReflectSchema_ByteSliceIsString proves the []byte special-
// case. Without it, []byte would describe as `array<integer>` —
// technically the kind-level truth, but completely wrong for the
// wire (encoding/json renders []byte as base64-encoded strings).
func TestReflectSchema_ByteSliceIsString(t *testing.T) {
	s := ReflectSchema(reflect.TypeOf([]byte{}))
	if s.Type != "string" || s.Format != "byte" {
		t.Errorf("[]byte → %+v, want type=string format=byte", s)
	}
}

// schemaUser is the headline test type. Exercises:
//   - tagged json names (Email → "email")
//   - omitempty marking a field optional
//   - validate:"required" overriding the omitempty heuristic
//   - pointer field marking optional
//   - nested struct producing a $ref + $defs entry
//   - slice producing array+items
type schemaAddress struct {
	Street string `json:"street"`
	City   string `json:"city"`
}

type schemaUser struct {
	ID      int64           `json:"id" validate:"required"`
	Email   string          `json:"email"`              // not optional → required
	Bio     string          `json:"bio,omitempty"`      // omitempty → optional
	Address *schemaAddress  `json:"address"`            // pointer → optional regardless of tag
	Tags    []string        `json:"tags,omitempty"`     // slice + optional
	History []schemaAddress `json:"history,omitempty"`  // slice of structs → $ref'd items
}

// TestReflectSchema_StructShape proves the headline shape: object
// with the right Properties, Required, $ref to a sibling type in
// $defs, and proper array→items unwinding for slices.
func TestReflectSchema_StructShape(t *testing.T) {
	s := ReflectSchema(reflect.TypeOf(schemaUser{}))
	if s.Type != "object" {
		t.Fatalf("Type = %q, want object", s.Type)
	}

	// Required: id (validate:"required") + email (not omitempty,
	// not pointer). Bio is omitempty; Address is *struct;
	// Tags + History are omitempty slices — all four optional.
	wantRequired := map[string]bool{"id": true, "email": true}
	gotRequired := map[string]bool{}
	for _, r := range s.Required {
		gotRequired[r] = true
	}
	for name := range wantRequired {
		if !gotRequired[name] {
			t.Errorf("Required missing %q (got %v)", name, s.Required)
		}
	}
	for _, name := range []string{"bio", "address", "tags", "history"} {
		if gotRequired[name] {
			t.Errorf("%q should be optional, found in Required", name)
		}
	}

	// Address property is a $ref pointing at #/$defs/schemaAddress.
	addr, ok := s.Properties["address"]
	if !ok {
		t.Fatal("Properties[address] missing")
	}
	if addr.Ref != "#/$defs/schemaAddress" {
		t.Errorf("address.Ref = %q, want #/$defs/schemaAddress", addr.Ref)
	}
	if _, ok := s.Defs["schemaAddress"]; !ok {
		t.Errorf("Defs missing schemaAddress (have %v)", keysOf(s.Defs))
	}

	// History is array<$ref schemaAddress> — the slice unwrap
	// should land the same $ref under Items, NOT recompute the
	// schema and add it to Defs twice.
	hist, ok := s.Properties["history"]
	if !ok {
		t.Fatal("Properties[history] missing")
	}
	if hist.Type != "array" {
		t.Errorf("history.Type = %q, want array", hist.Type)
	}
	if hist.Items == nil || hist.Items.Ref != "#/$defs/schemaAddress" {
		t.Errorf("history.Items = %+v, want $ref to schemaAddress", hist.Items)
	}
}

// schemaNode tests the cycle-break path: a self-referential type
// (graph node) used to crash naive recursive schema builders. The
// $ref-on-second-visit logic should produce a finite schema with
// the recursive reference resolving to #/$defs/schemaNode.
type schemaNode struct {
	Name     string        `json:"name"`
	Children []*schemaNode `json:"children,omitempty"`
}

func TestReflectSchema_HandlesRecursion(t *testing.T) {
	// The builder MUST terminate. If recursion isn't broken,
	// this test hangs forever — preferable to a stack overflow
	// but still actionable: a CI timeout means we got the
	// cycle-break wrong.
	s := ReflectSchema(reflect.TypeOf(schemaNode{}))
	if s.Type != "object" {
		t.Fatalf("Type = %q, want object", s.Type)
	}
	children, ok := s.Properties["children"]
	if !ok {
		t.Fatal("Properties[children] missing")
	}
	if children.Items == nil {
		t.Fatal("children.Items nil")
	}
	if children.Items.Ref != "#/$defs/schemaNode" {
		t.Errorf("children.Items.Ref = %q, want #/$defs/schemaNode", children.Items.Ref)
	}
	if _, ok := s.Defs["schemaNode"]; !ok {
		t.Errorf("Defs missing schemaNode")
	}
}

// TestReflectSchema_JSONTagSkipDash proves json:"-" skips a field
// from the emitted schema entirely. Encoding/json drops it from
// the wire; the schema reflector has to match or operators get
// spurious drift warnings.
func TestReflectSchema_JSONTagSkipDash(t *testing.T) {
	type secret struct {
		Public  string `json:"public"`
		Secret  string `json:"-"`
		Untaged string
	}
	s := ReflectSchema(reflect.TypeOf(secret{}))
	if _, ok := s.Properties["public"]; !ok {
		t.Error("Properties[public] missing")
	}
	if _, ok := s.Properties["Secret"]; ok {
		t.Error("Secret field should be skipped (json:-)")
	}
	if _, ok := s.Properties["secret"]; ok {
		t.Error("secret (lowercase) should be skipped (json:-)")
	}
	if _, ok := s.Properties["Untaged"]; !ok {
		t.Error("Untaged should fall back to Go field name when no json tag")
	}
}

// keysOf returns the map's key set as a sorted-by-iteration slice.
// Test-only helper for human-readable error messages.
func keysOf(m map[string]*Schema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
