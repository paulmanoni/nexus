package client

import (
	"testing"

	"github.com/paulmanoni/nexus/registry"
)

// TestAuthMetaOverlay pins the auth.Config → manifest AuthInfo wiring:
// SetAuthMeta's non-empty fields land on the built manifest's Auth
// section (the same Auth the public/projected manifest carries), while
// empty fields leave the SDK defaults in place. The Auth section is
// created by buildManifest only when an authInfo provider is set, so
// the handler wires one (mirroring auth.Module's SetAuthInfo bridge).
func TestAuthMetaOverlay(t *testing.T) {
	reg := registry.New()
	h := &Handler{
		reg:      reg,
		authInfo: func() ExtractorInfo { return ExtractorInfo{Strategy: "cookie", CookieName: "session"} },
	}

	// No meta → no overlay; the hint fields stay zero so the SDK falls
	// back to its own heuristics / defaults.
	if got := h.Manifest(); got.Auth == nil {
		t.Fatal("Auth section missing despite authInfo provider")
	} else if got.Auth.TokenField != "" || got.Auth.CSRFCookie != "" || got.Auth.CSRFHeader != "" {
		t.Fatalf("unexpected hints before overlay: %+v", got.Auth)
	}

	h.SetAuthMeta(AuthMeta{
		TokenField: "data.token",
		CSRFCookie: "csrftoken",
		CSRFHeader: "X-CSRFToken",
	})

	m := h.Manifest()
	if m.Auth == nil {
		t.Fatal("Auth section missing after overlay")
	}
	if m.Auth.TokenField != "data.token" {
		t.Errorf("TokenField = %q, want %q", m.Auth.TokenField, "data.token")
	}
	if m.Auth.CSRFCookie != "csrftoken" {
		t.Errorf("CSRFCookie = %q, want %q", m.Auth.CSRFCookie, "csrftoken")
	}
	if m.Auth.CSRFHeader != "X-CSRFToken" {
		t.Errorf("CSRFHeader = %q, want %q", m.Auth.CSRFHeader, "X-CSRFToken")
	}
	// The extractor strategy from the provider must survive the overlay.
	if m.Auth.Strategy != "cookie" {
		t.Errorf("Strategy = %q, want %q (overlay clobbered the extractor)", m.Auth.Strategy, "cookie")
	}
}

// TestAuthMetaEmpty guards the Empty() skip-path used to avoid a
// needless overlay (and cache churn) when auth.Config set no hints.
func TestAuthMetaEmpty(t *testing.T) {
	if !(AuthMeta{}).Empty() {
		t.Error("zero AuthMeta should report Empty")
	}
	if (AuthMeta{CSRFHeader: "X"}).Empty() {
		t.Error("AuthMeta with a field set should not report Empty")
	}
}

// TestAuthMetaWithDefaults pins the framework defaults auth.Module
// applies: empty fields get the Django-style CSRF pair + data.token,
// while explicitly-set fields are preserved.
func TestAuthMetaWithDefaults(t *testing.T) {
	got := (AuthMeta{}).WithDefaults()
	if got.TokenField != DefaultTokenField || got.CSRFCookie != DefaultCSRFCookie || got.CSRFHeader != DefaultCSRFHeader {
		t.Fatalf("zero meta defaults = %+v, want %q/%q/%q", got, DefaultTokenField, DefaultCSRFCookie, DefaultCSRFHeader)
	}
	if DefaultCSRFCookie != "csrftoken" || DefaultCSRFHeader != "X-CSRFToken" || DefaultTokenField != "data.token" {
		t.Fatalf("default constants drifted: %q/%q/%q", DefaultTokenField, DefaultCSRFCookie, DefaultCSRFHeader)
	}
	// Explicit values survive.
	set := AuthMeta{TokenField: "session.jwt", CSRFCookie: "XSRF-TOKEN", CSRFHeader: "X-XSRF-TOKEN"}
	if got := set.WithDefaults(); got != set {
		t.Errorf("WithDefaults clobbered explicit values: %+v", got)
	}
}
