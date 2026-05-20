package peer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/trace"
)

// reflectTypeFor is a tiny helper so the tests can ask for the
// reflect.Type of a generic type without spelling out
// reflect.TypeOf((*T)(nil)).Elem() each time. Lives in the test
// file to keep the production package's API focused.
func reflectTypeFor[T any]() reflect.Type {
	return reflect.TypeOf((*T)(nil)).Elem()
}

// resetCallTable clears the package-global method registry between
// tests. AsCall is process-wide by design (the dispatcher is a
// single mux); tests share that state, so we reset it here.
func resetCallTable() {
	callTable.Range(func(k, _ any) bool {
		callTable.Delete(k)
		return true
	})
}

// roundTripFixture spins up a httptest TLS server speaking the peer
// wire format and pairs it with a Registry pointed at that server.
// Uses AuthNone (dev mode) to keep the fixture small — the auth
// tests below exercise the AuthHMAC path separately.
func roundTripFixture(t *testing.T) (*Registry, func()) {
	t.Helper()
	t.Setenv(devEnv, "1") // unlock AuthNone

	mux := http.NewServeMux()
	// Tests don't exercise the dashboard bus path; nil here is
	// the documented no-trace-events mode. A separate test below
	// (TestDispatchCall_StitchesTraceFromHeader) drives the bus
	// branch with a real bus.
	mux.HandleFunc("/__peer/call", dispatchCall(AuthNone, nil, nil, "test-server"))
	mux.HandleFunc("/__peer/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// httptest.NewTLSServer wires a self-signed cert + a ready
	// http.Client that trusts it — saves us from generating PKI
	// material for every test.
	srv := httptest.NewTLSServer(mux)

	reg := &Registry{
		identity: "test-client",
		authMode: AuthNone,
		peers: map[string]*peerConn{
			"target": {
				name:       "target",
				url:        srv.URL,
				httpClient: srv.Client(), // already trusts the test cert
				sem:        make(chan struct{}, 8),
			},
		},
	}
	reg.peers["target"].ready.Store(true)
	return reg, srv.Close
}

type echoArgs struct {
	Message string `json:"message"`
}
type echoReply struct {
	Echo string `json:"echo"`
}

// TestAsCall_RoundTrip is the headline integration: register a
// method with AsCall (using the framework's reflective shape),
// dispatch through the HTTP/2 handler, decode a typed result on
// the client side via peer.Call[Out]. This exercises the full
// HandlerShape → BoundHandler → Envelope → Call[Out] chain.
func TestAsCall_RoundTrip(t *testing.T) {
	resetCallTable()
	defer resetCallTable()

	// Skip the AsCall registration path (it needs fx.Start) and
	// stamp a callEntry directly. AsCall's behavior is tested
	// separately via the framework's reflective inspector tests;
	// here we're proving the wire layer dispatches correctly.
	callTable.Store("echo", &callEntry{
		Method:   "echo",
		ArgsType: reflectTypeFor[echoArgs](),
		RetType:  reflectTypeFor[echoReply](),
		Bound: func(_ context.Context, args any) (any, error) {
			in, ok := args.(echoArgs)
			if !ok {
				return nil, errors.New("bad args type")
			}
			return echoReply{Echo: "got: " + in.Message}, nil
		},
	})

	reg, stop := roundTripFixture(t)
	defer stop()

	got, err := Call[echoReply](context.Background(), reg, "target", "echo",
		echoArgs{Message: "hello"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Echo != "got: hello" {
		t.Errorf("Echo = %q, want %q", got.Echo, "got: hello")
	}
}

// TestCall_TypedError verifies that a peer.Error returned by the
// handler arrives at the caller as *peer.Error — fully typed,
// errors.As-able. This is the contract that lets cross-app code
// branch on error.Code without rebuilding the error pyramid.
func TestCall_TypedError(t *testing.T) {
	resetCallTable()
	defer resetCallTable()
	callTable.Store("failing", &callEntry{
		Method:   "failing",
		ArgsType: nil,
		Bound: func(_ context.Context, _ any) (any, error) {
			return nil, &Error{Code: "VALIDATION", Msg: "bad input", Hint: "try again"}
		},
	})

	reg, stop := roundTripFixture(t)
	defer stop()

	_, err := Call[struct{}](context.Background(), reg, "target", "failing", nil)
	if err == nil {
		t.Fatal("Call: expected error")
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("Call: want *peer.Error, got %T (%v)", err, err)
	}
	if pe.Code != "VALIDATION" || pe.Hint != "try again" {
		t.Errorf("Error mismatch: %+v", pe)
	}
}

// TestCall_FastFailWhenPeerDown drives the IsHealthy short-circuit:
// the prober flips ready=false, every subsequent Call returns
// immediately instead of queuing behind the dial timeout.
func TestCall_FastFailWhenPeerDown(t *testing.T) {
	resetCallTable()
	defer resetCallTable()
	reg, stop := roundTripFixture(t)
	defer stop()

	reg.peers["target"].ready.Store(false)

	start := time.Now()
	_, err := Call[struct{}](context.Background(), reg, "target", "anything", nil)
	if err == nil || !strings.Contains(err.Error(), "marked unhealthy") {
		t.Errorf("expected unhealthy fast-fail, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("fast-fail took %s — should be sub-ms", elapsed)
	}
}

// TestDispatchCall_StitchesTraceFromHeader proves the inbound side
// of the traceparent wiring: a request that carries a W3C
// traceparent header lands on the dispatcher's bus as a
// peer.handle span whose TraceID matches the caller's. Without
// stitching, every peer call would be the root of its own trace
// and the dashboard waterfall would split mid-flight.
func TestDispatchCall_StitchesTraceFromHeader(t *testing.T) {
	resetCallTable()
	defer resetCallTable()

	callTable.Store("ping", &callEntry{
		Method:   "ping",
		ArgsType: nil,
		Bound: func(_ context.Context, _ any) (any, error) {
			return struct{}{}, nil
		},
	})

	bus := trace.NewBus(64)
	// Subscribe BEFORE issuing the call so we don't race the
	// publisher. Buffered channel + Subscribe's snapshot guarantee
	// no events drop between the dispatcher firing and the test
	// reading them off.
	_, events, cancelSub := bus.Subscribe(0, 32)
	defer cancelSub()

	srv := httptest.NewTLSServer(http.HandlerFunc(
		dispatchCall(AuthNone, nil, bus, "callee-svc")))
	defer srv.Close()

	// Use a fixed traceparent — easier to assert than minting one
	// fresh and reading it back from headers. Caller side would
	// pull this off ctx via trace.InjectHeader; we hard-code to
	// isolate the inbound stitching from the outbound wiring.
	const wantTrace = "0123456789abcdef0123456789abcdef"
	const callerSpan = "fedcba9876543210"

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/__peer/call",
		strings.NewReader(`{"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("traceparent", "00-"+wantTrace+"-"+callerSpan+"-01")
	req.Header.Set("X-Nexus-Peer", "caller-svc")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	// Collect events for up to 200ms; we expect a span.start +
	// span.end pair for the peer.handle span, both tagged with
	// the caller's traceID and our callerSpan as ParentID.
	deadline := time.After(200 * time.Millisecond)
	var saw []trace.Event
loop:
	for len(saw) < 2 {
		select {
		case e := <-events:
			if strings.HasPrefix(e.Name, "peer.handle") {
				saw = append(saw, e)
			}
		case <-deadline:
			break loop
		}
	}
	if len(saw) < 2 {
		t.Fatalf("expected span.start + span.end for peer.handle, got %d events", len(saw))
	}
	for _, e := range saw {
		if e.TraceID != wantTrace {
			t.Errorf("TraceID = %q, want %q (caller's trace should stitch)", e.TraceID, wantTrace)
		}
		if e.ParentID != callerSpan {
			t.Errorf("ParentID = %q, want %q (should parent to caller's span)", e.ParentID, callerSpan)
		}
	}
}

// TestCall_RespectsContextCancel proves the per-peer semaphore
// honors ctx.Done(): when the budget is exhausted, blocked calls
// abort cleanly on cancellation instead of leaking goroutines.
func TestCall_RespectsContextCancel(t *testing.T) {
	resetCallTable()
	defer resetCallTable()

	// Use a slow handler so we can fill the semaphore.
	release := make(chan struct{})
	callTable.Store("slow", &callEntry{
		Method:   "slow",
		ArgsType: nil,
		Bound: func(ctx context.Context, _ any) (any, error) {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return struct{}{}, nil
		},
	})

	reg, stop := roundTripFixture(t)
	defer stop()

	// Shrink the budget to 1 so the second concurrent call has to
	// queue, then test the queueing path's cancel behavior.
	reg.peers["target"].sem = make(chan struct{}, 1)

	var firstStarted atomic.Bool
	go func() {
		firstStarted.Store(true)
		_, _ = Call[struct{}](context.Background(), reg, "target", "slow", nil)
	}()
	// Wait until the first goroutine has acquired the slot.
	for !firstStarted.Load() {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Call[struct{}](ctx, reg, "target", "slow", nil)
	close(release)
	if err == nil {
		t.Fatal("expected context error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if waited := time.Since(start); waited > 100*time.Millisecond {
		t.Errorf("cancel took %s — should respect ctx deadline (25ms)", waited)
	}
}
