package chirouter

import (
	"testing"

	"github.com/paulmanoni/nexus/httpx"
	"github.com/paulmanoni/nexus/httpx/routertest"
)

// The chi adapter must satisfy the shared router seam contract — most notably
// translating chi's "*"-keyed catch-all into the canonical *name param WITH
// gin's leading slash, and resolving named :params. The cross-backend spec
// (named/wildcard params, exact root + sub-path matching, NoRoute, middleware
// order + Abort, groups, Static) lives in routertest.
func TestConformance(t *testing.T) {
	routertest.RunConformance(t, func() httpx.Router { return New() })
}
