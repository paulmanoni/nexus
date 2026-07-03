// Package cache provides a cache for nexus apps. The default Manager is
// in-memory only and pulls NO heavy dependencies (no Redis client, no
// gocache, no Prometheus) — values are stored in an in-process TTL map
// (patrickmn/go-cache) and serialized with msgpack.
//
// Redis is opt-in, database/sql-style: blank-import the redis backend and
// it registers itself, after which a Manager in "production" mode keeps a
// Redis connection and transparently fails over to memory on outage:
//
//	import (
//	    "github.com/paulmanoni/nexus/extension/cache"
//	    _ "github.com/paulmanoni/nexus/extension/cache/redis" // enable Redis
//	)
//
// Without that import the binary never links go-redis. Typical wiring with
// fx:
//
//	fx.New(
//	    fx.Provide(zap.NewExample),
//	    cache.Module,                 // provides *cache.Manager + *cache.Config
//	    fx.Invoke(func(app *nexus.App, m *cache.Manager) {
//	        app.Register(m.AsResource("session-cache", "Hybrid redis/memory"))
//	    }),
//	)
//
// Without fx, call NewConfig() + NewManager(cfg, logger) and Start().
package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	gocache "github.com/patrickmn/go-cache"
	"github.com/vmihailenco/msgpack/v5"
	"go.uber.org/zap"

	"github.com/paulmanoni/nexus/resource"
)

// ErrNotFound is returned by Get when the key is absent (or the cache isn't
// initialized). Distinct so callers can branch on a miss vs a real error.
var ErrNotFound = errors.New("cache: key not found")

// Config holds cache configuration. Populate via NewConfig (env-driven) or
// construct directly.
type Config struct {
	// Environment controls Redis behavior. "production" attempts Redis and
	// keeps reconnecting; anything else stays on memory. Redis only ever
	// engages when the redis backend is registered (blank-import
	// extension/cache/redis); otherwise this is a no-op and the Manager is
	// memory-only regardless of Environment.
	Environment string

	RedisHost     string
	RedisPort     string
	RedisPassword string
	RedisDB       int

	// DefaultExpiry is the in-memory store's default TTL. CleanupExpiry is
	// its GC tick.
	DefaultExpiry time.Duration
	CleanupExpiry time.Duration

	// ConnectTimeout caps the initial Redis ping during connect attempts.
	ConnectTimeout time.Duration

	// ReconnectInterval controls how often the manager retries Redis when
	// it's down. 0 defaults to 30s.
	ReconnectInterval time.Duration

	// PersistPath, when non-empty, makes the in-memory store survive
	// process restarts: Start tries LoadFile, Stop calls SaveFile.
	// Aimed at `nexus dev`'s auto-restart loop so auth tokens (or
	// any cached state) don't evaporate on every Go save. Redis still
	// wins when reachable; this is the memory-only path's escape
	// hatch. Set explicitly, or let NewConfig auto-pick
	// ".nexus/dev-cache.gob" when NEXUS_DEV=1.
	PersistPath string
}

// NewConfig builds a Config from env vars: APP_ENV, REDIS_HOST, REDIS_PORT,
// REDIS_PASSWORD, NEXUS_DEV_CACHE_FILE. Defaults: env=development,
// host=localhost, port=6379, db=0, 15m/10m expiries, 5s connect timeout,
// 30s reconnect. PersistPath auto-defaults to ".nexus/dev-cache.gob" when
// NEXUS_DEV=1 (set by `nexus dev`) so tokens + cached state survive the
// auto-restart loop without anything to wire by hand.
func NewConfig() *Config {
	env := os.Getenv("APP_ENV")
	if env == "" {
		env = "development"
	}
	persistPath := os.Getenv("NEXUS_DEV_CACHE_FILE")
	if persistPath == "" && os.Getenv("NEXUS_DEV") == "1" {
		persistPath = ".nexus/dev-cache.gob"
	}
	return &Config{
		Environment:       env,
		RedisHost:         os.Getenv("REDIS_HOST"),
		RedisPort:         os.Getenv("REDIS_PORT"),
		RedisPassword:     os.Getenv("REDIS_PASSWORD"),
		RedisDB:           0,
		DefaultExpiry:     15 * time.Minute,
		CleanupExpiry:     10 * time.Minute,
		ConnectTimeout:    5 * time.Second,
		ReconnectInterval: 30 * time.Second,
		PersistPath:       persistPath,
	}
}

// RedisAddress returns host:port, filling in localhost:6379 when blank.
func (c *Config) RedisAddress() string {
	host := c.RedisHost
	if host == "" {
		host = "localhost"
	}
	port := c.RedisPort
	if port == "" {
		port = "6379"
	}
	return host + ":" + port
}

// Backend is a cache storage tier. The memory backend (default) and the
// optional Redis backend (extension/cache/redis) both implement it. Values
// are passed already-typed; an implementation is responsible for its own
// serialization. Get returns ErrNotFound (or any non-nil error) on a miss.
type Backend interface {
	Get(ctx context.Context, key string, out any) error
	Set(ctx context.Context, key string, value any, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
	Clear(ctx context.Context) error
}

// RedisSupervisor is the lifecycle the optional Redis backend installs.
// Start kicks off the connect + reconnect loop (swapping the Manager's
// active backend via ActivateRedis / FallBackToMemory); Stop tears it down.
type RedisSupervisor interface {
	Start()
	Stop()
}

// newRedisSupervisor is set by extension/cache/redis's init via RegisterRedis.
// nil means the Redis backend was never imported — the Manager stays
// memory-only and never links go-redis.
var newRedisSupervisor func(*Manager) RedisSupervisor

// RegisterRedis installs the Redis supervisor factory. Called from
// extension/cache/redis's init(), so a blank import is all an app needs to
// enable Redis (the database/sql driver pattern). Not part of the surface
// apps call directly.
func RegisterRedis(f func(*Manager) RedisSupervisor) { newRedisSupervisor = f }

// Manager is the live cache. Get/Set/Delete are safe for concurrent use; the
// active backend flips between Redis and memory atomically under a mutex when
// the Redis supervisor reports a connectivity change.
type Manager struct {
	config *Config
	logger *zap.Logger

	mu     sync.RWMutex
	mem    *memoryBackend // always present
	active Backend        // mem by default; the supervisor swaps in Redis

	sup              RedisSupervisor
	isRedisConnected bool

	ctx    context.Context
	cancel context.CancelFunc
}

// NewManager constructs a memory-backed Manager. When the Redis backend is
// registered (blank-import extension/cache/redis) and Environment is
// "production", Start() launches the async connect + reconnect loop — an
// unreachable Redis never delays boot; the Manager serves from memory until
// Redis comes up, then flips atomically.
func NewManager(cfg *Config, logger *zap.Logger) *Manager {
	if logger == nil {
		logger = zap.NewNop()
	}
	ctx, cancel := context.WithCancel(context.Background())
	mem := &memoryBackend{c: gocache.New(cfg.DefaultExpiry, cfg.CleanupExpiry)}
	return &Manager{
		config: cfg,
		logger: logger,
		mem:    mem,
		active: mem,
		ctx:    ctx,
		cancel: cancel,
	}
}

// Config, Logger, and Context expose the bits the Redis supervisor needs
// without it reaching into Manager internals.
func (m *Manager) Config() *Config          { return m.config }
func (m *Manager) Logger() *zap.Logger      { return m.logger }
func (m *Manager) Context() context.Context { return m.ctx }

// ActivateRedis swaps the active backend to b and marks Redis connected.
// Called by the supervisor on a successful (re)connect.
func (m *Manager) ActivateRedis(b Backend) {
	m.mu.Lock()
	m.active = b
	m.isRedisConnected = true
	m.mu.Unlock()
	m.logger.Info("cache: switched to redis")
}

// FallBackToMemory reverts the active backend to the in-memory store.
// Called by the supervisor when Redis goes away.
func (m *Manager) FallBackToMemory() {
	m.mu.Lock()
	if !m.isRedisConnected {
		m.mu.Unlock()
		return
	}
	m.isRedisConnected = false
	m.active = m.mem
	m.mu.Unlock()
	m.logger.Warn("cache: redis unavailable, switched to memory")
}

// Start restores the persist file (if any) and, when the Redis backend is
// registered and Environment is "production", launches the supervisor. Safe
// to call once.
func (m *Manager) Start() {
	m.loadPersistFile()
	if m.config.Environment != "production" || newRedisSupervisor == nil {
		return
	}
	m.sup = newRedisSupervisor(m)
	m.sup.Start()
}

// Stop snapshots the persist file (if any), stops the supervisor, and
// cancels the manager context. Idempotent.
func (m *Manager) Stop() {
	m.savePersistFile()
	if m.sup != nil {
		m.sup.Stop()
	}
	m.cancel()
}

// loadPersistFile best-effort restores the in-memory store from PersistPath.
// Silent skip when no path is set or the file doesn't yet exist; logs other
// errors but doesn't fail boot — a corrupt dev cache shouldn't block the
// binary from coming up.
func (m *Manager) loadPersistFile() {
	if m.config.PersistPath == "" {
		return
	}
	err := m.mem.c.LoadFile(m.config.PersistPath)
	switch {
	case err == nil:
		m.logger.Info("cache: restored from disk", zap.String("path", m.config.PersistPath))
	case os.IsNotExist(err):
		// First boot — nothing to load. Stay silent.
	default:
		m.logger.Warn("cache: load persist file failed",
			zap.String("path", m.config.PersistPath), zap.Error(err))
	}
}

// savePersistFile writes the in-memory store to PersistPath, creating the
// parent dir if needed. Failures are logged but never returned — Stop's
// contract is best-effort cleanup.
func (m *Manager) savePersistFile() {
	if m.config.PersistPath == "" {
		return
	}
	if dir := filepath.Dir(m.config.PersistPath); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	if err := m.mem.c.SaveFile(m.config.PersistPath); err != nil {
		m.logger.Warn("cache: save persist file failed",
			zap.String("path", m.config.PersistPath), zap.Error(err))
		return
	}
	m.logger.Info("cache: snapshot written", zap.String("path", m.config.PersistPath))
}

// IsRedisConnected reports whether Redis is the currently active store.
func (m *Manager) IsRedisConnected() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.isRedisConnected
}

func (m *Manager) backend() Backend {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active
}

// Get deserializes the cached value under key into out. Returns ErrNotFound
// when the key is missing.
func (m *Manager) Get(ctx context.Context, key string, out any) error {
	return m.backend().Get(ctx, key, out)
}

// Set stores value under key with the given TTL.
func (m *Manager) Set(ctx context.Context, key string, value any, ttl time.Duration) error {
	return m.backend().Set(ctx, key, value, ttl)
}

// Delete removes key from the active store.
func (m *Manager) Delete(ctx context.Context, key string) error {
	return m.backend().Delete(ctx, key)
}

// Clear wipes every key in the active store.
func (m *Manager) Clear(ctx context.Context) error {
	return m.backend().Clear(ctx)
}

// AsResource builds a nexus resource.Resource for this Manager. Backend
// ("redis" vs "memory") is reported live via WithDetails.
func (m *Manager) AsResource(name, description string, opts ...resource.Option) resource.Resource {
	base := []resource.Option{
		resource.WithDetails(func() map[string]any {
			backend := "memory"
			if m.IsRedisConnected() {
				backend = "redis"
			}
			return map[string]any{
				"backend": backend,
				"env":     m.config.Environment,
				"address": m.config.RedisAddress(),
			}
		}),
	}
	return resource.NewCache(name, description, nil, func() bool { return true }, append(base, opts...)...)
}

// --- memory backend --------------------------------------------------------

// memoryBackend is the default, dependency-light tier: an in-process TTL map
// (patrickmn/go-cache) holding msgpack-serialized values. Serializing (rather
// than storing values by reference) preserves value semantics — a cached
// struct mutated by the caller after Set doesn't change the cached copy —
// matching the Redis backend's behavior.
type memoryBackend struct{ c *gocache.Cache }

func (b *memoryBackend) Get(_ context.Context, key string, out any) error {
	v, ok := b.c.Get(key)
	if !ok {
		return ErrNotFound
	}
	data, ok := v.([]byte)
	if !ok {
		return errors.New("cache: corrupt memory entry")
	}
	return msgpack.Unmarshal(data, out)
}

func (b *memoryBackend) Set(_ context.Context, key string, value any, ttl time.Duration) error {
	data, err := msgpack.Marshal(value)
	if err != nil {
		return err
	}
	// ttl <= 0 → use the store's default expiry (gocache.DefaultExpiration).
	if ttl <= 0 {
		ttl = gocache.DefaultExpiration
	}
	b.c.Set(key, data, ttl)
	return nil
}

func (b *memoryBackend) Delete(_ context.Context, key string) error {
	b.c.Delete(key)
	return nil
}

func (b *memoryBackend) Clear(_ context.Context) error {
	b.c.Flush()
	return nil
}
