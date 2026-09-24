package extension

import (
	"context"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus"
)

// TestValidate_Generate exercises the Generate slot's validation rules.
// Both fields are required when the slot is non-nil — a typo'd plugin
// (forgetting OutDir, say) should fail at construction, not at
// `nexus build` time when the partial signature panics.
func TestValidate_Generate(t *testing.T) {
	cases := []struct {
		name    string
		plugin  Plugin
		wantErr string
	}{
		{
			name: "Generate without OutDir rejected",
			plugin: Plugin{
				Name: "fe",
				Generate: &Generate{
					Render: func(GenerateContext) ([]File, error) { return nil, nil },
				},
			},
			wantErr: "Generate.OutDir is required",
		},
		{
			name: "Generate without Render rejected",
			plugin: Plugin{
				Name: "fe",
				Generate: &Generate{
					OutDir: func(*nexus.App) (string, error) { return "/tmp", nil },
				},
			},
			wantErr: "Generate.Render is required",
		},
		{
			name: "Generate with both fields valid",
			plugin: Plugin{
				Name: "fe",
				Generate: &Generate{
					OutDir: func(*nexus.App) (string, error) { return "/tmp", nil },
					Render: func(GenerateContext) ([]File, error) { return nil, nil },
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(tc.plugin)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate: %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate: %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestUse_Generate_RecordsHasGenerate asserts the PluginRecord that
// extension.Use builds carries HasGenerate=true when the (deprecated)
// Generate slot is set, and that Use no longer registers a driver —
// nothing ever read one back. Two Generate plugins in one app therefore
// boot instead of panicking on the old one-driver rule.
func TestUse_Generate_RecordsHasGenerate(t *testing.T) {
	gen := func(name string) Plugin {
		return Plugin{
			Name: name,
			Generate: &Generate{
				OutDir: func(*nexus.App) (string, error) { return "/tmp", nil },
				Render: func(GenerateContext) ([]File, error) { return nil, nil },
			},
		}
	}
	app, stop, err := nexus.InProcess(nexus.Config{}, Use(gen("fe")), Use(gen("fe2")))
	if err != nil {
		t.Fatalf("InProcess: %v", err)
	}
	defer stop(context.Background())

	flagged := map[string]bool{}
	for _, r := range app.Plugins() {
		if r.HasGenerate {
			flagged[r.Name] = true
		}
	}
	if len(flagged) != 2 || !flagged["fe"] || !flagged["fe2"] {
		t.Fatalf("plugins flagged HasGenerate = %v, want exactly fe and fe2", flagged)
	}
	if drv := app.GenerateDrivers(); len(drv) != 0 {
		t.Fatalf("GenerateDrivers() = %d drivers, want none registered by Use", len(drv))
	}
}

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
