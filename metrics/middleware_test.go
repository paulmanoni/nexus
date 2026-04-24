package metrics

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/trace"
)

// TestGinRecorder_Publishes4xxStatus proves the metrics recorder
// emits the ACTUAL HTTP status on request.op — not the old
// 200-or-500 bucket. Regression guard for the case where a REST
// handler writes c.JSON(400, ...) and the dashboard animator needs
// to see that as a failure.
func TestGinRecorder_Publishes4xxStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bus := trace.NewBus(16)
	_, ch, cancel := bus.Subscribe(0, 16)
	defer cancel()

	store := NewMemoryStore()
	eng := gin.New()
	eng.GET("/boom", ginTraceBus(bus), ginRecorder(store, "svc.boom"), func(c *gin.Context) {
		c.JSON(400, gin.H{"error": "bad"})
	})

	req := httptest.NewRequest("GET", "/boom", nil)
	w := httptest.NewRecorder()
	eng.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Fatalf("response code = %d; want 400", w.Code)
	}

	// Pull events until we see request.op.
	var sawOp bool
	for i := 0; i < 8 && !sawOp; i++ {
		select {
		case ev := <-ch:
			if ev.Kind != "request.op" {
				continue
			}
			sawOp = true
			if ev.Status != 400 {
				t.Errorf("request.op status = %d; want 400", ev.Status)
			}
			if ev.Error == "" {
				t.Errorf("request.op error should be non-empty for 4xx")
			}
		default:
			// drain quickly; nothing to wait for
		}
	}
	if !sawOp {
		t.Fatal("request.op event never fired")
	}
}

// ginTraceBus mirrors what the REST transport does: attach the bus to
// context so publishOpEventWithStatus can find it.
func ginTraceBus(bus *trace.Bus) gin.HandlerFunc {
	return trace.Middleware(bus, "svc", "GET /boom", "rest")
}