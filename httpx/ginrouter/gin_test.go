package ginrouter

import (
	"testing"

	"github.com/paulmanoni/nexus/httpx"
	"github.com/paulmanoni/nexus/httpx/routertest"
)

// The gin adapter is opt-in (separate module) but must still satisfy the same
// router seam contract as the stdlib and chi backends. The cross-backend spec
// lives in routertest; this is the gin module's first test coverage.
func TestConformance(t *testing.T) {
	routertest.RunConformance(t, func() httpx.Router { return New() })
}
