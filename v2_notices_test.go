package nexus

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/internal/v2notice"
	"github.com/paulmanoni/nexus/middleware"
)

func v2DevOn(t *testing.T) {
	t.Helper()
	t.Setenv("NEXUS_DEV", "1")
	t.Setenv("NEXUS_V2_NOTICES", "")
	prev := v2notice.Output
	v2notice.Output = io.Discard
	v2notice.Reset()
	t.Cleanup(func() {
		v2notice.Output = prev
		v2notice.Reset()
	})
}

func v2Recorded() string {
	var names []string
	for _, it := range v2notice.Items() {
		names = append(names, it.Name)
	}
	return strings.Join(names, "\n")
}

func TestV2Notice_ErrForbidden(t *testing.T) {
	v2DevOn(t)
	// An Authorize refusal with the app's own error: the framework adds
	// ErrForbidden to the chain, which is not the app using it.
	noteErrForbidden(forbiddenError{errors.New("not yours")})
	if got := v2Recorded(); got != "" {
		t.Fatalf("recorded %q for a framework-wrapped refusal", got)
	}
	noteErrForbidden(forbiddenError{ErrForbidden})
	if got := v2Recorded(); got != "nexus.ErrForbidden" {
		t.Fatalf("recorded %q", got)
	}
}

func TestV2Notice_URITag(t *testing.T) {
	v2DevOn(t)
	type GetArgs struct {
		ID   string `uri:"id"`
		Slug string `path:"slug" uri:"slug"`
	}
	noteURITag(reflect.TypeOf(GetArgs{}))
	got := v2Recorded()
	if !strings.Contains(got, `uri:"id"`) || strings.Contains(got, "slug") {
		t.Fatalf("recorded %q", got)
	}
}

func TestV2Notice_ConfigShapes(t *testing.T) {
	v2DevOn(t)
	cfg := Config{}
	cfg.Middleware.Global = []middleware.Middleware{{Name: "x"}}
	noteGlobalMiddleware(cfg)
	noteChangedDefaults(cfg)
	got := v2Recorded()
	for _, want := range []string{"Config.Middleware.Global", "max_body_bytes"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %q", want, got)
		}
	}
	v2notice.Reset()
	cfg.Server.MaxBodyBytes = 1 << 20
	noteChangedDefaults(cfg)
	if got := v2Recorded(); got != "" {
		t.Fatalf("max_body_bytes set but recorded %q", got)
	}
}
