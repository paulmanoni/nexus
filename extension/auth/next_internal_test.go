package auth

import "testing"

func TestSafeNext(t *testing.T) {
	for _, ok := range []string{"/", "/orders", "/orders?page=2&q=a%20b", "/a#b", "/admin/users/42/edit"} {
		if safeNext(ok) != ok {
			t.Errorf("safeNext(%q) refused a same-site path", ok)
		}
	}
	for _, bad := range []string{
		"", "orders", "//evil.com", "/\\evil.com", "\\\\evil.com", "https://evil.com", "javascript:alert(1)",
		"/%2F%2Fevil.com", "/%5Cevil.com", "/%252F%252Fevil.com", "%2F%2Fevil.com",
		"/a\x00b", "/a\nb", "/%0d%0aSet-Cookie:x", "///evil.com", "/%2525252F%2525252Fevil",
	} {
		if got := safeNext(bad); got != "" {
			t.Errorf("safeNext(%q) = %q, want refused", bad, got)
		}
	}
}

func TestParseLimit(t *testing.T) {
	if l, err := parseLimit("account", "", "5/15m"); err != nil || l.n != 5 || l.window.Minutes() != 15 {
		t.Fatalf("default = %+v, %v", l, err)
	}
	if l, err := parseLimit("ip", "off", "50/15m"); err != nil || l.n != 0 {
		t.Fatalf("off = %+v, %v", l, err)
	}
	for _, bad := range []string{"5", "x/1m", "5/x", "0/1m"} {
		if _, err := parseLimit("account", bad, ""); err == nil {
			t.Errorf("parseLimit(%q) accepted", bad)
		}
	}
}
