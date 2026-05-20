package nexus

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// configStore is the process-wide config singleton. nexus.Get
// reads from it via the activeConfigStore pointer installed by
// the config extension's Server / Client / Local entrypoints at
// boot. Reads are lock-free via atomic.Pointer; writes
// (server-pushed refreshes, source reloads) atomic-swap the
// whole snapshot so readers never see a torn view.
type configStore struct {
	snap atomic.Pointer[configSnap]

	mu        sync.Mutex
	listeners map[string][]func(any)
}

// configSnap is the immutable value tree readers walk. Each
// refresh produces a fresh snapshot; the old one is discarded
// once no reader still holds it.
type configSnap struct {
	values    map[string]any
	version   string
	updatedAt time.Time
}

// activeConfigStore is the package-level handle nexus.Get
// reaches into. Set once by the config extension at boot.
var activeConfigStore atomic.Pointer[configStore]

// pendingConfigListeners holds OnConfigChange callbacks
// registered before any config entrypoint installed the store.
// InstallConfigStore replays them once it lands.
var (
	pendingConfigMu        sync.Mutex
	pendingConfigListeners = map[string][]func(any){}
)

// InstallConfigStore installs the process-wide config store with
// the given initial values. Called once by extension/config at
// boot. Multiple installations in one process is a configuration
// error caught here (the second installer panics with a clear
// message).
//
// Public so extension/config can call it across package boundaries.
// User code does NOT call this directly — the extension's
// Server/Client/Local entrypoints drive it.
func InstallConfigStore(values map[string]any, version string) {
	s := &configStore{listeners: map[string][]func(any){}}
	s.snap.Store(&configSnap{
		values:    values,
		version:   version,
		updatedAt: time.Now().UTC(),
	})
	if !activeConfigStore.CompareAndSwap(nil, s) {
		panic("nexus: multiple config.Server/Client/Local entrypoints in one process — pick one")
	}
	// Replay any pre-boot OnConfigChange callbacks.
	pendingConfigMu.Lock()
	for key, cbs := range pendingConfigListeners {
		s.mu.Lock()
		s.listeners[key] = append(s.listeners[key], cbs...)
		s.mu.Unlock()
	}
	pendingConfigListeners = map[string][]func(any){}
	pendingConfigMu.Unlock()
}

// UpdateConfigStore swaps in a new value tree + version,
// triggering OnConfigChange callbacks for keys whose values
// changed. Called by config.Client on every successful refresh
// and by config.Local's reload path (phase 2).
func UpdateConfigStore(values map[string]any, version string) {
	s := activeConfigStore.Load()
	if s == nil {
		// Caller hasn't installed yet — race during boot.
		// Fall through to install instead so the first update
		// lands as the initial snapshot.
		InstallConfigStore(values, version)
		return
	}
	prev := s.snap.Load()
	next := &configSnap{
		values:    values,
		version:   version,
		updatedAt: time.Now().UTC(),
	}
	s.snap.Store(next)
	// Fire change callbacks for keys whose value differs from
	// the previous snapshot.
	s.mu.Lock()
	keys := make([]string, 0, len(s.listeners))
	for k := range s.listeners {
		keys = append(keys, k)
	}
	s.mu.Unlock()
	for _, k := range keys {
		oldV, _ := configResolvePath(prev.values, k)
		newV, _ := configResolvePath(next.values, k)
		if !configEqual(oldV, newV) {
			s.mu.Lock()
			cbs := append([]func(any){}, s.listeners[k]...)
			s.mu.Unlock()
			for _, cb := range cbs {
				cb(newV)
			}
		}
	}
}

// ClearConfigStoreForTest unwinds InstallConfigStore. Test-only
// escape hatch — production never calls this.
func ClearConfigStoreForTest() {
	activeConfigStore.Store(nil)
	pendingConfigMu.Lock()
	pendingConfigListeners = map[string][]func(any){}
	pendingConfigMu.Unlock()
}

// ConfigVersion returns the version stamp of the current
// snapshot, or "" when no store is installed. Useful for log
// breadcrumbs ("starting on config version X").
func ConfigVersion() string {
	s := activeConfigStore.Load()
	if s == nil {
		return ""
	}
	cur := s.snap.Load()
	if cur == nil {
		return ""
	}
	return cur.version
}

// --- internals ---

func configResolveKey(key string) (any, bool) {
	// ENV override always wins.
	if raw, ok := configEnvOverride(key); ok {
		return raw, true
	}
	s := activeConfigStore.Load()
	if s == nil {
		return nil, false
	}
	cur := s.snap.Load()
	if cur == nil {
		return nil, false
	}
	return configResolvePath(cur.values, key)
}

func configResolvePath(tree map[string]any, key string) (any, bool) {
	if key == "" {
		return tree, true
	}
	parts := configSplitDotted(key)
	var cur any = tree
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := m[p]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

func configSplitDotted(key string) []string {
	n := 1
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			n++
		}
	}
	out := make([]string, 0, n)
	start := 0
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			out = append(out, key[start:i])
			start = i + 1
		}
	}
	out = append(out, key[start:])
	return out
}

func configEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, va := range av {
			vb, ok := bv[k]
			if !ok || !configEqual(va, vb) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !configEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	}
	// Fall back to a JSON-string compare for richer types.
	abytes, _ := json.Marshal(a)
	bbytes, _ := json.Marshal(b)
	return string(abytes) == string(bbytes)
}

// configEnvOverride looks up an environment variable matching the
// dotted key (dots → underscores, ASCII letters uppercased).
//
//	config.api.timeout → CONFIG_API_TIMEOUT
func configEnvOverride(key string) (string, bool) {
	envKey := configKeyToEnv(key)
	v, ok := configLookupEnv(envKey)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

func configKeyToEnv(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c == '.':
			b.WriteByte('_')
		case c >= 'a' && c <= 'z':
			b.WriteByte(c - 'a' + 'A')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// configLookupEnv is injectable for tests. Defaults to
// os.LookupEnv; tests can swap in a fake map without touching
// the OS-level environment.
var configLookupEnv = os.LookupEnv

// subscribeConfig registers an OnConfigChange callback. Used by
// the public OnConfigChange function in config_get.go.
func subscribeConfig(key string, fn func(any)) {
	s := activeConfigStore.Load()
	if s == nil {
		pendingConfigMu.Lock()
		pendingConfigListeners[key] = append(pendingConfigListeners[key], fn)
		pendingConfigMu.Unlock()
		return
	}
	s.mu.Lock()
	s.listeners[key] = append(s.listeners[key], fn)
	s.mu.Unlock()
}

// configConvertAny converts a value pulled from the snapshot
// tree (a generic any from a map walk) into T. JSON round-trip
// handles primitives, slices, structs, time.Duration strings
// uniformly. ENV string values go through configConvertString
// instead.
func configConvertAny[T any](raw any) (T, error) {
	var zero T
	if v, ok := raw.(T); ok {
		return v, nil
	}
	// time.Duration as YAML string ("5s", "200ms").
	if _, isDur := any(zero).(time.Duration); isDur {
		if s, ok := raw.(string); ok {
			d, err := time.ParseDuration(s)
			if err != nil {
				return zero, fmt.Errorf("duration parse: %w", err)
			}
			return any(d).(T), nil
		}
	}
	// JSON round-trip for everything else.
	body, err := json.Marshal(raw)
	if err != nil {
		return zero, err
	}
	var out T
	if err := json.Unmarshal(body, &out); err != nil {
		return zero, err
	}
	return out, nil
}

func configConvertString[T any](raw string) (T, error) {
	var zero T
	switch any(zero).(type) {
	case string:
		return any(raw).(T), nil
	case bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return zero, err
		}
		return any(b).(T), nil
	case int:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return zero, err
		}
		return any(n).(T), nil
	case int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return zero, err
		}
		return any(n).(T), nil
	case float64:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return zero, err
		}
		return any(f).(T), nil
	case time.Duration:
		d, err := time.ParseDuration(raw)
		if err != nil {
			return zero, err
		}
		return any(d).(T), nil
	}
	var out T
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return zero, fmt.Errorf("env value %q not convertible to %T", raw, zero)
	}
	return out, nil
}
