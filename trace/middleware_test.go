package trace

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRecord_EmitsLeafSpanPairUnderRoot(t *testing.T) {
	bus := NewBus(16)
	_, ch, cancel := bus.Subscribe(0, 16)
	defer cancel()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Middleware(bus, "svc", "op", "rest"))
	r.GET("/x", func(c *gin.Context) {
		Record(c, "db.query", time.Now().Add(-5*time.Millisecond), errors.New("nope"))
		c.String(200, "ok")
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))

	// Expect: request.start, span.start, span.end, request.end.
	events := drain(ch, 4, time.Second)
	if len(events) != 4 {
		t.Fatalf("expected 4 events, got %d: %+v", len(events), events)
	}
	rs, ss, se, re := events[0], events[1], events[2], events[3]
	if rs.Kind != KindRequestStart || ss.Kind != KindSpanStart || se.Kind != KindSpanEnd || re.Kind != KindRequestEnd {
		t.Fatalf("wrong kind order: %s %s %s %s", rs.Kind, ss.Kind, se.Kind, re.Kind)
	}
	// Record's leaf span must parent under the request root.
	if ss.ParentID != rs.SpanID {
		t.Fatalf("leaf ParentID should equal request SpanID: %q vs %q", ss.ParentID, rs.SpanID)
	}
	if ss.TraceID != rs.TraceID {
		t.Fatalf("leaf TraceID should match request: %q vs %q", ss.TraceID, rs.TraceID)
	}
	if se.Error != "nope" {
		t.Fatalf("error not recorded on leaf end: %q", se.Error)
	}
	if ss.Name != "db.query" {
		t.Fatalf("leaf span name wrong: %q", ss.Name)
	}
}

func TestBus_SnapshotByTrace(t *testing.T) {
	b := NewBus(10)
	b.Publish(Event{TraceID: "a", Kind: KindRequestStart})
	b.Publish(Event{TraceID: "b", Kind: KindRequestStart})
	b.Publish(Event{TraceID: "a", Kind: KindSpanEnd})

	got := b.SnapshotByTrace("a")
	if len(got) != 2 {
		t.Fatalf("expected 2 events for trace a, got %d", len(got))
	}
	if got[0].Kind != KindRequestStart || got[1].Kind != KindSpanEnd {
		t.Fatalf("wrong events: %+v", got)
	}
	if b.SnapshotByTrace("missing") != nil {
		t.Fatal("expected nil for missing trace")
	}
	if b.SnapshotByTrace("") != nil {
		t.Fatal("empty traceID should return nil")
	}
}