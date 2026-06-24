package ditest_test

import (
	"testing"

	"github.com/paulmanoni/nexus/di"
	"github.com/paulmanoni/nexus/di/ditest"
)

// The builtin container is the default backend; it must satisfy the same
// conformance suite the fx adapter is held to (see di/fxcontainer).
func TestBuiltinConformance(t *testing.T) {
	ditest.RunConformance(t, di.Builtin())
}
