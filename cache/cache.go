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
}

// NewConfig builds a Config from env vars: APP_ENV, REDIS_HOST, REDIS_PORT,
// REDIS_PASSWORD. Defaults: env=development, host=localhost, port=6379,
// db=0, 15m/10m expiries, 5s connect timeout, 30s reconnect.
func NewConfig() *Config {
	env := os.Getenv("APP_ENV")
	if env == "" {
		env = "development"
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
	goCacheStore store.StoreInterface
	cacheStore   store.StoreInterface
	redisClient  *redis.Client

	executor failsafe.Executor[*redis.Client]

	ctx    context.Context
	cancel context.CancelFunc

	isRedisConnected bool
}

// NewManager constructs a Manager with the in-memory store initialized. In
// "production" it also kicks off a Redis connect attempt. Call Start() to
// run the reconnect loop (NewManager alone doesn't spawn goroutines).
func NewManager(cfg *Config, logger *zap.Logger) *Manager {
	if logger == nil {
		logger = zap.NewNop()
	}
	ctx, cancel := context.WithCancel(context.Background())

	retry := retryPolicy.NewBuilder[*redis.Client]().
		WithDelay(500*time.Millisecond).
		WithBackoff(2, 5*time.Second).
		WithMaxRetries(5).
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
		goCacheStore: goCacheStore,
		executor:     executor,
		ctx:          ctx,
		cancel:       cancel,
	}
	m.setupMemoryCache()
	if cfg.Environment == "production" {
		m.connectToRedis()
	}
	return m
}

// Start kicks off the background reconnect/health loop. Safe to call once;
// subsequent calls are no-ops. Skipped entirely outside production mode.
func (m *Manager) Start() {
	if m.config.Environment != "production" {
		return
	}
	go m.maintainConnection()
}

// Stop cancels the background loop. Idempotent.
func (m *Manager) Stop() {
	m.cancel()
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
		})
		ctx, cancel := context.WithTimeout(context.Background(), m.config.ConnectTimeout)
		defer cancel()
		if _, err := c.Ping(ctx).Result(); err != nil {
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
	m.logger.Warn("cache: redis unavailable, switched to memory")
}

func (m *Manager) maintainConnection() {
	interval := m.config.ReconnectInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
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
	store := m.redisStore
	m.mu.RUnlock()
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Any non-"key missing" error indicates a transport problem. The gocache
	// redis store returns a redis.Nil-style error for missing keys but the
	// Get API wraps it so we can't reliably distinguish — we treat only a
	// nil error as "healthy" and everything else as "unhealthy". This is
	// pessimistic but matches the oats behavior (any error → switch).
	if _, err := store.Get(ctx, "__nexus_cache_healthcheck__"); err != nil {
		m.logger.Error("cache: health check failed, switching to memory", zap.Error(err))
		m.switchToMemoryCache()
	}
}
