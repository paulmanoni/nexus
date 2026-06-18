package middleware

import (
	"context"
	"errors"
	"testing"

	"github.com/paulmanoni/nexus/httpx"

	"github.com/paulmanoni/nexus/graph"
)

// fakeCarrier is an in-package carrier so tests can build a RequestCtx without
// a real transport adapter.
type fakeCarrier struct {
	headers      map[string]string
	setHeaders   map[string]string // captured response headers
	ip           string
	pathVal      string
	rejectErr    error
	rejectSeen   int // captured status from the last reject call
	rejectedBody any // captured body from the last rejectJSON call
}

func (f *fakeCarrier) header(key string) string { return f.headers[key] }
func (f *fakeCarrier) clientIP() string         { return f.ip }
func (f *fakeCarrier) path() string             { return f.pathVal }
func (f *fakeCarrier) setHeader(key, val string) {
	if f.setHeaders == nil {
		f.setHeaders = map[string]string{}
	}
	f.setHeaders[key] = val
}
func (f *fakeCarrier) reject(status int, err error) error {
	f.rejectSeen = status
	if f.rejectErr != nil {
		return f.rejectErr
	}
	return err
}
func (f *fakeCarrier) rejectJSON(status int, body any) error {
	f.rejectSeen = status
	f.rejectedBody = body
	return errRejected
}

func TestTransportSet(t *testing.T) {
	s := Transports(TransportREST, TransportGraphQL)
	if !s.Has(TransportREST) || !s.Has(TransportGraphQL) {
		t.Fatalf("expected REST and GraphQL set, got %s", s)
	}
	if s.Has(TransportWebSocket) {
		t.Fatalf("WebSocket should not be set in %s", s)
	}

	var empty TransportSet
	for _, tr := range []Transport{TransportREST, TransportGraphQL, TransportWebSocket} {
		if empty.Has(tr) {
			t.Fatalf("zero value should be the empty set, but Has(%s) was true", tr)
		}
	}

	if !AllTransports.Has(TransportREST) || !AllTransports.Has(TransportGraphQL) || !AllTransports.Has(TransportWebSocket) {
		t.Fatalf("AllTransports should include every transport, got %s", AllTransports)
	}

	if got := Transports(TransportREST, TransportGraphQL).String(); got != "{REST, GraphQL}" {
		t.Fatalf("String() canonical order wrong: got %q", got)
	}
	if got := empty.String(); got != "{}" {
		t.Fatalf("empty String() = %q, want {}", got)
	}
}

func TestFunc(t *testing.T) {
	called := false
	nextCalled := false
	f := NewFunc("test", AllTransports, func(rc *RequestCtx, next Next) error {
		called = true
		return next(rc)
	})

	if f.Name() != "test" {
		t.Fatalf("Name() = %q, want test", f.Name())
	}
	if f.Transports() != AllTransports {
		t.Fatalf("Transports() = %s, want %s", f.Transports(), AllTransports)
	}

	rc := newRequestCtx(context.Background(), TransportREST, &fakeCarrier{})
	err := f.Handle(rc, func(*RequestCtx) error { nextCalled = true; return nil })
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if !called || !nextCalled {
		t.Fatalf("Handle should invoke closure (%v) and thread next (%v)", called, nextCalled)
	}
}

func TestLegacyBundleTransports(t *testing.T) {
	ginOnly := AsHandler(Middleware{Name: "g", Gin: func(*httpx.Ctx) {}})
	if got := ginOnly.Transports(); got != Transports(TransportREST, TransportWebSocket) {
		t.Fatalf("gin-only Transports = %s, want {REST, WebSocket}", got)
	}

	graphOnly := AsHandler(Middleware{Name: "q", Graph: func(next graph.FieldResolveFn) graph.FieldResolveFn { return next }})
	if got := graphOnly.Transports(); got != Transports(TransportGraphQL) {
		t.Fatalf("graph-only Transports = %s, want {GraphQL}", got)
	}

	both := AsHandler(Middleware{
		Name:  "b",
		Gin:   func(*httpx.Ctx) {},
		Graph: func(next graph.FieldResolveFn) graph.FieldResolveFn { return next },
	})
	if got := both.Transports(); got != Transports(TransportREST, TransportWebSocket, TransportGraphQL) {
		t.Fatalf("both Transports = %s, want {REST, GraphQL, WebSocket}", got)
	}

	var none TransportSet
	if got := AsHandler(Middleware{Name: "n"}).Transports(); got != none {
		t.Fatalf("empty bundle Transports = %s, want empty set", got)
	}
}

func TestRequestCtx(t *testing.T) {
	fc := &fakeCarrier{
		headers: map[string]string{"X-Test": "v"},
		ip:      "1.2.3.4",
		pathVal: "/x",
	}
	rc := newRequestCtx(context.Background(), TransportREST, fc)

	if rc.Header("X-Test") != "v" {
		t.Fatalf("Header delegation failed")
	}
	if rc.ClientIP() != "1.2.3.4" {
		t.Fatalf("ClientIP delegation failed")
	}
	if rc.Path() != "/x" {
		t.Fatalf("Path delegation failed")
	}

	rc.SetHeader("Retry-After", "5")
	if fc.setHeaders["Retry-After"] != "5" {
		t.Fatalf("SetHeader delegation failed")
	}

	rc.Set("k", 42)
	if v, ok := rc.Get("k"); !ok || v.(int) != 42 {
		t.Fatalf("Set/Get round-trip failed: %v %v", v, ok)
	}
	if _, ok := rc.Get("missing"); ok {
		t.Fatalf("Get of missing key should report ok=false")
	}

	type ctxKey struct{}
	rc.WithContext(context.WithValue(context.Background(), ctxKey{}, "y"))
	if rc.Context.Value(ctxKey{}) != "y" {
		t.Fatalf("WithContext did not swap the context")
	}

	sentinel := errors.New("boom")
	if err := rc.Reject(401, sentinel); err != sentinel {
		t.Fatalf("Reject returned %v, want sentinel", err)
	}
	if fc.rejectSeen != 401 {
		t.Fatalf("carrier saw status %d, want 401", fc.rejectSeen)
	}
}

func TestLegacyBundleHandle(t *testing.T) {
	// GraphQL transport with nil Graph: pass-through to next.
	nextCalled := false
	h := AsHandler(Middleware{Name: "g"})
	rc := newRequestCtx(context.Background(), TransportGraphQL, &fakeCarrier{})
	if err := h.Handle(rc, func(*RequestCtx) error { nextCalled = true; return nil }); err != nil {
		t.Fatalf("graphql pass-through returned error: %v", err)
	}
	if !nextCalled {
		t.Fatalf("graphql pass-through should call next")
	}

	// REST transport: guard error mentioning the transport (gin runs natively in step 3).
	hr := newRequestCtx(context.Background(), TransportREST, &fakeCarrier{})
	err := AsHandler(Middleware{Name: "r", Gin: func(*httpx.Ctx) {}}).
		Handle(hr, func(*RequestCtx) error { return nil })
	if err == nil {
		t.Fatalf("REST Handle should return the step-3 guard error")
	}
}
