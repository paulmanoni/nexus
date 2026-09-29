package cache

import (
	"context"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus"
	"go.uber.org/zap"
)

type fakeSupervisor struct{ started *int }

func (f fakeSupervisor) Start() { *f.started++ }
func (f fakeSupervisor) Stop()  {}

// withRedisBackend installs a fake redis backend for the test.
func withRedisBackend(t *testing.T, started *int) {
	t.Helper()
	prev := newRedisSupervisor
	t.Cleanup(func() { newRedisSupervisor = prev })
	RegisterRedis(func(*Manager) RedisSupervisor { return fakeSupervisor{started} })
}

func TestDriverDecidesRedis(t *testing.T) {
	var started int
	withRedisBackend(t, &started)
	cases := []struct {
		env, driver string
		redis       bool
	}{
		{"production", "", true},
		{"production", "auto", true},
		{"development", "", false},
		{"production", "memory", false},
		{"production", "MEMORY", false},
		{"development", "redis", true},
	}
	for _, tc := range cases {
		started = 0
		cfg := NewConfig()
		cfg.Environment, cfg.Driver, cfg.PersistPath = tc.env, tc.driver, ""
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%s/%q: %v", tc.env, tc.driver, err)
		}
		m := NewManager(cfg, nil)
		m.Start()
		m.Stop()
		if got := started == 1; got != tc.redis {
			t.Errorf("env %s driver %q: redis started = %v, want %v", tc.env, tc.driver, got, tc.redis)
		}
	}
}

func TestDriverValidate(t *testing.T) {
	prev := newRedisSupervisor
	newRedisSupervisor = nil
	defer func() { newRedisSupervisor = prev }()

	if err := (&Config{Driver: "redis"}).Validate(); err == nil || !strings.Contains(err.Error(), "extension/cache/redis") {
		t.Errorf("redis without the backend: %v", err)
	}
	if err := (&Config{Driver: "memcached"}).Validate(); err == nil || !strings.Contains(err.Error(), "unknown driver") {
		t.Errorf("unknown driver: %v", err)
	}
	if err := (&Config{Driver: "memory"}).Validate(); err != nil {
		t.Errorf("memory: %v", err)
	}
}

func TestDriverFromConfigAndEnv(t *testing.T) {
	nexus.ClearConfigStoreForTest()
	t.Cleanup(nexus.ClearConfigStoreForTest)
	nexus.InstallConfigStore(map[string]any{
		"cache": map[string]any{"session": map[string]any{"driver": "memory"}},
	}, "test")
	if got := configFromTOML("session").Driver; got != "memory" {
		t.Errorf("toml driver = %q, want memory", got)
	}
	t.Setenv("CACHE_DRIVER", "redis")
	if got := configFromTOML("other").Driver; got != "redis" {
		t.Errorf("CACHE_DRIVER default = %q, want redis", got)
	}
}

type badCache struct{ *Manager }

func TestBindRejectsBadDriver(t *testing.T) {
	_, stop, err := nexus.InProcess(nexus.Config{},
		nexus.Supply(zap.NewNop()),
		Bind[badCache]("bad", func() *Config { c := NewConfig(); c.Driver = "memcached"; return c }),
		nexus.Invoke(func(*badCache) {}),
	)
	if stop != nil {
		defer func() { _ = stop(context.Background()) }()
	}
	if err == nil || !strings.Contains(err.Error(), `cache.Bind("bad")`) {
		t.Fatalf("want a boot error naming the cache, got %v", err)
	}
}
