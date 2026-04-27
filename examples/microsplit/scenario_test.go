package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulmanoni/nexus"
)

// These tests verify the production-grade features the framework
// gained for split deployments using the same primitives the
// framework uses under the hood. They don't boot the full microsplit
// binary — that requires `nexus build --deployment X` codegen — but
// they exercise the runtime contract a generated client would
// satisfy, with httptest servers standing in for live users-svc
// replicas. The test names map 1:1 to the four shipped features.
//
// To verify Listeners + Health/Ready end-to-end (which require the
// full fx lifecycle binding real net.Listeners), follow the curl
// recipes documented in main.go's package comment after running
// `nexus dev --split`.

// usersFake stands in for one users-svc replica. Each instance counts
// hits and serves /__nexus/health for the readiness prober and the
// user lookup endpoint that checkout invokes.
type usersFake struct {
	*httptest.Server
	hits *uint64
}

func newUsersFake(name string) *usersFake {
	var hits uint64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/__nexus/health":
			w.WriteHeader(http.StatusOK)
		default:
			atomic.AddUint64(&hits, 1)
			w.Header().Set("X-Replica", name)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"u1","name":"Alice"}`))
		}
	}))
	return &usersFake{Server: srv, hits: &hits}
}

func (f *usersFake) count() uint64 { return atomic.LoadUint64(f.hits) }

// TestMicrosplit_RoundRobinAcrossReplicas verifies that a 3-replica
// users-svc gets balanced traffic when checkout invokes it without a
// route key. This is feature #1 (multi-replica peers) running on the
// same plumbing checkout would use under split mode.
func TestMicrosplit_RoundRobinAcrossReplicas(t *testing.T) {
	a := newUsersFake("a")
	b := newUsersFake("b")
	c := newUsersFake("c")
	defer a.Close()
	defer b.Close()
	defer c.Close()

	peer := nexus.Peer{URLs: []string{a.URL, b.URL, c.URL}}
	caller := nexus.NewPeerCaller(peer)

	for i := 0; i < 9; i++ {
		var out struct {
			ID, Name string
		}
		if err := caller.Call(context.Background(), "GET", "/users/u1", nil, &out); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	if a.count() != 3 || b.count() != 3 || c.count() != 3 {
		t.Errorf("expected 3 hits per replica; a=%d b=%d c=%d", a.count(), b.count(), c.count())
	}
}

// TestMicrosplit_RouteKeyPinsByUser verifies that checkout's
// WithRouteKey(userID) primitive pins all calls for the same user to
// the same replica. This is feature #2 (sticky-key routing) and
// matches what NewSubmit in checkout.go does at runtime.
func TestMicrosplit_RouteKeyPinsByUser(t *testing.T) {
	replicas := []*usersFake{newUsersFake("a"), newUsersFake("b"), newUsersFake("c")}
	defer func() {
		for _, r := range replicas {
			r.Close()
		}
	}()
	urls := []string{replicas[0].URL, replicas[1].URL, replicas[2].URL}
	caller := nexus.NewPeerCaller(nexus.Peer{URLs: urls})

	// 12 calls all keyed "u-alice" should land on exactly one replica.
	ctx := nexus.WithRouteKey(context.Background(), "u-alice")
	for i := 0; i < 12; i++ {
		var out struct {
			ID, Name string
		}
		if err := caller.Call(ctx, "GET", "/users/u-alice", nil, &out); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	hot := -1
	for i, r := range replicas {
		if r.count() > 0 {
			if hot != -1 {
				t.Errorf("u-alice spread across replicas %d and %d (hits=%d/%d/%d)",
					hot, i, replicas[0].count(), replicas[1].count(), replicas[2].count())
			}
			hot = i
		}
	}
	if hot == -1 {
		t.Fatal("u-alice never landed anywhere")
	}
	if replicas[hot].count() != 12 {
		t.Errorf("u-alice replica %d: want 12 hits, got %d", hot, replicas[hot].count())
	}
}

// TestMicrosplit_5xxEjectsAndRecovers verifies that a sick replica is
// passively ejected so subsequent calls round past it, and that it
// re-enters rotation after the eject window expires. This is the
// passive-failover behavior every multi-replica peer relies on.
func TestMicrosplit_5xxEjectsAndRecovers(t *testing.T) {
	var sickHits, healthyHits uint64
	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddUint64(&sickHits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sick.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddUint64(&healthyHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"u1","name":"Alice"}`))
	}))
	defer healthy.Close()

	caller := nexus.NewPeerCaller(
		nexus.Peer{URLs: []string{sick.URL, healthy.URL}},
		nexus.WithEjectDuration(150*time.Millisecond),
	)

	// Drive enough calls to trip the sick replica's ejection.
	for i := 0; i < 4; i++ {
		var out struct{ ID string }
		_ = caller.Call(context.Background(), "GET", "/users/u1", nil, &out)
	}
	if atomic.LoadUint64(&sickHits) == 0 {
		t.Fatal("sick replica should have been hit before eject")
	}

	// While the sick replica is ejected, every call should land on
	// the healthy one.
	beforeSick := atomic.LoadUint64(&sickHits)
	for i := 0; i < 6; i++ {
		var out struct{ ID string }
		if err := caller.Call(context.Background(), "GET", "/users/u1", nil, &out); err != nil {
			t.Fatalf("post-eject call %d: %v", i, err)
		}
	}
	if atomic.LoadUint64(&sickHits) != beforeSick {
		t.Errorf("sick replica took traffic during eject window")
	}

	// After cooldown, the sick replica is eligible again.
	time.Sleep(200 * time.Millisecond)
	beforeSick = atomic.LoadUint64(&sickHits)
	for i := 0; i < 8; i++ {
		var out struct{ ID string }
		_ = caller.Call(context.Background(), "GET", "/users/u1", nil, &out)
	}
	if atomic.LoadUint64(&sickHits) == beforeSick {
		t.Error("sick replica should re-enter rotation after eject window expired")
	}
}
