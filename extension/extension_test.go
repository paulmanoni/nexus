package extension

import (
	"context"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/httpx"

	"github.com/paulmanoni/nexus"
)

func TestValidate(t *testing.T) {
	okHandler := func(c *httpx.Ctx) {}

	cases := []struct {
		name    string
		plugin  Plugin
		wantErr string // substring; empty means expect nil
	}{
		{
			name:    "empty Name rejected",
			plugin:  Plugin{},
			wantErr: "Name is required",
		},
		{
			name:    "Name with slash rejected",
			plugin:  Plugin{Name: "auth/v2"},
			wantErr: "must be kebab-case",
		},
		{
			name:    "Name with whitespace rejected",
			plugin:  Plugin{Name: "feature flags"},
			wantErr: "must be kebab-case",
		},
		{
			name: "Dashboard.Route missing Method rejected",
			plugin: Plugin{
				Name: "x",
				Dashboard: &Dashboard{
					Routes: []Route{{Path: "/p", Handler: okHandler}},
				},
			},
			wantErr: "Method is required",
		},
		{
			name: "Dashboard.Route missing Handler rejected",
			plugin: Plugin{
				Name: "x",
				Dashboard: &Dashboard{
					Routes: []Route{{Method: "GET", Path: "/p"}},
				},
			},
			wantErr: "Handler is required",
		},
		{
			name:   "minimal valid plugin",
			plugin: Plugin{Name: "ok"},
		},
		{
			name: "valid plugin with all slots",
			plugin: Plugin{
				Name:    "ok",
				Version: "1.0",
				Dashboard: &Dashboard{
					Tab:    &Tab{ID: "x", Label: "X"},
					Routes: []Route{{Method: "GET", Path: "", Handler: okHandler}},
				},
				Client:    &Client{Namespace: "x"},
				Lifecycle: &Lifecycle{},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(tc.plugin)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate returned %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate returned nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestUse_ReturnsOption(t *testing.T) {
	// Sanity: extension.Use of a valid Plugin returns a non-nil Option
	// that nexus.Options can flatten. We don't boot fx here — the goal
	// is to catch gross panics / nil-derefs in the Use plumbing.
	opt := Use(Plugin{
		Name:    "smoke",
		Version: "1",
		Options: []nexus.Option{nexus.Invoke(func() {})},
		Lifecycle: &Lifecycle{
			OnBoot: func(context.Context, *nexus.App) error { return nil },
		},
		Dashboard: &Dashboard{
			Tab:        &Tab{ID: "smoke", Label: "Smoke"},
			Routes:     []Route{{Method: "GET", Path: "/p", Handler: func(c *httpx.Ctx) {}}},
			LiveEvents: []string{"smoke.evt"},
		},
		Client: &Client{
			Namespace: "smoke",
			Apply:     func(*nexus.App) error { return nil },
		},
	})
	if opt == nil {
		t.Fatal("Use returned nil Option")
	}
	// Should be wrappable in nexus.Options without panicking.
	if nested := nexus.Options(opt); nested == nil {
		t.Fatal("nexus.Options(opt) returned nil")
	}
}

func TestTabRecord_MapsFields(t *testing.T) {
	got := tabRecord(&Dashboard{Tab: &Tab{ID: "a", Label: "B", Icon: "c"}})
	if got == nil {
		t.Fatal("tabRecord returned nil for non-nil Tab")
	}
	if got.ID != "a" || got.Label != "B" || got.Icon != "c" {
		t.Fatalf("tabRecord mismatch: %+v", got)
	}
	if tabRecord(nil) != nil {
		t.Fatal("tabRecord(nil) should return nil")
	}
	if tabRecord(&Dashboard{}) != nil {
		t.Fatal("tabRecord with no Tab should return nil")
	}
}

func TestLiveEvents_Copies(t *testing.T) {
	src := []string{"a", "b"}
	out := liveEvents(&Dashboard{LiveEvents: src})
	if len(out) != 2 || out[0] != "a" || out[1] != "b" {
		t.Fatalf("liveEvents: %v", out)
	}
	// Mutating source should not affect the snapshot.
	src[0] = "mutated"
	if out[0] == "mutated" {
		t.Fatal("liveEvents returned a shared slice; expected a copy")
	}
}
