package nexus

import (
	"context"
	"testing"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/paulmanoni/nexus/extension/cache"
)

type testCacheHandle struct{ *cache.Manager }

func TestCache_BadTypePanicsAtWiring(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for T without embedded *cache.Manager")
		}
	}()
	type bad struct{ X int }
	_ = Cache[bad]("x", func() *cache.Config { return cache.NewConfig() })
}

// TestCache_WiresInjectableHandle runs Cache[T] through a real fx graph
// with a memory-backed cache (no Redis needed).
func TestCache_WiresInjectableHandle(t *testing.T) {
	app := New(Config{})

	var got *testCacheHandle
	opt := Cache[testCacheHandle]("session", func() *cache.Config {
		return cache.NewConfig() // memory store, no Redis
	}, WithCacheDefault())

	fxapp := fx.New(
		fx.Supply(app),
		fx.Supply(zap.NewNop()),
		opt.nexusOption(),
		fx.Populate(&got),
	)
	ctx := context.Background()
	if err := fxapp.Start(ctx); err != nil {
		t.Fatalf("fx start: %v", err)
	}
	defer func() { _ = fxapp.Stop(ctx) }()

	if got == nil || got.Manager == nil {
		t.Fatal("*testCacheHandle / embedded *cache.Manager not set")
	}
	// promoted method from *cache.Manager works (memory store ⇒ not redis)
	if got.IsRedisConnected() {
		t.Error("expected memory store (IsRedisConnected=false) in test")
	}
}
