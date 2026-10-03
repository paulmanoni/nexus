package extension

import (
	"testing"
)

// TestStaticContributor_WrapsFiles is the adapter the CLI uses to
// inject HTTP-fetched contributions back into the renderer's
// Contributors slot. The contributor must ignore the GenerateContext
// (the bytes are pre-rendered) and return the wrapped slice verbatim.
func TestStaticContributor_WrapsFiles(t *testing.T) {
	in := []File{
		{Path: "auth/vue.ts", Body: []byte("// auth")},
		{Path: "oauth2/vue.ts", Body: []byte("// oauth2")},
	}
	c := StaticContributor(in)
	got, err := c.NexusContribute(GenerateContext{}) // empty ctx — must not panic
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Path != "auth/vue.ts" || got[1].Path != "oauth2/vue.ts" {
		t.Fatalf("StaticContributor returned %+v, want both files in order", got)
	}
	// Mutating the input slice after construction must not affect
	// the wrapped contributor — the adapter copies on construct.
	in[0].Body = []byte("MUTATED")
	got2, _ := c.NexusContribute(GenerateContext{})
	if string(got2[0].Body) == "MUTATED" {
		t.Fatal("StaticContributor shared the input slice — must copy on construct")
	}
}

// TestContributorFunc_AdapterCallsThrough verifies the ContributorFunc
// helper passes the GenerateContext through unchanged and propagates
// the returned files / error. Tiny test but the adapter is the
// path-of-least-resistance for users who'd rather write a function
// than declare a named contributor type.
func TestContributorFunc_AdapterCallsThrough(t *testing.T) {
	called := false
	var got GenerateContext
	c := ContributorFunc(func(ctx GenerateContext) ([]File, error) {
		called = true
		got = ctx
		return []File{{Path: "x.ts", Body: []byte("// x")}}, nil
	})
	files, err := c.NexusContribute(GenerateContext{
		BasePath: "/api",
		Extras:   map[string]any{"k": "v"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("underlying function never invoked")
	}
	if got.BasePath != "/api" || got.Extras["k"] != "v" {
		t.Fatalf("context not passed through: %+v", got)
	}
	if len(files) != 1 || files[0].Path != "x.ts" {
		t.Fatalf("files not returned: %+v", files)
	}
}
