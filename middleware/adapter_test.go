package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/graph"
)

func init() { gin.SetMode(gin.TestMode) }

func TestFromHandlerRealizations(t *testing.T) {
	pass := func(rc *RequestCtx, next Next) error { return next(rc) }

	all := FromHandler(NewFunc("a", AllTransports, pass))
	if all.Gin == nil || all.Graph == nil {
		t.Fatalf("AllTransports handler should yield both realizations")
	}
	if all.Name != "a" {
		t.Fatalf("Name not propagated: %q", all.Name)
	}

	restOnly := FromHandler(NewFunc("r", Transports(TransportREST), pass))
	if restOnly.Gin == nil || restOnly.Graph != nil {
		t.Fatalf("REST-only handler should yield Gin only")
	}

	wsOnly := FromHandler(NewFunc("w", Transports(TransportWebSocket), pass))
	if wsOnly.Gin == nil || wsOnly.Graph != nil {
		t.Fatalf("WS-only handler should yield Gin (upgrade) only")
	}

	graphOnly := FromHandler(NewFunc("g", Transports(TransportGraphQL), pass))
	if graphOnly.Gin != nil || graphOnly.Graph == nil {
		t.Fatalf("GraphQL-only handler should yield Graph only")
	}
}

func TestGinAdapterPassThrough(t *testing.T) {
	seen := false
	h := NewFunc("ip", AllTransports, func(rc *RequestCtx, next Next) error {
		if rc.ClientIP() == "" { // exercises the carrier
			t.Errorf("expected a client IP from gin")
		}
		return next(rc)
	})
	mw := FromHandler(h)

	r := gin.New()
	r.GET("/x", mw.Gin, func(c *gin.Context) { seen = true; c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "203.0.113.5:1234"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if !seen {
		t.Fatalf("downstream handler should have run")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestGinAdapterReject(t *testing.T) {
	downstream := false
	h := NewFunc("deny", AllTransports, func(rc *RequestCtx, next Next) error {
		return rc.Reject(http.StatusTooManyRequests, errors.New("nope"))
	})
	mw := FromHandler(h)

	r := gin.New()
	r.GET("/x", mw.Gin, func(*gin.Context) { downstream = true })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if downstream {
		t.Fatalf("rejected request should not reach downstream")
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
}

func TestGraphAdapterPassThrough(t *testing.T) {
	h := NewFunc("ok", AllTransports, func(rc *RequestCtx, next Next) error { return next(rc) })
	fm := FromHandler(h).Graph

	resolver := func(p graph.ResolveParams) (any, error) { return "result", nil }
	wrapped := fm(resolver)

	got, err := wrapped(graph.ResolveParams{Context: context.Background()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "result" {
		t.Fatalf("got %v, want result", got)
	}
}

func TestGraphAdapterReject(t *testing.T) {
	resolverCalled := false
	h := NewFunc("deny", AllTransports, func(rc *RequestCtx, next Next) error {
		return rc.Reject(0, errors.New("forbidden"))
	})
	fm := FromHandler(h).Graph

	resolver := func(p graph.ResolveParams) (any, error) { resolverCalled = true; return "x", nil }
	wrapped := fm(resolver)

	_, err := wrapped(graph.ResolveParams{Context: context.Background()})
	if err == nil || err.Error() != "forbidden" {
		t.Fatalf("expected forbidden error, got %v", err)
	}
	if resolverCalled {
		t.Fatalf("rejected resolver should not run")
	}
}

func TestClientIPCtxRoundTrip(t *testing.T) {
	ctx := WithClientIP(context.Background(), "9.9.9.9")
	if ClientIPFromCtx(ctx) != "9.9.9.9" {
		t.Fatalf("client IP round-trip failed")
	}
	if ClientIPFromCtx(context.Background()) != "" {
		t.Fatalf("absent IP should be empty")
	}
}
