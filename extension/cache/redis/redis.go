// Package redis is the opt-in Redis backend for the nexus cache. Blank-import
// it to enable Redis (the database/sql driver pattern):
//
//	import _ "github.com/paulmanoni/nexus/extension/cache/redis"
//
// Its init() registers a supervisor factory with package cache. After that, a
// cache.Manager whose Config.Environment is "production" keeps a Redis
// connection (with retry + circuit breaker) and transparently fails over to
// the in-memory tier on outage, reconnecting on a 30s tick. Importing this
// package is the ONLY thing that links go-redis — a memory-only app never
// pays for it.
package redis

import (
	"context"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	retryPolicy "github.com/failsafe-go/failsafe-go/retrypolicy"
	"github.com/redis/go-redis/v9"
	"github.com/vmihailenco/msgpack/v5"
	"go.uber.org/zap"

	"github.com/paulmanoni/nexus/extension/cache"
)

func init() {
	cache.RegisterRedis(func(m *cache.Manager) cache.RedisSupervisor {
		return &supervisor{m: m}
	})
}

// backend implements cache.Backend over a live *redis.Client, serializing
// values with msgpack to match the memory tier's value semantics.
type backend struct{ client *redis.Client }

func (b *backend) Get(ctx context.Context, key string, out any) error {
	data, err := b.client.Get(ctx, key).Bytes()
	if err != nil {
		if err == redis.Nil {
			return cache.ErrNotFound
		}
		return err
	}
	return msgpack.Unmarshal(data, out)
}

func (b *backend) Set(ctx context.Context, key string, value any, ttl time.Duration) error {
	data, err := msgpack.Marshal(value)
	if err != nil {
		return err
	}
	return b.client.Set(ctx, key, data, ttl).Err()
}

func (b *backend) Delete(ctx context.Context, key string) error {
	return b.client.Del(ctx, key).Err()
}

func (b *backend) Clear(ctx context.Context) error {
	return b.client.FlushDB(ctx).Err()
}

// supervisor owns the connect + reconnect/health loop and swaps the Manager's
// active backend via the cache.Manager hooks. It's the moved-out equivalent
// of the old Manager.connectToRedis / maintainConnection / checkRedisConnection
// / switchToMemoryCache methods.
type supervisor struct {
	m      *cache.Manager
	client *redis.Client
}

func (s *supervisor) executor() failsafe.Executor[*redis.Client] {
	log := s.m.Logger()
	// Only 2 retries with a tight backoff on the initial connect — boot
	// shouldn't spend minutes thrashing against a down Redis. The long-lived
	// reconnect loop picks up availability on its ReconnectInterval cadence.
	retry := retryPolicy.NewBuilder[*redis.Client]().
		WithDelay(500*time.Millisecond).
		WithBackoff(2, 2*time.Second).
		WithMaxRetries(2).
		WithJitter(25).
		OnRetry(func(e failsafe.ExecutionEvent[*redis.Client]) {
			log.Warn("redis connect retrying",
				zap.Int("attempt", e.Attempts()), zap.Error(e.LastError()))
		}).Build()
	cb := circuitbreaker.NewBuilder[*redis.Client]().
		WithFailureThreshold(5).
		WithDelay(10 * time.Second).
		WithSuccessThreshold(1).
		Build()
	return failsafe.With[*redis.Client](retry, cb)
}

// Start launches the reconnect/health loop in the background and fires the
// first connect attempt immediately so a reachable Redis is picked up without
// waiting a full interval. Callers never block on Redis availability.
func (s *supervisor) Start() { go s.maintain() }

// Stop closes the live client (if any). The loop itself unwinds when the
// Manager context is cancelled (Manager.Stop does that).
func (s *supervisor) Stop() {
	if s.client != nil {
		_ = s.client.Close()
		s.client = nil
	}
}

func (s *supervisor) maintain() {
	cfg := s.m.Config()
	interval := cfg.ReconnectInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	s.connect()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.m.Context().Done():
			s.m.Logger().Info("cache: reconnect loop stopping")
			return
		case <-ticker.C:
			if !s.m.IsRedisConnected() {
				s.m.Logger().Info("cache: redis disconnected, retrying")
				s.connect()
			} else {
				s.healthCheck()
			}
		}
	}
}

func (s *supervisor) connect() {
	cfg := s.m.Config()
	log := s.m.Logger()
	client, err := s.executor().Get(func() (*redis.Client, error) {
		c := redis.NewClient(&redis.Options{
			Addr:     cfg.RedisAddress(),
			Password: cfg.RedisPassword,
			DB:       cfg.RedisDB,
			// Keep the pool tight on failed attempts so its own reconnect
			// loop doesn't spam pool.go logs between our retry ticks.
			MaxRetries: -1,
		})
		ctx, cancel := context.WithTimeout(context.Background(), cfg.ConnectTimeout)
		defer cancel()
		if _, err := c.Ping(ctx).Result(); err != nil {
			// Close the client so its background pool stops dialing —
			// otherwise the orphaned goroutines keep logging until GC.
			_ = c.Close()
			log.Error("cache: redis ping failed", zap.Error(err))
			return nil, err
		}
		return c, nil
	})
	if err != nil {
		log.Error("cache: redis connect failed, staying on memory", zap.Error(err))
		return
	}
	s.client = client
	s.m.ActivateRedis(&backend{client: client})
}

func (s *supervisor) healthCheck() {
	client := s.client
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// PING is the canonical Redis liveness probe: no pre-existing state,
	// clean error only on transport failure.
	if _, err := client.Ping(ctx).Result(); err != nil {
		s.m.Logger().Error("cache: health check failed, switching to memory", zap.Error(err))
		_ = client.Close()
		s.client = nil
		s.m.FallBackToMemory()
	}
}
