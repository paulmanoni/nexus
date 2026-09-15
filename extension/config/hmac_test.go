package config

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestVerifyConfigHMAC(t *testing.T) {
	const app, secret, path = "myapp", "s3cret", "/__config/snapshot/myapp/default"

	t.Run("valid round-trip", func(t *testing.T) {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", configHMACHeader(app, path, secret, time.Now()))
		if err := verifyConfigHMAC(r, app, secret); err != nil {
			t.Fatalf("valid signature rejected: %v", err)
		}
	})

	t.Run("wrong secret", func(t *testing.T) {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", configHMACHeader(app, path, "other", time.Now()))
		if err := verifyConfigHMAC(r, app, secret); err == nil {
			t.Fatal("signature under the wrong secret accepted")
		}
	})

	t.Run("wrong app", func(t *testing.T) {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", configHMACHeader("otherapp", path, secret, time.Now()))
		if err := verifyConfigHMAC(r, app, secret); err == nil {
			t.Fatal("signature for another app accepted")
		}
	})

	t.Run("path mismatch", func(t *testing.T) {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization",
			configHMACHeader(app, "/__config/snapshot/myapp/prod", secret, time.Now()))
		if err := verifyConfigHMAC(r, app, secret); err == nil {
			t.Fatal("signature over a different path accepted")
		}
	})

	t.Run("stale timestamp", func(t *testing.T) {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization",
			configHMACHeader(app, path, secret, time.Now().Add(-2*hmacMaxSkew)))
		if err := verifyConfigHMAC(r, app, secret); err == nil {
			t.Fatal("timestamp outside the skew window accepted")
		}
	})

	t.Run("arbitrary bearer rejected", func(t *testing.T) {
		// The phase-1 stub accepted any non-empty header; make sure
		// that never comes back.
		for _, h := range []string{"Nexus-Config-HMAC stub", "Bearer xyz", "garbage"} {
			r := httptest.NewRequest("GET", path, nil)
			r.Header.Set("Authorization", h)
			if err := verifyConfigHMAC(r, app, secret); err == nil {
				t.Fatalf("header %q accepted", h)
			}
		}
	})

	t.Run("no secret configured", func(t *testing.T) {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", configHMACHeader(app, path, "", time.Now()))
		if err := verifyConfigHMAC(r, app, ""); err == nil {
			t.Fatal("empty secret accepted")
		}
	})
}
