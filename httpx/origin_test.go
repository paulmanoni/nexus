package httpx

import (
	"net/http"
	"testing"
)

func req(t *testing.T, host, origin string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, "http://"+host+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

func TestCheckWebSocketOriginSameOrigin(t *testing.T) {
	SetAllowedWebSocketOrigins(nil)
	t.Setenv("NEXUS_DEV", "")

	for _, tc := range []struct {
		name, host, origin string
		want               bool
	}{
		{"no origin (non-browser client)", "api.example.com", "", true},
		{"exact same origin", "api.example.com", "https://api.example.com", true},
		{"scheme differs (TLS terminated upstream)", "api.example.com", "http://api.example.com", true},
		{"same host with port", "localhost:8080", "http://localhost:8080", true},
		{"cross-site attacker", "api.example.com", "https://evil.example", false},
		{"port differs", "localhost:8080", "http://localhost:5173", false},
		{"attacker suffix trick", "api.example.com", "https://api.example.com.evil.tld", false},
		{"attacker prefix trick", "api.example.com", "https://evilapi.example.com", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CheckWebSocketOrigin(req(t, tc.host, tc.origin)); got != tc.want {
				t.Errorf("CheckWebSocketOrigin(host=%s, origin=%s) = %v, want %v",
					tc.host, tc.origin, got, tc.want)
			}
		})
	}
}

func TestCheckWebSocketOriginAllowlist(t *testing.T) {
	t.Setenv("NEXUS_DEV", "")
	SetAllowedWebSocketOrigins([]string{"https://app.example.com", "*.trusted.tld"})
	t.Cleanup(func() { SetAllowedWebSocketOrigins(nil) })

	for _, tc := range []struct {
		name, origin string
		want         bool
	}{
		{"exact allowlisted", "https://app.example.com", true},
		{"wildcard subdomain", "https://a.trusted.tld", true},
		{"wildcard deep subdomain", "https://a.b.trusted.tld", true},
		// A "*.example.com" entry must not admit the parent domain: that
		// would silently widen the allowlist beyond what was written.
		{"wildcard parent not matched", "https://trusted.tld", false},
		// ...nor a domain that merely ends with the same characters.
		{"wildcard suffix trick", "https://eviltrusted.tld", false},
		{"unlisted origin", "https://evil.example", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CheckWebSocketOrigin(req(t, "api.example.com", tc.origin)); got != tc.want {
				t.Errorf("origin %s = %v, want %v", tc.origin, got, tc.want)
			}
		})
	}
}

func TestCheckWebSocketOriginWildcardDisablesCheck(t *testing.T) {
	t.Setenv("NEXUS_DEV", "")
	SetAllowedWebSocketOrigins([]string{"*"})
	t.Cleanup(func() { SetAllowedWebSocketOrigins(nil) })
	if !CheckWebSocketOrigin(req(t, "api.example.com", "https://evil.example")) {
		t.Error(`allowed_origins = ["*"] must restore the accept-everything behavior`)
	}
}

// Under nexus dev the SPA is on :5173 and the app on :8080, so every browser
// upgrade is cross-origin by construction. Loopback is allowed there — and
// only there.
func TestCheckWebSocketOriginDevLoopback(t *testing.T) {
	SetAllowedWebSocketOrigins(nil)

	t.Setenv("NEXUS_DEV", "1")
	for _, origin := range []string{"http://localhost:5173", "http://127.0.0.1:5173", "http://[::1]:5173"} {
		if !CheckWebSocketOrigin(req(t, "localhost:8080", origin)) {
			t.Errorf("dev: origin %s should be allowed", origin)
		}
	}
	if CheckWebSocketOrigin(req(t, "localhost:8080", "https://evil.example")) {
		t.Error("dev loopback allowance must not extend to remote origins")
	}

	t.Setenv("NEXUS_DEV", "")
	if CheckWebSocketOrigin(req(t, "localhost:8080", "http://localhost:5173")) {
		t.Error("outside dev, a different loopback port is still cross-origin")
	}
}
