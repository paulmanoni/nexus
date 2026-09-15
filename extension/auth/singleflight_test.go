package auth

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestResolveSingleFlight pins the stampede behavior: N concurrent
// requests bearing the same uncached token produce ONE backend
// resolve, and every caller gets the shared result.
func TestResolveSingleFlight(t *testing.T) {
	var calls atomic.Int64
	st := &moduleState{
		cache: newIdentityCache(CacheOption{TTL: time.Minute}),
	}
	sc := boundScheme{resolve: func(_ context.Context, tok string) (*Identity, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond) // hold the flight open
		return &Identity{ID: "u-" + tok}, nil
	}}

	const n = 32
	var wg sync.WaitGroup
	ids := make([]*Identity, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := st.resolveVia(context.Background(), sc, "tok-1")
			if err != nil {
				t.Errorf("resolve: %v", err)
				return
			}
			ids[i] = id
		}(i)
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("backend resolved %d times for one token, want 1", got)
	}
	for i, id := range ids {
		if id == nil || id.ID != "u-tok-1" {
			t.Fatalf("caller %d got %+v", i, id)
		}
	}

	// A waiter whose context cancels stops waiting rather than hanging.
	blocked := make(chan struct{})
	scSlow := boundScheme{resolve: func(ctx context.Context, tok string) (*Identity, error) {
		close(blocked)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	go func() { _, _ = st.resolveVia(leaderCtx, scSlow, "tok-2") }()
	<-blocked
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelWait()
	if _, err := st.resolveVia(waitCtx, scSlow, "tok-2"); err == nil {
		t.Fatal("cancelled waiter should return an error")
	}
	cancelLeader()
}
