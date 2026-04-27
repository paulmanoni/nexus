package nexus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestReplicas_RoundRobin verifies that calls land on each replica in
// turn so a 3-replica peer gets balanced traffic instead of pinning
// to the first URL.
func TestReplicas_RoundRobin(t *testing.T) {
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

	for i := 0; i < 9; i++ {
		var out struct{}
		if err := c.Call(context.Background(), "GET", "/x", nil, &out); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	for i, h := range hits {
		if h != 3 {
			t.Errorf("replica %d: want 3 hits, got %d (hits=%v)", i, h, hits)
		}
	}
}

// TestReplicas_EjectOn5xx verifies that a replica returning 5xx is
// passively ejected so subsequent calls round past it. Combined with
// WithEjectDuration set short, also verifies the replica re-enters
// rotation after the cooldown expires.
func TestReplicas_EjectOn5xx(t *testing.T) {
	var bad, good uint64
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddUint64(&bad, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer badSrv.Close()
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddUint64(&good, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer goodSrv.Close()

	c := NewRemoteCallerWithReplicas(
		[]string{badSrv.URL, goodSrv.URL},
		WithEjectDuration(150*time.Millisecond),
	)

	// First call may hit either replica depending on cursor seed; we
	// just want eventual ejection of bad. Drive enough calls that
	// the bad replica is hit at least once and ejected.
	for i := 0; i < 4; i++ {
		var out struct{}
		_ = c.Call(context.Background(), "GET", "/x", nil, &out)
	}
	hitsAfterEject := atomic.LoadUint64(&bad)
	if hitsAfterEject == 0 {
		t.Fatal("bad replica should have been hit at least once before eject")
	}

	// Now the bad replica should be ejected — every call lands on
	// good. Drive 6 more and confirm bad doesn't tick.
	beforeBad := atomic.LoadUint64(&bad)
	beforeGood := atomic.LoadUint64(&good)
	for i := 0; i < 6; i++ {
		var out struct{}
		_ = c.Call(context.Background(), "GET", "/x", nil, &out)
	}
	if atomic.LoadUint64(&bad) != beforeBad {
		t.Errorf("bad replica should be ejected; hits went %d → %d", beforeBad, atomic.LoadUint64(&bad))
	}
	if atomic.LoadUint64(&good)-beforeGood != 6 {
		t.Errorf("good replica should absorb all 6 calls; got %d", atomic.LoadUint64(&good)-beforeGood)
	}

	// After the eject window, bad re-enters rotation. Drive enough
	// calls to see at least one hit again.
	time.Sleep(200 * time.Millisecond)
	beforeBad = atomic.LoadUint64(&bad)
	for i := 0; i < 8; i++ {
		var out struct{}
		_ = c.Call(context.Background(), "GET", "/x", nil, &out)
	}
	if atomic.LoadUint64(&bad) == beforeBad {
		t.Error("bad replica should re-enter rotation after eject window expired")
	}
}

// TestReplicas_FailoverOnTransportError verifies that an idempotent
// call retries on a different replica when the first pick fails at
// the transport layer (connection refused on a closed listener).
func TestReplicas_FailoverOnTransportError(t *testing.T) {
	var goodHits uint64
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddUint64(&goodHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer good.Close()

	// Bind a listener and immediately close it so its URL is reachable
	// in form but refuses connections.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	c := NewRemoteCallerWithReplicas(
		[]string{deadURL, good.URL},
		WithRetries(1),
		WithEjectDuration(5*time.Second),
	)

	// One idempotent call — even if the cursor lands on dead first,
	// the retry should pick good and succeed.
	var out struct{}
	if err := c.Call(context.Background(), "GET", "/x", nil, &out); err != nil {
		t.Fatalf("call: want failover to good replica, got %v", err)
	}
	if atomic.LoadUint64(&goodHits) == 0 {
		t.Error("good replica should have been hit via failover")
	}
}
