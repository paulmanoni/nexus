package inertia_test

import (
	"context"
	"testing"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/inertia"
	"github.com/paulmanoni/nexus/registry"
)

// Viewer is a named shared-prop type: it must reach the schema refs pool so
// the SDK's NexusSharedProps can reference it by name.
type Viewer struct {
	Name string `json:"name"`
}

// TestPageTypes covers the type surface Page and the typed shares feed the
// client SDK: the page tag carries the component on every verb's endpoint,
// Prop fields walk to optional "any" (not a ref to an empty Prop), and
// ShareScoped / ShareTyped record their value types — named ones in refs —
// while an untyped Share records nothing.
func TestPageTypes(t *testing.T) {
	gates := nexus.NewScoped[map[string]bool](func() nexus.Compute[map[string]bool] {
		return func(ctx context.Context) (map[string]bool, error) { return map[string]bool{"saveUser": true}, nil }
	})
	app, stop, err := nexus.InProcess(nexus.Config{},
		inertia.Page("GET", "/widgets", "Widgets/Index", NewWidgets),
		inertia.Page("GET,POST", "/auth", "Auth", NewAuthForm),
		gates,
		inertia.ShareScoped("can", gates),
		inertia.ShareTyped("viewer", func(ctx context.Context) (*Viewer, error) { return &Viewer{Name: "ada"}, nil }),
		inertia.Share(func(ctx context.Context) (string, any) { return "csrf", "tok" }),
	)
	if err != nil {
		t.Fatalf("InProcess: %v", err)
	}
	defer stop(context.Background())

	pages := map[string]int{}
	for _, e := range app.Registry().Endpoints() {
		c := e.Tags[registry.PageTag]
		if c == "" {
			continue
		}
		pages[c]++
		if c != "Widgets/Index" {
			continue
		}
		if e.ReturnSchema == nil || e.ReturnSchema.Kind != "ref" {
			t.Fatalf("Widgets/Index return schema = %+v, want a ref", e.ReturnSchema)
		}
		props, ok := app.SchemaRefs()[e.ReturnSchema.Ref]
		if !ok {
			t.Fatalf("page props %q missing from refs", e.ReturnSchema.Ref)
		}
		for _, f := range props.Fields {
			if f.JSONName == "stats" || f.JSONName == "menu" {
				if f.Type.Kind != "any" || !f.Optional {
					t.Errorf("Prop field %s = %+v optional=%v, want optional any", f.JSONName, f.Type, f.Optional)
				}
			}
		}
	}
	if pages["Widgets/Index"] != 1 || pages["Auth"] != 2 {
		t.Errorf("page tags by component = %v, want Widgets/Index:1 Auth:2 (one per verb)", pages)
	}
	if _, leaked := app.SchemaRefs()["Prop"]; leaked {
		t.Error("inertia.Prop leaked into the refs pool as an empty interface")
	}

	shared := app.Registry().SharedProps()
	if c := shared["can"]; c.Kind != "map" || c.Of == nil || c.Of.Primitive != "boolean" {
		t.Errorf("shared can = %+v, want map of boolean", c)
	}
	if v := shared["viewer"]; v.Kind != "ref" || v.Ref != "Viewer" {
		t.Errorf("shared viewer = %+v, want ref Viewer", v)
	}
	if _, ok := app.SchemaRefs()["Viewer"]; !ok {
		t.Error("Viewer missing from the refs pool")
	}
	if _, ok := shared["csrf"]; ok {
		t.Error("untyped Share must not record a type")
	}
}
