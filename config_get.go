package nexus

import (
	"encoding/json"
	"fmt"
)

// Get is the unified config accessor. Walks the active config
// store at the dotted-path key, converts to T, returns the
// result. With no default, returns T's zero value when the key
// is missing or the conversion fails. With a default, returns
// the default in those cases. At most one default is consulted.
//
//	addr := nexus.Get[string]("config.server.addr")
//	port := nexus.Get[int]("config.server.port", 8080)
//	ttl  := nexus.Get[time.Duration]("config.cache.ttl", 5*time.Minute)
//
// Resolution priority, highest first:
//
//  1. Environment variable (CONFIG_SERVER_PORT for "config.server.port")
//  2. Config-extension snapshot (config.Server / .Client / .Local)
//  3. nexus.toml base layer (seeded by LoadConfig/Boot)
//  4. The default arg (or T's zero value)
//
// Layers 2 and 3 resolve per-key: a key absent from the extension
// snapshot falls through to the nexus.toml layer rather than
// returning the default. So nexus.Get reads any key declared in
// nexus.toml — the dotted key mirrors the TOML table path, e.g.
// [runtime.storage] url → Get("runtime.storage.url") — with NO
// config extension wired. The extension snapshot (when installed)
// overrides nexus.toml for keys it carries, since it's
// runtime-managed and hot-reloadable.
//
// The nexus.toml layer lands when MustLoadConfig/LoadConfig/Boot
// runs (typically the first line of main); the extension snapshot
// lands a little later, at app start. Calls before either is
// installed return the default (or zero).
func Get[T any](key string, defaults ...T) T {
	var zero T
	pickDefault := func() T {
		if len(defaults) > 0 {
			return defaults[0]
		}
		return zero
	}

	// ENV override wins.
	if raw, ok := configEnvOverride(key); ok {
		if v, err := configConvertString[T](raw); err == nil {
			return v
		}
		return pickDefault()
	}

	raw, ok := configResolveKey(key)
	if !ok || raw == nil {
		return pickDefault()
	}
	v, err := configConvertAny[T](raw)
	if err != nil {
		return pickDefault()
	}
	return v
}

// MustGet is Get without a fallback — panics if the key is
// missing or can't be converted to T. Use when absence is a
// boot-time bug, not a runtime condition.
//
//	signKey := nexus.MustGet[string]("config.signing.key")
func MustGet[T any](key string) T {
	if raw, ok := configEnvOverride(key); ok {
		v, err := configConvertString[T](raw)
		if err != nil {
			panic(fmt.Sprintf("nexus: env %s = %q: %v", configKeyToEnv(key), raw, err))
		}
		return v
	}
	raw, ok := configResolveKey(key)
	if !ok || raw == nil {
		panic(fmt.Sprintf("nexus: config key %q not set", key))
	}
	v, err := configConvertAny[T](raw)
	if err != nil {
		panic(fmt.Sprintf("nexus: config key %q convert: %v", key, err))
	}
	return v
}

// HasConfig reports whether key resolves to a non-nil value in
// the current snapshot or as an ENV override.
func HasConfig(key string) bool {
	if _, ok := configEnvOverride(key); ok {
		return true
	}
	raw, ok := configResolveKey(key)
	return ok && raw != nil
}

// OnConfigChange registers a callback that fires when the value
// at key changes between snapshots. The callback receives the
// new value typed as any; cast to the concrete type at the
// receiver. Callbacks run synchronously on the snapshot-apply
// goroutine — keep them cheap.
//
// Use for hot-reload: feature flags, timeouts, rate limits.
// Registrations made before app start are queued and replayed
// once the store installs.
//
//	nexus.OnConfigChange("config.api.timeout", func(v any) {
//	    if d, ok := v.(time.Duration); ok { srv.timeout.Store(d) }
//	})
func OnConfigChange(key string, fn func(value any)) {
	subscribeConfig(key, fn)
}

// BindConfig decodes the snapshot subtree at prefix into dst via
// encoding/json (same shape rules as Unmarshal — tags,
// omitempty, etc.). Returns an error when the subtree shape
// doesn't match dst.
//
//	var payment PaymentConfig
//	if err := nexus.BindConfig("config.payment", &payment); err != nil { ... }
func BindConfig(prefix string, dst any) error {
	raw, ok := configResolveKey(prefix)
	if !ok || raw == nil {
		return fmt.Errorf("nexus: config prefix %q not found", prefix)
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("nexus: marshal subtree at %q: %w", prefix, err)
	}
	return json.Unmarshal(body, dst)
}
