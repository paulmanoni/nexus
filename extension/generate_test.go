package extension

import (
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
// extension.Use builds carries HasGenerate=true when the Generate
// slot is set. The dashboard reads this to mark plugins that
// contribute codegen, so a regression here would silently lose the
// signal.
func TestUse_Generate_RecordsHasGenerate(t *testing.T) {
	p := Plugin{
		Name: "fe",
		Generate: &Generate{
			OutDir: func(*nexus.App) (string, error) { return "/tmp", nil },
			Render: func(GenerateContext) ([]File, error) { return nil, nil },
		},
	}
	if opt := Use(p); opt == nil {
		t.Fatal("Use returned nil for valid Generate plugin")
	}
}

// TestCollectContributors_WrapsRegisteredRecords boots an App via
// nexus.New, registers two contributors directly on it (bypassing
// fx — the helper is a pure post-registration read), and asserts
// collectContributors hands them back wrapped in adapters that
// faithfully proxy through to the underlying functions.
//
// This is the load-bearing seam phase 2 introduces: the driver's
// Render reads contributors lazily from the App at call time, so
// fxBootOptions ordering can't reorder this relative to fx.Invoke.
func TestCollectContributors_WrapsRegisteredRecords(t *testing.T) {
	app := nexus.New(nexus.Config{})
	calls := 0
	app.RegisterClientContributor(nexus.ClientContributorRecord{
		PluginName: "a",
		Contribute: func(ctx nexus.GenerateContext) ([]nexus.GeneratedFile, error) {
			calls++
			return []nexus.GeneratedFile{{Path: "a.ts", Body: []byte("// a")}}, nil
		},
	})
	app.RegisterClientContributor(nexus.ClientContributorRecord{
		PluginName: "b",
		Contribute: func(ctx nexus.GenerateContext) ([]nexus.GeneratedFile, error) {
			calls++
			return []nexus.GeneratedFile{{Path: "b.ts", Body: []byte("// b")}}, nil
		},
	})

	got := collectContributors(app)
	if len(got) != 2 {
		t.Fatalf("len(collectContributors) = %d, want 2", len(got))
	}

	for _, c := range got {
		files, err := c.NexusContribute(GenerateContext{})
		if err != nil {
			t.Fatalf("NexusContribute: %v", err)
		}
		if len(files) != 1 {
			t.Errorf("contributor returned %d files, want 1", len(files))
		}
	}
	if calls != 2 {
		t.Fatalf("underlying fns called %d times, want 2", calls)
	}
}

// TestCollectContributors_PropagatesErrors checks that a contributor
// failure surfaces as "contributor <name>: <err>" so the driver's
// callsite can point at the bad plugin in a build log.
func TestCollectContributors_PropagatesErrors(t *testing.T) {
	app := nexus.New(nexus.Config{})
	app.RegisterClientContributor(nexus.ClientContributorRecord{
		PluginName: "broken",
		Contribute: func(ctx nexus.GenerateContext) ([]nexus.GeneratedFile, error) {
			return nil, errBoom
		},
	})
	got := collectContributors(app)
	_, err := got[0].NexusContribute(GenerateContext{})
	if err == nil {
		t.Fatal("expected error from broken contributor")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Fatalf("error %q does not name plugin", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error %q does not wrap inner cause", err)
	}
}

// errBoom is the sentinel returned by the broken contributor in the
// error-propagation test. Lives at package scope so the test reads
// cleanly without an inline errors.New.
var errBoom = stubErr("boom")

type stubErr string

func (s stubErr) Error() string { return string(s) }

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
