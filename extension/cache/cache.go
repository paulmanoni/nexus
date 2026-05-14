// Package cache provides a Redis + in-memory hybrid cache for nexus apps,
// ported from the oats_applicant implementation. A Manager always has the
// in-memory store ready; in "production" mode it also tries to keep a Redis
// connection, falling back to memory on outage and reconnecting on a 30s
// tick.
//
// Typical wiring with fx:
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

	gcache "github.com/eko/gocache/lib/v4/cache"
	"github.com/eko/gocache/lib/v4/marshaler"
	"github.com/eko/gocache/lib/v4/store"
	gocache_store "github.com/eko/gocache/store/go_cache/v4"
	redis_store "github.com/eko/gocache/store/redis/v4"
	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	retryPolicy "github.com/failsafe-go/failsafe-go/retrypolicy"
	gocache "github.com/patrickmn/go-cache"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/paulmanoni/nexus/resource"
)

// Config holds cache configuration. Populate via NewConfig (env-driven) or
// construct directly.
type Config struct {
	// Environment controls Redis behavior. "production" attempts Redis and
	// keeps reconnecting; anything else stays on memory.
	Environment string

	RedisHost     string
	RedisPort     string
	RedisPassword string
	RedisDB       int

	// DefaultExpiry is go-cache's default TTL. CleanupExpiry is its GC tick.
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

// Manager is the live cache. Its Get/Set/Delete are safe for concurrent use;
// the underlying store flips between Redis and memory atomically under a
// mutex when connectivity changes.
type Manager struct {
	config    *Config
	logger    *zap.Logger
	marshaler *marshaler.Marshaler

	mu           sync.RWMutex
	redisStore   store.StoreInterface
	goCache      *gocache.Cache
	goCacheStore store.StoreInterface
	cacheStore   store.StoreInterface
	redisClient  *redis.Client

	executor failsafe.Executor[*redis.Client]

	ctx    context.Context
	cancel context.CancelFunc

	isRedisConnected bool
}

// NewManager constructs a Manager with the in-memory store initialized.
// Redis connection, when enabled via "production" mode, is attempted
// asynchronously in Start() so an unreachable Redis never delays boot.
// Call Start() to kick off the connect + reconnect loop.
func NewManager(cfg *Config, logger *zap.Logger) *Manager {
	if logger == nil {
		logger = zap.NewNop()
	}
	ctx, cancel := context.WithCancel(context.Background())

	// Only 2 retries with a tight backoff on the initial connect — boot
	// shouldn't spend minutes thrashing against a down Redis. The long-
	// lived reconnect loop (maintainConnection) picks up availability
	// on its ReconnectInterval cadence.
	retry := retryPolicy.NewBuilder[*redis.Client]().
		WithDelay(500*time.Millisecond).
		WithBackoff(2, 2*time.Second).
		WithMaxRetries(2).
		WithJitter(25).
		OnRetry(func(e failsafe.ExecutionEvent[*redis.Client]) {
			logger.Warn("redis connect retrying",
				zap.Int("attempt", e.Attempts()),
				zap.Error(e.LastError()),
			)
		}).Build()

	cb := circuitbreaker.NewBuilder[*redis.Client]().
		WithFailureThreshold(5).
		WithDelay(10 * time.Second).
		WithSuccessThreshold(1).
		Build()

	executor := failsafe.With[*redis.Client](retry, cb)

	goCache := gocache.New(cfg.DefaultExpiry, cfg.CleanupExpiry)
	goCacheStore := gocache_store.NewGoCache(goCache)

	m := &Manager{
		config:       cfg,
		logger:       logger,
		goCache:      goCache,
		goCacheStore: goCacheStore,
		executor:     executor,
		ctx:          ctx,
		cancel:       cancel,
	}
	m.setupMemoryCache()
	return m
}

// Start kicks off the background reconnect/health loop and fires the
// first Redis connect attempt asynchronously. Safe to call once.
//
// In addition: when PersistPath is set, restore the in-memory store
// from the gob-encoded file on disk so the cache survives a process
// restart (e.g., `nexus dev`'s auto-restart loop). Missing-file is
// the normal first-boot case and not an error. Redis still wins when
// reachable — the on-disk snapshot only seeds the memory tier.
//
// Callers never block on Redis availability — the manager serves
// from memory until Redis comes up, then flips atomically under the
// mutex.
func (m *Manager) Start() {
	m.loadPersistFile()
	if m.config.Environment != "production" {
		return
	}
	go m.maintainConnection()
}

// Stop cancels the background loop and, when PersistPath is set,
// snapshots the in-memory store to disk so the next process boot
// can pick up where this one left off. Idempotent.
func (m *Manager) Stop() {
	m.savePersistFile()
	m.cancel()
}

// loadPersistFile best-effort restores the in-memory store from
// PersistPath. Silent skip when no path is set or the file doesn't
// yet exist; logs other errors but doesn't fail boot — a corrupt
// dev cache shouldn't block the binary from coming up.
func (m *Manager) loadPersistFile() {
	if m.config.PersistPath == "" || m.goCache == nil {
		return
	}
	err := m.goCache.LoadFile(m.config.PersistPath)
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

// savePersistFile writes the in-memory store to PersistPath. Creates
// the parent directory if needed. Save failures are logged but never
// returned — Stop's contract is best-effort cleanup.
func (m *Manager) savePersistFile() {
	if m.config.PersistPath == "" || m.goCache == nil {
		return
	}
	if dir := filepath.Dir(m.config.PersistPath); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	if err := m.goCache.SaveFile(m.config.PersistPath); err != nil {
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

// Get deserializes the cached value under key into out. Returns an error if
// the key is missing or the cache isn't initialized.
func (m *Manager) Get(ctx context.Context, key string, out any) error {
	if m.marshaler == nil {
		return errors.New("cache: not initialized")
	}
	_, err := m.marshaler.Get(ctx, key, out)
	return err
}

// Set stores value under key with the given TTL.
func (m *Manager) Set(ctx context.Context, key string, value any, ttl time.Duration) error {
	if m.marshaler == nil {
		return errors.New("cache: not initialized")
	}
	return m.marshaler.Set(ctx, key, value, store.WithExpiration(ttl))
}

// Delete removes key from the active store.
func (m *Manager) Delete(ctx context.Context, key string) error {
	if m.marshaler == nil {
		return errors.New("cache: not initialized")
	}
	return m.marshaler.Delete(ctx, key)
}

// Clear wipes every key in the active store.
func (m *Manager) Clear(ctx context.Context) error {
	if m.marshaler == nil {
		return errors.New("cache: not initialized")
	}
	return m.marshaler.Clear(ctx)
}

// AsResource builds a nexus resource.Resource for this Manager. Mark it as
// default with extra options passed through. Backend ("redis" vs "memory")
// is reported live via WithDetails.
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

// --- internals -------------------------------------------------------------

func (m *Manager) setupMemoryCache() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cacheStore = m.goCacheStore
	m.marshaler = marshaler.New(gcache.New[any](m.goCacheStore))
	m.logger.Info("cache: using memory store")
}

func (m *Manager) connectToRedis() {
	client, err := m.executor.Get(func() (*redis.Client, error) {
		c := redis.NewClient(&redis.Options{
			Addr:     m.config.RedisAddress(),
			Password: m.config.RedisPassword,
			DB:       m.config.RedisDB,
			// Keep the pool tight on failed attempts so its own reconnect
			// loop doesn't spam pool.go logs between our retry ticks.
			MaxRetries: -1,
		})
		ctx, cancel := context.WithTimeout(context.Background(), m.config.ConnectTimeout)
		defer cancel()
		if _, err := c.Ping(ctx).Result(); err != nil {
			// Close the client so its background pool stops dialing —
			// otherwise the orphaned goroutines keep logging until GC.
			_ = c.Close()
			m.logger.Error("cache: redis ping failed", zap.Error(err))
			return nil, err
		}
		return c, nil
	})
	if err != nil {
		m.logger.Error("cache: redis connect failed, staying on memory", zap.Error(err))
		return
	}
	m.mu.Lock()
	m.redisClient = client
	m.redisStore = redis_store.NewRedis(client)
	m.cacheStore = m.redisStore
	m.marshaler = marshaler.New(gcache.New[any](m.redisStore))
	m.isRedisConnected = true
	m.mu.Unlock()
	m.logger.Info("cache: switched to redis")
}

func (m *Manager) switchToMemoryCache() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.isRedisConnected {
		return
	}
	m.isRedisConnected = false
	m.cacheStore = m.goCacheStore
	m.marshaler = marshaler.New(gcache.New[any](m.goCacheStore))
	// Close the stale client so its background pool stops dialing; a
	// fresh client will be built on the next connectToRedis attempt.
	if m.redisClient != nil {
		_ = m.redisClient.Close()
		m.redisClient = nil
		m.redisStore = nil
	}
	m.logger.Warn("cache: redis unavailable, switched to memory")
}

func (m *Manager) maintainConnection() {
	interval := m.config.ReconnectInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	// Kick off the first connect attempt immediately so a reachable Redis
	// is picked up without waiting a full ReconnectInterval. Runs in this
	// goroutine — the caller already launched us in the background.
	m.connectToRedis()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			m.logger.Info("cache: reconnect loop stopping")
			return
		case <-ticker.C:
			if !m.IsRedisConnected() {
				m.logger.Info("cache: redis disconnected, retrying")
				m.connectToRedis()
			} else {
				m.checkRedisConnection()
			}
		}
	}
}

func (m *Manager) checkRedisConnection() {
	m.mu.RLock()
	client := m.redisClient
	m.mu.RUnlock()
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// PING the redis client directly. The previous implementation did
	// Get("__nexus_cache_healthcheck__") which returned "value not
	// found" on every tick (the key was never set), and the code
	// couldn't distinguish that from a real transport error — so it
	// flapped: switch to memory → reconnect succeeds → next tick
	// health-checks a missing key → switch to memory → repeat.
	//
	// PING is the canonical Redis liveness probe: it requires no
	// pre-existing state and returns a clean error only on transport
	// failure. Same call connectToRedis already uses for the initial
	// dial.
	if _, err := client.Ping(ctx).Result(); err != nil {
		m.logger.Error("cache: health check failed, switching to memory", zap.Error(err))
		m.switchToMemoryCache()
	}
}
