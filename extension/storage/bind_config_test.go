package storage

import (
	"testing"

	"github.com/paulmanoni/nexus"
)

// TestConfigFromTOML reads a [storage.<name>] block into a Config; unseeded
// fields default to their zero value.
func TestConfigFromTOML(t *testing.T) {
	nexus.ClearConfigStoreForTest()
	t.Cleanup(nexus.ClearConfigStoreForTest)
	nexus.InstallConfigStore(map[string]any{
		"storage": map[string]any{
			"uploads": map[string]any{
				"driver":     "s3",
				"bucket":     "my-bucket",
				"region":     "us-east-1",
				"path_style": true,
				"access_key": "AK",
			},
		},
	}, "test")

	c := configFromTOML("uploads")
	if c.Driver != "s3" || c.Bucket != "my-bucket" || c.Region != "us-east-1" {
		t.Fatalf("unexpected config: %+v", c)
	}
	if !c.PathStyle {
		t.Errorf("PathStyle not read from block")
	}
	if c.AccessKey != "AK" {
		t.Errorf("AccessKey = %q, want AK", c.AccessKey)
	}
	if c.Root != "" {
		t.Errorf("Root should default empty, got %q", c.Root)
	}
}
