package httpx

import (
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
)

// WebSocket upgrades are NOT protected by the same-origin policy: a browser
// will happily let any page open a socket to any host, and CORS never enters
// the picture. The handshake carries cookies, so an upgrader that accepts
// every Origin lets an attacker's page connect as the victim and read (or
// send) whatever the socket carries — cross-site WebSocket hijacking.
//
// gorilla/websocket's own default rejects cross-origin upgrades for exactly
// this reason; a `CheckOrigin: func(*http.Request) bool { return true }` is
// the standard way that protection gets switched off. These helpers are the
// shared policy every nexus upgrader uses instead.

// allowedWSOrigins holds the operator-configured extra origins. Stored in an
// atomic so the HTTP path reads it without a lock; written once at boot from
// Config before any listener binds.
var allowedWSOrigins atomic.Pointer[[]string]

// SetAllowedWebSocketOrigins installs the operator's allowlist
// ([runtime.websocket] allowed_origins). Entries are matched against the
// request's Origin header:
//
//   - "https://app.example.com" — exact scheme+host match
//   - "*.example.com"           — any subdomain, scheme-insensitive
//   - "*"                       — disable the check entirely (opt in to the
//     pre-1.39 behavior; only safe when the socket carries no
//     ambient authority, i.e. no cookies and no session)
//
// Called by the framework at boot; apps configure this through nexus.toml.
func SetAllowedWebSocketOrigins(origins []string) {
	cp := append([]string(nil), origins...)
	allowedWSOrigins.Store(&cp)
}

// CheckWebSocketOrigin is the default CheckOrigin for every nexus upgrader:
// same-origin, plus anything the operator allowlisted, plus loopback under
// NEXUS_DEV so a dev frontend on :5173 can reach an app on :8080.
//
// A request with no Origin header is allowed: non-browser clients (a Go
// client, a CLI, a mobile app) don't send one, and they aren't subject to the
// attack this defends against — only browsers attach ambient credentials to a
// cross-site request.
func CheckWebSocketOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // not a browser-initiated upgrade
	}
	if sameOrigin(origin, r) {
		return true
	}
	if list := allowedWSOrigins.Load(); list != nil {
		for _, allowed := range *list {
			if allowed == "*" || originMatches(origin, allowed) {
				return true
			}
		}
	}
	// Dev: the SPA is served by viteless on :5173 while the app listens on
	// :8080, so every upgrade from the browser is cross-origin by
	// construction. Both ends are the developer's own machine.
	return devMode() && isLoopback(origin)
}

// sameOrigin reports whether the Origin header names the same host the
// request was addressed to. Ports are compared, schemes are not — a page on
// https://x reaching ws://x is the same site, and TLS termination upstream
// routinely makes the two disagree.
func sameOrigin(origin string, r *http.Request) bool {
	oh := hostOf(origin)
	if oh == "" {
		return false
	}
	return strings.EqualFold(oh, r.Host)
}

// originMatches applies one allowlist entry to an Origin header value.
func originMatches(origin, allowed string) bool {
	if strings.EqualFold(origin, allowed) {
		return true
	}
	oh, ah := hostOf(origin), hostOf(allowed)
	if oh == "" {
		return false
	}
	if ah == "" {
		ah = allowed // bare host / wildcard pattern, no scheme
	}
	if strings.HasPrefix(ah, "*.") {
		// "*.example.com" matches sub.example.com but NOT example.com —
		// matching the parent would silently widen the allowlist.
		suffix := ah[1:] // ".example.com"
		host := stripPort(oh)
		return strings.HasSuffix(strings.ToLower(host), strings.ToLower(suffix))
	}
	return strings.EqualFold(oh, ah)
}

// hostOf pulls "host:port" out of an origin value, tolerating a missing
// scheme. Returns "" for values that don't parse as an origin at all.
func hostOf(origin string) string {
	s := origin
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	// An origin has no path, but be forgiving about a stray trailing slash.
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return s
}

func stripPort(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// isLoopback reports whether an origin points at this machine.
func isLoopback(origin string) bool {
	host := stripPort(hostOf(origin))
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func devMode() bool { return os.Getenv("NEXUS_DEV") == "1" }
