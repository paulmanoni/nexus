package config

import (
	"reflect"
	"testing"
)

// TestResolveSnapshot_TwoTierMerge drives the four-step merge:
// _common.default → _common.<profile> → <app>.default →
// <app>.<profile>. Each later layer overlays on top, leaf
// values from the more specific layer win.
func TestResolveSnapshot_TwoTierMerge(t *testing.T) {
	content := map[string]appBody{
		"_common": {Profiles: map[string]map[string]any{
			"default": {
				"environment":   "shared-base",
				"observability": map[string]any{"log_level": "info"},
			},
			"prod": {
				"observability": map[string]any{"log_level": "warn"},
			},
		}},
		"app1": {Profiles: map[string]map[string]any{
			"default": {
				"api":      map[string]any{"timeout": "5s"},
				"features": map[string]any{"new_checkout": false},
			},
			"prod": {
				"features": map[string]any{"new_checkout": true},
				"api":      map[string]any{"timeout": "10s"},
			},
		}},
	}

	got, err := resolveSnapshot(content, "app1", "prod")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"environment": "shared-base",
		"observability": map[string]any{
			"log_level": "warn", // prod overlay
		},
		"api": map[string]any{
			"timeout": "10s", // prod overlay
		},
		"features": map[string]any{
			"new_checkout": true, // prod overlay
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merge mismatch:\n  got:  %v\n  want: %v", got, want)
	}
}

// TestResolveSnapshot_DefaultProfile proves the no-overlay case:
// when only `default` is defined, requesting any profile returns
// the default verbatim. Otherwise an app without a per-env
// section would fail to serve any profile but "default."
func TestResolveSnapshot_DefaultProfile(t *testing.T) {
	content := map[string]appBody{
		"app1": {Profiles: map[string]map[string]any{
			"default": {"key": "value"},
		}},
	}
	got, _ := resolveSnapshot(content, "app1", "prod")
	if got["key"] != "value" {
		t.Errorf("got %v, want default value passed through", got)
	}
}

// TestResolveSnapshot_MissingApp proves the operator-error path:
// asking for an app the source doesn't expose returns a clear
// error, not silent empty values.
func TestResolveSnapshot_MissingApp(t *testing.T) {
	_, err := resolveSnapshot(map[string]appBody{}, "unknown", "prod")
	if err == nil {
		t.Error("resolveSnapshot should error on missing app")
	}
}

// TestSnapshotVersion_StableAcrossRuns proves the version stamp
// is content-addressed: two equal value trees produce the same
// version, two different trees produce different versions. This
// is what makes polling-based refresh work (clients compare
// versions to short-circuit a full fetch).
func TestSnapshotVersion_StableAcrossRuns(t *testing.T) {
	v1, _ := snapshotVersion(map[string]any{"a": 1, "b": "two"})
	v2, _ := snapshotVersion(map[string]any{"b": "two", "a": 1}) // same content, different map iteration
	if v1 != v2 {
		t.Errorf("version not stable across map orderings: %q vs %q", v1, v2)
	}
	v3, _ := snapshotVersion(map[string]any{"a": 1, "b": "different"})
	if v1 == v3 {
		t.Errorf("version not sensitive to value change")
	}
}
