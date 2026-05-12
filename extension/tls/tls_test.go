package tls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// TestValidate covers the cases an operator can hit in production:
// empty Domains, wildcard, missing email outside staging, conflicting
// cache config. Each one should produce an error message that names
// the field — operators read the message in their boot logs.
func TestValidate(t *testing.T) {
	t.Parallel()
	t.Run("empty domains", func(t *testing.T) {
		err := validate(&Config{Email: "ops@example.com"})
		if err == nil || !strings.Contains(err.Error(), "Domains") {
			t.Fatalf("want Domains error, got %v", err)
		}
	})

	t.Run("wildcard rejected", func(t *testing.T) {
		err := validate(&Config{
			Domains: []string{"*.example.com"},
			Email:   "ops@example.com",
		})
		if err == nil || !strings.Contains(err.Error(), "wildcard") {
			t.Fatalf("want wildcard error, got %v", err)
		}
	})

	t.Run("email required in production", func(t *testing.T) {
		err := validate(&Config{Domains: []string{"app.example.com"}})
		if err == nil || !strings.Contains(err.Error(), "Email") {
			t.Fatalf("want Email error, got %v", err)
		}
	})

	t.Run("staging may omit email", func(t *testing.T) {
		err := validate(&Config{
			Domains: []string{"app.example.com"},
			Staging: true,
		})
		if err != nil {
			t.Fatalf("staging should permit empty email, got %v", err)
		}
	})

	t.Run("cache and cacheDir conflict", func(t *testing.T) {
		err := validate(&Config{
			Domains:  []string{"app.example.com"},
			Email:    "ops@example.com",
			CacheDir: "./certs",
			Cache:    autocert.DirCache("/tmp/foo"),
		})
		if err == nil || !strings.Contains(err.Error(), "Cache") {
			t.Fatalf("want cache conflict error, got %v", err)
		}
	})

	t.Run("trims whitespace and rejects empty domain", func(t *testing.T) {
		err := validate(&Config{
			Domains: []string{"app.example.com", "   "},
			Email:   "ops@example.com",
		})
		if err == nil || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("want empty-domain error, got %v", err)
		}
	})
}

// TestApplyDefaults locks in the defaults so a future refactor that
// changes them is at least visible in the test diff.
func TestApplyDefaults(t *testing.T) {
	t.Parallel()
	cfg := &Config{Domains: []string{"a.example.com"}, Email: "x@y.com"}
	applyDefaults(cfg)
	if cfg.HTTPSPort != 443 {
		t.Errorf("HTTPSPort default: got %d, want 443", cfg.HTTPSPort)
	}
	if cfg.HTTPPort != 80 {
		t.Errorf("HTTPPort default: got %d, want 80", cfg.HTTPPort)
	}
	if cfg.Redirect == nil || !*cfg.Redirect {
		t.Errorf("Redirect default: want true, got %v", cfg.Redirect)
	}
	if cfg.Cache == nil {
		t.Error("Cache should default to DirCache('./certs')")
	}
	if cfg.AcceptTOS == nil {
		t.Error("AcceptTOS should default to autocert.AcceptTOS")
	}
}

// TestContainsDomain exercises the case-insensitive whitelist check
// used by handleRenew. Operators frequently type domains in mixed
// case while testing in the browser; the whitelist match must not
// be a source of false negatives.
func TestContainsDomain(t *testing.T) {
	t.Parallel()
	whitelist := []string{"app.example.com", "api.example.com"}
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"app.example.com", true},
		{"APP.EXAMPLE.COM", true},
		{"Api.Example.com", true},
		{"other.example.com", false},
		{"app.example.com.attacker.com", false},
		{"", false},
	} {
		if got := containsDomain(whitelist, tc.in); got != tc.want {
			t.Errorf("containsDomain(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestStripPort handles the three Host header shapes we expect to
// see from real clients: plain hostname, hostname:port, and IPv6
// bracketed form with optional port.
func TestStripPort(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"example.com", "example.com"},
		{"example.com:80", "example.com"},
		{"[::1]", "[::1]"},
		{"[::1]:443", "[::1]"},
		{"localhost:8080", "localhost"},
	}
	for _, tc := range cases {
		if got := stripPort(tc.in); got != tc.want {
			t.Errorf("stripPort(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestInspectCert builds a self-signed cert in the same on-disk
// format autocert writes (CERTIFICATE block + PRIVATE KEY block),
// then exercises inspectCert's parse path. This catches the most
// common breakage — autocert changing the cache encoding — without
// requiring an actual ACME round-trip.
func TestInspectCert_ParsesCacheEntry(t *testing.T) {
	t.Parallel()
	domain := "app.example.com"
	notAfter := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)

	entry := buildSelfSignedCacheEntry(t, domain, notAfter)

	cache := autocert.DirCache(t.TempDir())
	if err := cache.Put(context.Background(), domain, entry); err != nil {
		t.Fatal(err)
	}

	info := inspectCert(context.Background(), &autocert.Manager{}, cache, domain)
	if !info.Present {
		t.Fatalf("Present=false, want true; info=%+v", info)
	}
	// NotAfter comparison must tolerate second-precision rounding on
	// the wire — both sides truncate to second to be sure.
	if !info.NotAfter.Equal(notAfter) {
		t.Errorf("NotAfter: got %v, want %v", info.NotAfter, notAfter)
	}
	if info.DaysLeft < 29 || info.DaysLeft > 30 {
		t.Errorf("DaysLeft: got %d, want ~30", info.DaysLeft)
	}
}

// TestInspectCert_MissingReturnsNotPresent ensures the "not yet
// issued" path returns Present=false WITHOUT an error so the
// dashboard can render an "issuing…" state cleanly.
func TestInspectCert_MissingReturnsNotPresent(t *testing.T) {
	t.Parallel()
	cache := autocert.DirCache(t.TempDir())
	info := inspectCert(context.Background(), &autocert.Manager{}, cache, "fresh.example.com")
	if info.Present {
		t.Errorf("Present=true for missing entry; want false")
	}
	if info.Error != "" {
		t.Errorf("Error %q for missing entry; want empty (ErrCacheMiss is normal)", info.Error)
	}
}

// buildSelfSignedCacheEntry produces a PEM blob in the exact shape
// autocert.DirCache stores: one CERTIFICATE block followed by one
// EC PRIVATE KEY block. inspectCert reads only the first CERTIFICATE
// block, so the private key portion is not strictly required for
// parsing — but we include it so the test fixture matches what real
// autocert writes, which protects us from "test passes, prod breaks
// because the real entry has extra leading whitespace" style drift.
func buildSelfSignedCacheEntry(t *testing.T, domain string, notAfter time.Time) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domain},
		Issuer:       pkix.Name{CommonName: "Test CA"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     notAfter,
		DNSNames:     []string{domain},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	out = append(out, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})...)
	return out
}
