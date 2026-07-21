package auth_test

import (
	"reflect"
	"testing"

	"github.com/paulmanoni/nexus/extension/auth"
)

// TestDescribeAlias pins the deprecation contract: the old auth.Describe
// must keep returning exactly what the renamed auth.InspectExtractor does,
// so existing callers behave identically until Describe is removed.
func TestDescribeAlias(t *testing.T) {
	for _, ex := range []auth.Extractor{auth.Bearer(), auth.Cookie("sid"), auth.APIKey("X-Key"), nil} {
		got := auth.Describe(ex)
		want := auth.InspectExtractor(ex)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Describe(%v) = %+v, InspectExtractor = %+v — alias diverged", ex, got, want)
		}
	}
	if s := auth.InspectExtractor(auth.Bearer()).Strategy; s != "bearer" {
		t.Errorf("Bearer strategy = %q, want bearer", s)
	}
}
