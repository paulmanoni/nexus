package redis

import (
	"context"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/extension/cache"
)

type payload struct{ Count int }

// TestBlankImportRegistersRedis verifies the database/sql-style wiring: simply
// importing this package (which this test does) registers the supervisor with
// package cache, so a production Manager now ATTEMPTS Redis. We point it at a
// refused address so the attempt fails fast and the Manager falls back to
// memory — proving (a) registration happened (the supervisor ran) and (b)
// failover still serves from memory without a Redis server.
func TestBlankImportRegistersRedis(t *testing.T) {
	m := cache.NewManager(&cache.Config{
		Environment:       "production",
		RedisHost:         "127.0.0.1",
		RedisPort:         "1", // connection refused → fast, deterministic failure
		DefaultExpiry:     time.Minute,
		ConnectTimeout:    100 * time.Millisecond,
		ReconnectInterval: time.Hour, // don't retry during the test
	}, nil)

	m.Start() // launches the supervisor; the connect attempt fails and logs
	defer m.Stop()

	// Give the async connect attempt a moment to run and fall back.
	time.Sleep(50 * time.Millisecond)

	if m.IsRedisConnected() {
		t.Fatal("did not expect a Redis connection to a refused address")
	}

	// Memory tier still works through the same Manager.
	ctx := context.Background()
	if err := m.Set(ctx, "k", payload{Count: 9}, time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var got payload
	if err := m.Get(ctx, "k", &got); err != nil || got.Count != 9 {
		t.Errorf("memory fallback broken: got %+v err %v", got, err)
	}
}
