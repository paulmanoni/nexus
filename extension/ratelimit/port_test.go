package ratelimit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/graph"
)

// The unified NewMiddleware (ported to middleware.FromHandler) must keep its
// per-transport behavior: REST denies with 429 + Retry-After; GraphQL denies
// with a "rate limit exceeded" error. Both realizations come from one Handler.

func TestNewMiddlewareREST(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := NewMemoryStore()
	mw := NewMiddleware(s, "rest.key", Limit{RPM: 600, Burst: 1})
	if mw.Gin == nil {
		t.Fatal("expected a Gin realization")
	}

	r := gin.New()
	r.GET("/x", mw.Gin, func(c *gin.Context) { c.Status(http.StatusOK) })
	do := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
		return w
	}

	if w := do(); w.Code != http.StatusOK {
		t.Fatalf("first request: status %d, want 200", w.Code)
	}
	w := do()
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatalf("429 response missing Retry-After header")
	}
}

func TestNewMiddlewareGraphQL(t *testing.T) {
	s := NewMemoryStore()
	mw := NewMiddleware(s, "gql.key", Limit{RPM: 600, Burst: 1})
	if mw.Graph == nil {
		t.Fatal("expected a Graph realization")
	}

	resolved := 0
	resolver := func(p graph.ResolveParams) (any, error) { resolved++; return "ok", nil }
	wrapped := mw.Graph(resolver)
	call := func() (any, error) { return wrapped(graph.ResolveParams{Context: context.Background()}) }

	if _, err := call(); err != nil {
		t.Fatalf("first call errored: %v", err)
	}
	_, err := call()
	if err == nil || !strings.Contains(err.Error(), "rate limit exceeded") {
		t.Fatalf("second call: want rate-limit error, got %v", err)
	}
	if resolved != 1 {
		t.Fatalf("resolver ran %d times, want 1 (denied call must not resolve)", resolved)
	}
}

func TestNewMiddlewarePerIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := NewMemoryStore()
	mw := NewMiddleware(s, "perip.key", Limit{RPM: 600, Burst: 1, PerIP: true})

	r := gin.New()
	r.GET("/x", mw.Gin, func(c *gin.Context) { c.Status(http.StatusOK) })
	req := func(ip string) int {
		w := httptest.NewRecorder()
		rq := httptest.NewRequest(http.MethodGet, "/x", nil)
		rq.RemoteAddr = ip + ":1111"
		r.ServeHTTP(w, rq)
		return w.Code
	}

	// Distinct IPs get independent buckets; the same IP is throttled.
	if req("1.1.1.1") != http.StatusOK {
		t.Fatal("first IP-1 request should pass")
	}
	if req("2.2.2.2") != http.StatusOK {
		t.Fatal("first IP-2 request should pass (separate bucket)")
	}
	if req("1.1.1.1") != http.StatusTooManyRequests {
		t.Fatal("second IP-1 request should be throttled")
	}
}
