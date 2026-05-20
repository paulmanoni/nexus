package peer

import (
	"context"
	"encoding/json"
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
	mux.HandleFunc("/__peer/schema", emitSchema("test-server"))
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
		schemas:  newSchemaCache(),
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

// TestCall_DriftCheck_MissingMethod proves the drift check fires
// when the client tries to call a method the peer's schema
// doesn't list. Without this, the framework would let the call
// hit the wire and bounce off the server's NOT_FOUND envelope —
// less direct than failing at the client and harder to debug.
func TestCall_DriftCheck_MissingMethod(t *testing.T) {
	resetCallTable()
	defer resetCallTable()

	// Only "ping" is registered; the test calls "pong".
	callTable.Store("ping", &callEntry{
		Method:   "ping",
		ArgsType: nil,
		Bound: func(_ context.Context, _ any) (any, error) {
			return struct{}{}, nil
		},
	})

	reg, stop := roundTripFixture(t)
	defer stop()

	_, err := Call[struct{}](context.Background(), reg, "target", "pong", nil)
	if err == nil {
		t.Fatal("expected drift error for unknown method")
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("expected *peer.Error, got %T (%v)", err, err)
	}
	if pe.Code != "METHOD_UNKNOWN" {
		t.Errorf("error code = %q, want METHOD_UNKNOWN", pe.Code)
	}
	// The error should list available methods so the operator
	// can diff the typo against what the peer actually exposes.
	// Stored as []string in the in-process error path; would
	// arrive as []any after a JSON round-trip across the wire,
	// hence the type-switch.
	var available []string
	switch v := pe.Details["available_methods"].(type) {
	case []string:
		available = v
	case []any:
		for _, m := range v {
			if s, ok := m.(string); ok {
				available = append(available, s)
			}
		}
	default:
		t.Fatalf("details.available_methods = %T, want []string or []any", v)
	}
	found := false
	for _, m := range available {
		if m == "ping" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("available_methods missing 'ping': %v", available)
	}
}

// TestEmitSchema_ListsRegisteredMethods drives the server side of
// schema emission directly: register two methods, hit
// /__peer/schema, assert the payload lists both with the right
// type names. Catches "registered methods drift from emitted
// schema" regressions early.
func TestEmitSchema_ListsRegisteredMethods(t *testing.T) {
	resetCallTable()
	defer resetCallTable()

	callTable.Store("alpha", &callEntry{
		Method:   "alpha",
		ArgsType: reflectTypeFor[echoArgs](),
		RetType:  reflectTypeFor[echoReply](),
		Bound:    func(_ context.Context, _ any) (any, error) { return nil, nil },
	})
	callTable.Store("beta", &callEntry{
		Method: "beta",
		// no args/return → omitted from the wire
		Bound: func(_ context.Context, _ any) (any, error) { return nil, nil },
	})

	srv := httptest.NewServer(http.HandlerFunc(emitSchema("emitter-svc")))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got PeerSchema
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Identity != "emitter-svc" {
		t.Errorf("Identity = %q, want emitter-svc", got.Identity)
	}
	if got.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", got.SchemaVersion, SchemaVersion)
	}
	if len(got.Methods) != 2 {
		t.Fatalf("got %d methods, want 2: %+v", len(got.Methods), got.Methods)
	}
	byName := map[string]MethodSchema{}
	for _, m := range got.Methods {
		byName[m.Name] = m
	}
	alpha, ok := byName["alpha"]
	if !ok {
		t.Fatal("alpha missing")
	}
	if !strings.Contains(alpha.ArgsType, "echoArgs") {
		t.Errorf("alpha.ArgsType = %q, want contains echoArgs", alpha.ArgsType)
	}
	if !strings.Contains(alpha.ReturnType, "echoReply") {
		t.Errorf("alpha.ReturnType = %q, want contains echoReply", alpha.ReturnType)
	}
	beta, ok := byName["beta"]
	if !ok {
		t.Fatal("beta missing")
	}
	if beta.ArgsType != "" || beta.ReturnType != "" {
		t.Errorf("beta should have empty types (no args/return): %+v", beta)
	}
}

// TestProber_FlipsReadyOnFailure exercises the prober side effects:
// a peer that returns non-200 on /__peer/health should flip
// ready=false within one probe interval, and a peer that recovers
// should flip back. Uses a switch atomic that the test controls
// to drive the up/down transitions.
func TestProber_FlipsReadyOnFailure(t *testing.T) {
	resetCallTable()
	defer resetCallTable()

	var healthy atomic.Bool
	healthy.Store(true)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := &Registry{
		schemas: newSchemaCache(),
		peers: map[string]*peerConn{
			"target": {
				name:       "target",
				url:        srv.URL,
				httpClient: srv.Client(),
				sem:        make(chan struct{}, 8),
			},
		},
	}
	r.peers["target"].ready.Store(true)

	// Drive probeOnce directly rather than spinning up the full
	// prober loop — that path is what we want to exercise (and
	// it's deterministic), without sleeping through 10s
	// proberInterval ticks just to observe the state machine.
	probeOnce(context.Background(), r, r.peers["target"])
	if !r.peers["target"].ready.Load() {
		t.Fatal("ready=false after healthy probe; should be true")
	}

	healthy.Store(false)
	probeOnce(context.Background(), r, r.peers["target"])
	if r.peers["target"].ready.Load() {
		t.Fatal("ready=true after 503; should be false")
	}

	healthy.Store(true)
	probeOnce(context.Background(), r, r.peers["target"])
	if !r.peers["target"].ready.Load() {
		t.Fatal("ready=false after recovery; should be true")
	}
}

// TestProber_ResetsSchemaOnDownTransition proves the prober's
// side-effect contract: when a peer goes from ready→down, the
// cached schema for that peer is dropped, so the next Call after
// recovery re-fetches the (possibly-updated) schema. Without
// this, a peer that ships a new method would never have it
// surfaced on the caller's cache.
func TestProber_ResetsSchemaOnDownTransition(t *testing.T) {
	r := &Registry{
		schemas: newSchemaCache(),
		peers: map[string]*peerConn{
			"target": {name: "target"},
		},
	}
	r.peers["target"].ready.Store(true)
	// Prime the schema cache.
	r.schemas.schemas["target"] = &PeerSchema{Identity: "target"}

	flipReady(r, r.peers["target"], false)
	if _, ok := r.schemas.schemas["target"]; ok {
		t.Error("schema cache should be reset on ready→down transition")
	}

	// Re-prime; an up→up transition shouldn't reset.
	r.peers["target"].ready.Store(true)
	r.schemas.schemas["target"] = &PeerSchema{Identity: "target"}
	flipReady(r, r.peers["target"], true)
	if _, ok := r.schemas.schemas["target"]; !ok {
		t.Error("schema cache reset on up→up transition; should be preserved")
	}
}

// driftCallerArgs / driftPeerArgs / driftPeerReturn / driftCallerReturn
// are the per-fixture types the structural-drift tests below use.
// Each pair differs from its counterpart in one specific dimension
// (renamed field, new required field, mismatched primitive) so the
// assertion can point at exactly the rule that fired.

// New-required-field case: peer's schema requires Currency that
// the caller doesn't send.
type driftV1Args struct {
	UserID  string `json:"userId" validate:"required"`
	ItemIDs []string `json:"itemIds" validate:"required"`
}
type driftV2Args struct {
	UserID   string   `json:"userId" validate:"required"`
	ItemIDs  []string `json:"itemIds" validate:"required"`
	Currency string   `json:"currency" validate:"required"`
}

// Type-swap case: peer expects a string where the caller sends an
// integer. JSON decode would fail on the peer side either way; we
// want fail-fast at the call site.
type driftStringArgs struct {
	Value string `json:"value" validate:"required"`
}
type driftIntArgs struct {
	Value int `json:"value" validate:"required"`
}

// TestVerifyMethod_FailsOnMissingRequiredProperty drives the
// structural check's new-required-field path. Peer says
// driftV2Args (requires currency); caller's args type is
// driftV1Args (no currency). Without this guard the call would
// arrive at the peer with a zero-value Currency, fail handler
// validation, and surface as a confusing generic INTERNAL error.
func TestVerifyMethod_FailsOnMissingRequiredProperty(t *testing.T) {
	peerSchema := &PeerSchema{
		Identity: "orders-svc",
		Methods: []MethodSchema{{
			Name:       "createOrder",
			ArgsType:   "peer.driftV2Args",
			ArgsSchema: ReflectSchema(reflectTypeFor[driftV2Args]()),
		}},
	}
	err := verifyMethod(peerSchema, "createOrder",
		reflectTypeFor[driftV1Args](), nil)
	if err == nil {
		t.Fatal("expected drift error — caller missing required currency")
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("expected *peer.Error, got %T (%v)", err, err)
	}
	if pe.Code != "SCHEMA_MISSING_REQUIRED" {
		t.Errorf("code = %q, want SCHEMA_MISSING_REQUIRED", pe.Code)
	}
	if prop, _ := pe.Details["property"].(string); prop != "currency" {
		t.Errorf("details.property = %v, want currency", pe.Details["property"])
	}
}

// TestVerifyMethod_FailsOnTypeMismatch covers the primitive-type
// swap. The compare walks into Properties recursively so a
// nested type swap on a leaf field surfaces too — not just the
// top-level object-vs-array case.
func TestVerifyMethod_FailsOnTypeMismatch(t *testing.T) {
	peerSchema := &PeerSchema{
		Identity: "echo-svc",
		Methods: []MethodSchema{{
			Name:       "set",
			ArgsType:   "peer.driftStringArgs",
			ArgsSchema: ReflectSchema(reflectTypeFor[driftStringArgs]()),
		}},
	}
	err := verifyMethod(peerSchema, "set",
		reflectTypeFor[driftIntArgs](), nil)
	if err == nil {
		t.Fatal("expected drift error — string vs integer mismatch")
	}
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("expected *peer.Error, got %T (%v)", err, err)
	}
	if pe.Code != "SCHEMA_MISMATCH" {
		t.Errorf("code = %q, want SCHEMA_MISMATCH", pe.Code)
	}
}

// TestVerifyMethod_PassesOnForwardCompatibleAdd proves the
// forward-compat policy: if the CALLER has extra fields the peer
// doesn't know about, the call still succeeds. encoding/json on
// the peer side silently drops unknown keys, so the call is
// safe — failing here would block every rolling deploy where the
// caller is ahead of the peer.
func TestVerifyMethod_PassesOnForwardCompatibleAdd(t *testing.T) {
	// Peer is on the older v1 shape; caller's args are v2 (added
	// optional/required currency).
	peerSchema := &PeerSchema{
		Identity: "orders-svc",
		Methods: []MethodSchema{{
			Name:       "createOrder",
			ArgsType:   "peer.driftV1Args",
			ArgsSchema: ReflectSchema(reflectTypeFor[driftV1Args]()),
		}},
	}
	err := verifyMethod(peerSchema, "createOrder",
		reflectTypeFor[driftV2Args](), nil)
	if err != nil {
		t.Errorf("expected pass — caller with extra fields is forward-compat: %v", err)
	}
}

// TestVerifyMethod_PassesOnNoSchema proves the graceful-degradation
// path: an older peer that hasn't yet rolled out the JSON-Schema
// emit (ArgsSchema=nil) still has a usable type-name comparison.
// Without this fallback, every Call to a pre-rc4 peer would block.
func TestVerifyMethod_PassesOnNoSchema(t *testing.T) {
	peerSchema := &PeerSchema{
		Identity: "legacy-svc",
		Methods: []MethodSchema{{
			Name:     "doThing",
			ArgsType: "peer.driftV1Args",
			// ArgsSchema deliberately nil — simulates a peer
			// that hasn't shipped the new schema field yet.
		}},
	}
	err := verifyMethod(peerSchema, "doThing",
		reflectTypeFor[driftV1Args](), nil)
	if err != nil {
		t.Errorf("expected pass — nil ArgsSchema should not block call: %v", err)
	}
}

// TestDashboard_PeerListIncludesEveryPeer proves the dashboard
// handler enumerates every peer the registry knows about with
// the right Healthy + SchemaCached state. Exercises the in-memory
// projection used by the Peers tab without spinning up the whole
// dashboard mount path.
func TestDashboard_PeerListIncludesEveryPeer(t *testing.T) {
	r := &Registry{
		identity: "self",
		schemas:  newSchemaCache(),
		peers: map[string]*peerConn{
			"alpha": {name: "alpha", url: "https://alpha:1", sem: make(chan struct{}, 4)},
			"beta":  {name: "beta", url: "https://beta:2", sem: make(chan struct{}, 4)},
		},
	}
	r.peers["alpha"].ready.Store(true)
	r.peers["beta"].ready.Store(false)
	// Prime alpha's schema cache so we can assert the
	// SchemaCached flag flips per peer, not in aggregate.
	r.schemas.schemas["alpha"] = &PeerSchema{Identity: "alpha"}

	rows := snapshotPeers(r)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	byName := map[string]PeerStatus{}
	for _, row := range rows {
		byName[row.Name] = row
	}
	alpha, ok := byName["alpha"]
	if !ok {
		t.Fatal("alpha missing from rows")
	}
	if !alpha.Healthy {
		t.Errorf("alpha.Healthy = false, want true")
	}
	if !alpha.SchemaCached {
		t.Errorf("alpha.SchemaCached = false, want true")
	}
	beta, ok := byName["beta"]
	if !ok {
		t.Fatal("beta missing from rows")
	}
	if beta.Healthy {
		t.Errorf("beta.Healthy = true, want false")
	}
	if beta.SchemaCached {
		t.Errorf("beta.SchemaCached = true, want false (never primed)")
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
