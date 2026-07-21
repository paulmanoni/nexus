package cache

import "github.com/paulmanoni/nexus"

// BindFromConfig binds a marker type T to the [cache.<name>] block in
// nexus.toml — the cache counterpart to db.BindFromConfig, so wiring a cache
// from config no longer means hand-writing a build closure full of
// nexus.Get calls. T must embed *Manager, exactly as with Bind. The
// framework manages lifecycle + dashboard registration identically; only
// the config source differs.
//
//	cache.BindFromConfig[SessionCache]("session", cache.WithDefault())
//
// reads the block:
//
//	[cache.session]
//	environment        = "production"   # "production" engages Redis (needs the redis backend imported)
//	redis_host         = "localhost"
//	redis_port         = "6379"
//	redis_password     = "${REDIS_PASSWORD}"
//	redis_db           = 0
//	default_expiry     = "15m"
//	cleanup_expiry     = "10m"
//	connect_timeout    = "5s"
//	reconnect_interval = "30s"
//	persist_path       = ".nexus/dev-cache.gob"
//
// Every key is optional and falls back to NewConfig()'s default, so a block
// with only redis_host/redis_port set is enough. Lifecycle options
// (WithDefault, WithDescription) stay explicit in code rather than being
// read from the block — the block describes the connection, code describes
// its role. The build runs at boot (nexus.Get resolves the toml base layer,
// any config extension, and ENV overrides), so this works under nexus.Boot.
func BindFromConfig[T any](name string, opts ...BindOption) nexus.Option {
	return Bind[T](name, func() *Config { return configFromTOML(name) }, opts...)
}

// configFromTOML overlays the [cache.<name>] block onto NewConfig()'s
// defaults: each field keeps its default unless the block overrides it.
// Durations accept toml strings ("15m"), matching nexus.Get's conversion.
func configFromTOML(name string) *Config {
	c := NewConfig()
	p := "cache." + name + "."
	c.Environment = nexus.Get(p+"environment", c.Environment)
	c.RedisHost = nexus.Get(p+"redis_host", c.RedisHost)
	c.RedisPort = nexus.Get(p+"redis_port", c.RedisPort)
	c.RedisPassword = nexus.Get(p+"redis_password", c.RedisPassword)
	c.RedisDB = nexus.Get(p+"redis_db", c.RedisDB)
	c.DefaultExpiry = nexus.Get(p+"default_expiry", c.DefaultExpiry)
	c.CleanupExpiry = nexus.Get(p+"cleanup_expiry", c.CleanupExpiry)
	c.ConnectTimeout = nexus.Get(p+"connect_timeout", c.ConnectTimeout)
	c.ReconnectInterval = nexus.Get(p+"reconnect_interval", c.ReconnectInterval)
	c.PersistPath = nexus.Get(p+"persist_path", c.PersistPath)
	return c
}
