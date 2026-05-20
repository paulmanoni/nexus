package tour

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestHandleInjectJS confirms the embedded agent is reachable
// over the GET /__nexus/tour/inject.js route. We check for two
// signposts that prove the Phase-2 body landed (not the Phase-1
// placeholder): the boot guard variable name and the recorder
// bar selector.
func TestHandleInjectJS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/__nexus/tour/inject.js", handleInjectJS)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/__nexus/tour/inject.js", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("content-type = %q, want application/javascript", ct)
	}
	body := rec.Body.String()
	for _, signpost := range []string{
		"__nexusTourMounted", // boot guard
		"createRecorder",     // recorder module
		"createRunner",       // runner module
		"play-ring",          // CSS for the play-mode overlay
	} {
		if !strings.Contains(body, signpost) {
			t.Errorf("served body missing %q (Phase-2 agent not embedded?)", signpost)
		}
	}
	// Sanity: it should be substantial, not the 100-byte stub.
	if len(body) < 5000 {
		t.Errorf("served body is %d bytes — looks like the Phase-1 stub, not Phase-2", len(body))
	}
}