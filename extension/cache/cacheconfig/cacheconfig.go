// Package cacheconfig declares the [cache.<name>] tables of nexus.toml.
// It is apart from extension/cache so tools that check a nexus.toml (the
// nexus CLI) know the tables without linking the cache itself.
package cacheconfig

import (
	"time"

	"github.com/paulmanoni/nexus/v2/config"
)

// Block is one [cache.<name>] block, read by cache.BindFromConfig.
type Block struct {
	Driver            string        `toml:"driver"`
	Environment       string        `toml:"environment"`
	RedisHost         string        `toml:"redis_host"`
	RedisPort         any           `toml:"redis_port" schema:"string,integer"`
	RedisPassword     string        `toml:"redis_password"`
	RedisDB           int           `toml:"redis_db"`
	DefaultExpiry     time.Duration `toml:"default_expiry"`
	CleanupExpiry     time.Duration `toml:"cleanup_expiry"`
	ConnectTimeout    time.Duration `toml:"connect_timeout"`
	ReconnectInterval time.Duration `toml:"reconnect_interval"`
	PersistPath       string        `toml:"persist_path"`
}

// Declares [cache.<name>] so nexus.toml may carry it.
var _ = config.Section[map[string]Block]("cache")
