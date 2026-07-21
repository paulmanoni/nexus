package cache

import (
	"testing"
	"time"

	"github.com/paulmanoni/nexus"
)

// TestConfigFromTOML overlays a [cache.<name>] block onto NewConfig()'s
// defaults: seeded keys win, unseeded keys keep their default.
func TestConfigFromTOML(t *testing.T) {
	nexus.ClearConfigStoreForTest()
	t.Cleanup(nexus.ClearConfigStoreForTest)
	nexus.InstallConfigStore(map[string]any{
		"cache": map[string]any{
			"session": map[string]any{
				"redis_host":     "redis.internal",
				"redis_port":     "6380",
				"redis_db":       2,
				"default_expiry": "1m",
			},
		},
	}, "test")

	c := configFromTOML("session")
	if c.RedisHost != "redis.internal" {
		t.Errorf("RedisHost = %q, want redis.internal", c.RedisHost)
	}
	if c.RedisPort != "6380" {
		t.Errorf("RedisPort = %q, want 6380", c.RedisPort)
	}
	if c.RedisDB != 2 {
		t.Errorf("RedisDB = %d, want 2", c.RedisDB)
	}
	if c.DefaultExpiry != time.Minute {
		t.Errorf("DefaultExpiry = %v, want 1m", c.DefaultExpiry)
	}
	// An unseeded key must keep NewConfig()'s default rather than zeroing.
	if c.CleanupExpiry == 0 {
		t.Errorf("CleanupExpiry should keep its NewConfig default, got 0")
	}
}
