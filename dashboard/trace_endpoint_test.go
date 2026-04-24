package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/trace"
)

// publishTrace seeds a bus with a realistic request/span event sequence
// for one trace so we can exercise the /__nexus/traces/:id endpoint.
func publishTrace(bus *trace.Bus, traceID string) {
	base := time.Now()
	// Root request span.
	rootID := "1111111111111111"
	bus.Publish(trace.Event{
		TraceID: traceID, SpanID: rootID, Kind: trace.KindRequestStart,
		Service: "svc", Endpoint: "GET /x", Name: "GET /x", Transport: "rest",
		Timestamp: base,
	})
	// Child span under root.
	childID := "2222222222222222"
	bus.Publish(trace.Event{
		TraceID: traceID, SpanID: childID, ParentID: rootID, Kind: trace.KindSpanStart,
		Service: "svc", Endpoint: "GET /x", Name: "db.query",
		Timestamp: base.Add(2 * time.Millisecond),
	})
	bus.Publish(trace.Event{
		TraceID: traceID, SpanID: childID, ParentID: rootID, Kind: trace.KindSpanEnd,
		Service: "svc", Endpoint: "GET /x", Name: "db.query",
		DurationMs: 3, Timestamp: base.Add(5 * time.Millisecond),
		Meta: map[string]any{"rows": float64(12)},
	})
	bus.Publish(trace.Event{
		TraceID: traceID, SpanID: rootID, Kind: trace.KindRequestEnd,
		Service: "svc", Endpoint: "GET /x", Name: "GET /x", Transport: "rest",
		DurationMs: 10, Status: 200,
	})
}

func TestTraceByID_ReturnsWaterfall(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	bus := trace.NewBus(32)
	Mount(e, registry.New(), bus, nil, nil, nil, Config{})

	publishTrace(bus, "tid-1")

	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", Prefix+"/traces/tid-1", nil))
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}

	var body struct {
		TraceID string `json:"traceId"`
		Spans   []struct {
			SpanID     string         `json:"spanId"`
			ParentID   string         `json:"parentId"`
			Name       string         `json:"name"`
			Kind       string         `json:"kind"`
			StartMs    int64          `json:"startMs"`
			DurationMs int64          `json:"durationMs"`
			Status     int            `json:"status"`
			Attrs      map[string]any `json:"attrs"`
		} `json:"spans"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if body.TraceID != "tid-1" {
		t.Fatalf("traceId: %q", body.TraceID)
	}
	if len(body.Spans) != 2 {
		t.Fatalf("expected 2 spans, got %d: %s", len(body.Spans), w.Body.String())
	}
	// Root first (StartMs=0), child second.
	if body.Spans[0].SpanID != "1111111111111111" || body.Spans[0].ParentID != "" {
		t.Fatalf("root mismatch: %+v", body.Spans[0])
	}
	if body.Spans[1].ParentID != "1111111111111111" || body.Spans[1].Name != "db.query" {
		t.Fatalf("child mismatch: %+v", body.Spans[1])
	}
	if body.Spans[1].StartMs < body.Spans[0].StartMs {
		t.Fatalf("child should start after root: %d < %d", body.Spans[1].StartMs, body.Spans[0].StartMs)
	}
	if body.Spans[1].DurationMs != 3 {
		t.Fatalf("child duration wrong: %d", body.Spans[1].DurationMs)
	}
	if body.Spans[1].Attrs["rows"].(float64) != 12 {
		t.Fatalf("attrs not surfaced: %+v", body.Spans[1].Attrs)
	}
	if body.Spans[0].Status != 200 {
		t.Fatalf("root status not surfaced: %d", body.Spans[0].Status)
	}
}

func TestTraceByID_NotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	e := gin.New()
	bus := trace.NewBus(16)
	Mount(e, registry.New(), bus, nil, nil, nil, Config{})

	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest("GET", Prefix+"/traces/missing", nil))
	if w.Code != 404 {
		t.Fatalf("want 404, got %d", w.Code)
	}
}