package httpx

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
)

// X-Forwarded-For / X-Real-IP are client-writable headers: believing
// them unconditionally lets any direct client pick its own "IP" —
// bypassing per-IP rate limits and minting unbounded limiter state.
// ClientIP therefore only honors forwarded headers when the DIRECT
// peer (RemoteAddr) is a trusted proxy, and resolves X-Forwarded-For
// right-to-left, skipping trusted hops — the standard defense against
// a client prepending fake entries that a real proxy then appends to.

// trustedProxyPrefixes holds the operator's proxy CIDR set. Atomic for
// lockless per-request reads; written once at boot from Config.
var trustedProxyPrefixes atomic.Pointer[[]netip.Prefix]

// defaultTrustedProxies covers loopback plus the private ranges —
// the common "load balancer in the same VPC" deployment. A direct
// public-internet client is never inside it, so its forwarded
// headers are ignored.
var defaultTrustedProxies = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
}

// SetTrustedProxies installs the operator's proxy allowlist
// ([runtime.server] trusted_proxies). Forwarded headers are honored
// only when the connecting peer falls inside one of these CIDRs
// (single IPs are accepted and treated as /32 / /128). An explicit
// empty list trusts no proxy at all — ClientIP then always returns
// the socket peer. Invalid entries are skipped and returned so boot
// can report them. Unset, the default is loopback + private ranges.
func SetTrustedProxies(cidrs []string) (invalid []string) {
	prefixes := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if p, err := netip.ParsePrefix(c); err == nil {
			prefixes = append(prefixes, p)
			continue
		}
		if a, err := netip.ParseAddr(c); err == nil {
			prefixes = append(prefixes, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		invalid = append(invalid, c)
	}
	trustedProxyPrefixes.Store(&prefixes)
	return invalid
}

func isTrustedProxy(addr netip.Addr) bool {
	prefixes := defaultTrustedProxies
	if p := trustedProxyPrefixes.Load(); p != nil {
		prefixes = *p
	}
	addr = addr.Unmap()
	for _, pre := range prefixes {
		if pre.Contains(addr) {
			return true
		}
	}
	return false
}

// clientIP resolves the request's client IP under the trusted-proxy
// policy. The socket peer is the anchor; forwarded headers only move
// the answer when that peer is trusted.
func clientIP(r *http.Request) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peer = host
	}
	peerAddr, err := netip.ParseAddr(peer)
	if err != nil || !isTrustedProxy(peerAddr) {
		return peer
	}
	// Walk X-Forwarded-For right to left: the rightmost entry was
	// appended by our own proxy; keep stepping over trusted hops and
	// return the first address a trusted proxy vouched for. Entries
	// further left are client-supplied and unverifiable.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		hops := strings.Split(xff, ",")
		for i := len(hops) - 1; i >= 0; i-- {
			hop := strings.TrimSpace(hops[i])
			a, err := netip.ParseAddr(hop)
			if err != nil {
				break // malformed hop — stop believing the header
			}
			if i == 0 || !isTrustedProxy(a) {
				return hop
			}
		}
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		if _, err := netip.ParseAddr(xr); err == nil {
			return xr
		}
	}
	return peer
}
