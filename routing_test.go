package nexus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestRouting_SameKeyHitsSameReplica verifies that calls decorated
// with the same route key land on the same replica deterministically,
// while different keys spread across replicas. This is the affinity
// contract sticky-keyed workloads (per-user state) depend on.
func TestRouting_SameKeyHitsSameReplica(t *testing.T) {
	var hits [3]uint64
	mk := func(i int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddUint64(&hits[i], 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}))
	}
	srvs := [3]*httptest.Server{mk(0), mk(1), mk(2)}
	defer func() {
		for _, s := range srvs {
			s.Close()
		}
	}()

	c := NewRemoteCallerWithReplicas([]string{srvs[0].URL, srvs[1].URL, srvs[2].URL})

	// 10 calls all keyed "user-A" should land on one replica.
	keyA := "user-A"
	ctxA := WithRouteKey(context.Background(), keyA)
	for i := 0; i < 10; i++ {
		var out struct{}
		if err := c.Call(ctxA, "GET", "/x", nil, &out); err != nil {
			t.Fatalf("keyA call %d: %v", i, err)
		}
	}

	// Verify exactly one replica took every keyA call.
	nonZero := 0
	for _, h := range hits {
		if h > 0 {
			nonZero++
		}
	}
	if nonZero != 1 {
		t.Fatalf("keyA should pin to one replica; hits=%v", hits)
	}

	// Call with a different key — likely hits a different replica
	// (FNV distribution; with 3 replicas and 2 keys, very unlikely to
	// collide). Verify by checking that *some* call hits a different
	// replica than keyA's.
	keyAReplica := -1
	for i, h := range hits {
		if h > 0 {
			keyAReplica = i
			break
		}
	}

	// Drive several keys; at least one should land on a different
	// replica than keyA's. With FNV across 3 replicas, the chance
	// that 5 distinct keys all collide on the same replica is (1/3)^5
	// ≈ 0.4% — vanishingly small flake risk.
	differentKeys := []string{"user-B", "user-C", "user-D", "user-E", "user-F"}
	for _, k := range differentKeys {
		ctxK := WithRouteKey(context.Background(), k)
		var out struct{}
		if err := c.Call(ctxK, "GET", "/x", nil, &out); err != nil {
			t.Fatalf("key %q: %v", k, err)
		}
	}

	// At least one of the other replicas now has hits.
	other := false
	for i, h := range hits {
		if i != keyAReplica && h > 0 {
			other = true
			break
		}
	}
	if !other {
		t.Errorf("expected at least one different key to hit a non-keyA replica; hits=%v keyAReplica=%d", hits, keyAReplica)
	}
}

// TestRouting_FallbackOnEjectedReplica verifies that when the keyed
// replica is ejected, the call falls forward to the next non-ejected
// replica instead of starving — affinity is best-effort, not strict.
func TestRouting_FallbackOnEjectedReplica(t *testing.T) {
	var hits [3]uint64
	mk := func(i int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddUint64(&hits[i], 1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}))
	}
	srvs := [3]*httptest.Server{mk(0), mk(1), mk(2)}
	defer func() {
		for _, s := range srvs {
			s.Close()
		}
	}()

	c := NewRemoteCallerWithReplicas([]string{srvs[0].URL, srvs[1].URL, srvs[2].URL})

	// Find which replica "user-A" hashes to and manually eject it.
	keyA := "user-A"
	preferred := hashRouteKey(keyA, 3)
	c.replicas[preferred].eject(5_000_000_000) // 5s in nanoseconds

	// Now a keyA call should fall forward to a different replica.
	ctxA := WithRouteKey(context.Background(), keyA)
	var out struct{}
	if err := c.Call(ctxA, "GET", "/x", nil, &out); err != nil {
		t.Fatalf("keyA call: %v", err)
	}
	if hits[preferred] != 0 {
		t.Errorf("ejected replica %d should have 0 hits; got %d", preferred, hits[preferred])
	}
	totalOthers := uint64(0)
	for i, h := range hits {
		if i != preferred {
			totalOthers += h
		}
	}
	if totalOthers != 1 {
		t.Errorf("expected 1 hit on a non-preferred replica; got %d (hits=%v)", totalOthers, hits)
	}
}
