package httpx

import (
	"net/http/httptest"
	"testing"
)

func TestClientIPTrustedProxyPolicy(t *testing.T) {
	// Restore the default policy after each mutation below.
	t.Cleanup(func() { trustedProxyPrefixes.Store(nil) })

	req := func(remote, xff, xrealip string) string {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		if xrealip != "" {
			r.Header.Set("X-Real-IP", xrealip)
		}
		return clientIP(r)
	}

	t.Run("direct public client cannot spoof via XFF", func(t *testing.T) {
		if got := req("203.0.113.9:4444", "1.2.3.4", ""); got != "203.0.113.9" {
			t.Fatalf("got %q, want the socket peer", got)
		}
	})

	t.Run("direct public client cannot spoof via X-Real-IP", func(t *testing.T) {
		if got := req("203.0.113.9:4444", "", "1.2.3.4"); got != "203.0.113.9" {
			t.Fatalf("got %q, want the socket peer", got)
		}
	})

	t.Run("trusted LB forwards the real client", func(t *testing.T) {
		if got := req("10.0.0.5:1234", "203.0.113.9", ""); got != "203.0.113.9" {
			t.Fatalf("got %q, want the forwarded client", got)
		}
	})

	t.Run("client-prepended XFF entries are ignored", func(t *testing.T) {
		// Client sent "X-Forwarded-For: 1.2.3.4" and the LB appended
		// the real address — the rightmost untrusted hop wins.
		if got := req("10.0.0.5:1234", "1.2.3.4, 203.0.113.9", ""); got != "203.0.113.9" {
			t.Fatalf("got %q, want the LB-appended client", got)
		}
	})

	t.Run("chain of trusted proxies is walked through", func(t *testing.T) {
		if got := req("10.0.0.5:1234", "203.0.113.9, 10.0.0.6", ""); got != "203.0.113.9" {
			t.Fatalf("got %q, want the first untrusted hop", got)
		}
	})

	t.Run("no forwarded headers falls back to peer", func(t *testing.T) {
		if got := req("10.0.0.5:1234", "", ""); got != "10.0.0.5" {
			t.Fatalf("got %q, want the socket peer", got)
		}
	})

	t.Run("explicit empty allowlist trusts nobody", func(t *testing.T) {
		if bad := SetTrustedProxies(nil); len(bad) != 0 {
			t.Fatalf("unexpected invalid entries: %v", bad)
		}
		defer trustedProxyPrefixes.Store(nil)
		if got := req("10.0.0.5:1234", "203.0.113.9", ""); got != "10.0.0.5" {
			t.Fatalf("got %q, want the socket peer under trust-none", got)
		}
	})

	t.Run("custom CIDR and single-IP entries", func(t *testing.T) {
		if bad := SetTrustedProxies([]string{"198.51.100.0/24", "203.0.113.7"}); len(bad) != 0 {
			t.Fatalf("unexpected invalid entries: %v", bad)
		}
		defer trustedProxyPrefixes.Store(nil)
		if got := req("198.51.100.9:80", "9.9.9.9", ""); got != "9.9.9.9" {
			t.Fatalf("CIDR-trusted proxy not honored: got %q", got)
		}
		if got := req("203.0.113.7:80", "9.9.9.9", ""); got != "9.9.9.9" {
			t.Fatalf("single-IP-trusted proxy not honored: got %q", got)
		}
		if got := req("10.0.0.5:80", "9.9.9.9", ""); got != "10.0.0.5" {
			t.Fatalf("default ranges should be replaced, not extended: got %q", got)
		}
	})

	t.Run("invalid entries are reported", func(t *testing.T) {
		bad := SetTrustedProxies([]string{"not-a-cidr", "10.0.0.0/8"})
		trustedProxyPrefixes.Store(nil)
		if len(bad) != 1 || bad[0] != "not-a-cidr" {
			t.Fatalf("got invalid=%v", bad)
		}
	})

	t.Run("malformed XFF stops header trust", func(t *testing.T) {
		if got := req("10.0.0.5:1234", "garbage, 203.0.113.9", ""); got != "203.0.113.9" {
			t.Fatalf("got %q — rightmost valid untrusted hop should win", got)
		}
		if got := req("10.0.0.5:1234", "203.0.113.9, garbage", ""); got != "10.0.0.5" {
			t.Fatalf("got %q — a malformed trusted-side hop must fall back to the peer", got)
		}
	})
}
