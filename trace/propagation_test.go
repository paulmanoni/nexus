package trace

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestParseTraceparent_Valid(t *testing.T) {
	h := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	tr, sp, ok := parseTraceparent(h)
	if !ok {
		t.Fatal("expected ok")
	}
	if tr != "0123456789abcdef0123456789abcdef" || sp != "0123456789abcdef" {
		t.Fatalf("wrong parts: %q / %q", tr, sp)
	}
}

func TestParseTraceparent_Invalid(t *testing.T) {
	cases := []string{
		"",
		"01-0123456789abcdef0123456789abcdef-0123456789abcdef-01",                   // wrong version
		"00-0123456789abcdef0123456789abcdef-0123456789abcdef",                      // too few parts
		"00-0123456789abcdef0123456789abcde-0123456789abcdef-01",                    // trace id wrong length
		"00-0123456789abcdef0123456789abcdef-0123456789abcde-01",                    // span id wrong length
		"00-00000000000000000000000000000000-0123456789abcdef-01",                   // trace id all zeros
		"00-0123456789abcdef0123456789abcdef-0000000000000000-01",                   // span id all zeros
		"00-0123456789abcdefg123456789abcdef-0123456789abcdef-01",                   // non-hex
	}
	for _, h := range cases {
		if _, _, ok := parseTraceparent(h); ok {
			t.Fatalf("expected parse to fail: %q", h)
		}
	}
}

func TestMiddleware_HonorsInboundTraceparent(t *testing.T) {
	bus := NewBus(16)
	_, ch, cancel := bus.Subscribe(0, 16)
	defer cancel()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Middleware(bus, "svc", "op", "rest"))
	r.GET("/x", func(c *gin.Context) {
		span, _ := SpanFrom(c)
		if !span.Remote {
			t.Errorf("expected span.Remote=true for inbound traceparent")
		}
		if span.TraceID != "0123456789abcdef0123456789abcdef" {
			t.Errorf("traceID not inherited: %q", span.TraceID)
		}
		if span.ParentID != "fedcba9876543210" {
			t.Errorf("parentID not set from upstream span id: %q", span.ParentID)
		}
		c.String(200, "ok")
	})

	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("traceparent", "00-0123456789abcdef0123456789abcdef-fedcba9876543210-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	events := drain(ch, 2, time.Second)
	if len(events) != 2 {
		t.Fatalf("expected request.start+end, got %d", len(events))
	}
	if events[0].TraceID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("event TraceID not inherited: %q", events[0].TraceID)
	}
	if !events[0].Remote {
		t.Fatalf("expected Remote=true on request.start event")
	}
}

func TestMiddleware_MintsFreshWhenNoHeader(t *testing.T) {
	bus := NewBus(16)
	_, ch, cancel := bus.Subscribe(0, 16)
	defer cancel()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Middleware(bus, "svc", "op", "rest"))
	r.GET("/x", func(c *gin.Context) { c.String(200, "ok") })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))

	events := drain(ch, 1, time.Second)
	if len(events) == 0 {
		t.Fatal("no events")
	}
	if events[0].Remote {
		t.Fatalf("should not be remote without header")
	}
	if len(events[0].TraceID) != 32 {
		t.Fatalf("fresh TraceID should be 32 hex: %q", events[0].TraceID)
	}
}

func TestInjectHeader(t *testing.T) {
	span := &Span{
		TraceID: "0123456789abcdef0123456789abcdef",
		SpanID:  "0123456789abcdef",
	}
	ctx := context.WithValue(context.Background(), spanCtxKey{}, span)
	h := http.Header{}
	InjectHeader(ctx, h)
	got := h.Get("traceparent")
	want := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestInjectHeader_NoSpan(t *testing.T) {
	h := http.Header{}
	InjectHeader(context.Background(), h)
	if h.Get("traceparent") != "" {
		t.Fatal("should not inject without a span")
	}
}

func TestHTTPClient_InjectsTraceparent(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	span := &Span{
		TraceID: "0123456789abcdef0123456789abcdef",
		SpanID:  "0123456789abcdef",
	}
	ctx := context.WithValue(context.Background(), spanCtxKey{}, span)

	u, _ := url.Parse(srv.URL)
	req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)

	client := HTTPClient(nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	resp.Body.Close()

	want := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	if gotHeader != want {
		t.Fatalf("got %q want %q", gotHeader, want)
	}
}

func TestHTTPClient_PreservesExistingTraceparent(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("traceparent")
	}))
	defer srv.Close()

	preset := "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01"
	span := &Span{
		TraceID: "0123456789abcdef0123456789abcdef",
		SpanID:  "0123456789abcdef",
	}
	ctx := context.WithValue(context.Background(), spanCtxKey{}, span)
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	req.Header.Set("traceparent", preset)

	client := HTTPClient(nil)
	resp, _ := client.Do(req)
	resp.Body.Close()

	if gotHeader != preset {
		t.Fatalf("caller's traceparent should win: got %q want %q", gotHeader, preset)
	}
}