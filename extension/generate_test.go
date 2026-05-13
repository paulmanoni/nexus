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
