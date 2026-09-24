package registry

import (
	"reflect"
	"testing"
)

// opaqueProp stands in for inertia.Prop: unexported fields, resolved at write
// time, so its Go shape says nothing about the JSON it produces.
type opaqueProp struct{ val any }

func (opaqueProp) NexusSchemaOpaque() {}

type ptrOpaque struct{ n int }

func (*ptrOpaque) NexusSchemaOpaque() {}

// TestWalkType_SchemaOpaque pins the opt-out: a SchemaOpaque field walks to
// an optional "any" (TS `field?: unknown`) and never enters the refs pool,
// while plain fields and a bare `any` keep their normal rendering.
func TestWalkType_SchemaOpaque(t *testing.T) {
	type pageProps struct {
		Title string      `json:"title"`
		Stats opaqueProp  `json:"stats"`
		Menu  *opaqueProp `json:"menu"`
		Count ptrOpaque   `json:"count"`
		Extra any         `json:"extra"`
	}
	refs := map[string]NamedType{}
	WalkType(reflect.TypeOf(pageProps{}), refs)

	for _, leaked := range []string{"opaqueProp", "ptrOpaque"} {
		if _, ok := refs[leaked]; ok {
			t.Errorf("%s leaked into refs: %v", leaked, keys(refs))
		}
	}
	nt, ok := findRef(refs, "pageProps")
	if !ok {
		t.Fatalf("pageProps missing from refs: %v", keys(refs))
	}
	want := map[string]bool{"title": false, "stats": true, "menu": true, "count": true, "extra": false}
	for _, f := range nt.Fields {
		opt, tracked := want[f.JSONName]
		if !tracked {
			continue
		}
		if f.Optional != opt {
			t.Errorf("field %s: Optional = %v, want %v", f.JSONName, f.Optional, opt)
		}
		if f.JSONName != "title" && f.Type.Kind != "any" {
			t.Errorf("field %s: Kind = %q, want any", f.JSONName, f.Type.Kind)
		}
	}

	top := WalkType(reflect.TypeOf(opaqueProp{}), refs)
	if top.Kind != "any" || !top.Optional {
		t.Errorf("top-level opaque walk = %+v, want optional any", top)
	}
	if bare := WalkType(reflect.TypeOf((*any)(nil)).Elem(), refs); bare.Optional {
		t.Errorf("bare any must stay non-optional, got %+v", bare)
	}
}

// TestSharedProps covers the registry's typed shared-prop store: set,
// replace (last wins), empty-key ignore, and a defensive copy on read.
func TestSharedProps(t *testing.T) {
	r := New()
	if r.SharedProps() != nil {
		t.Fatal("fresh registry should report no shared props")
	}
	r.SetSharedProp("can", TypeRef{Kind: "primitive", Primitive: "string"})
	r.SetSharedProp("can", TypeRef{Kind: "primitive", Primitive: "boolean"})
	r.SetSharedProp("", TypeRef{Kind: "any"})

	got := r.SharedProps()
	if len(got) != 1 || got["can"].Primitive != "boolean" {
		t.Fatalf("SharedProps = %v, want only can:boolean", got)
	}
	got["other"] = TypeRef{Kind: "any"}
	if _, ok := r.SharedProps()["other"]; ok {
		t.Error("SharedProps must return a copy")
	}
}
