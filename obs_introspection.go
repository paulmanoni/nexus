package nexus

import (
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
)

// devModeBypass reports whether the framework is running under
// `nexus dev` (or a caller that explicitly set NEXUS_DEV=1). When
// true, the introspection gate is fully open — dashboards always
// work in dev. Production binaries never see NEXUS_DEV=1, so the
// strict-by-default stance is preserved where it matters.
func devModeBypass() bool {
	return os.Getenv(NexusDevEnv) == "1"
}

// parseIntrospectionNetworks compiles each CIDR string into a
// *net.IPNet for O(1) membership checks per request. Invalid CIDRs
// fail fast with a wrapped error so the operator sees the problem
// at boot rather than at the first dashboard request.
//
// Empty input returns (nil, nil) — the gate logic treats nil as
// "no allowlist", meaning Introspection alone decides access.
func parseIntrospectionNetworks(cidrs []string) ([]*net.IPNet, error) {
	if len(cidrs) == 0 {
		return nil, nil
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, fmt.Errorf("nexus: IntrospectionNetworks[%q]: %w", cidr, err)
		}
		out = append(out, network)
	}
	return out, nil
}

// introspectionAllowed reports whether the request should bypass
// the Introspection gate. introspect=true short-circuits true (the
// flag opens everything globally); otherwise the request's TCP
// peer (RemoteIP — unspoofable; X-Forwarded-For ignored) is matched
// against the pre-parsed networks.
//
// Empty networks + introspect=false = strict mode: every gated
// route 404s.
func introspectionAllowed(c *gin.Context, introspect bool, networks []*net.IPNet) bool {
	if introspect {
		return true
	}
	if len(networks) == 0 {
		return false
	}
	// RemoteIP is the actual TCP peer. ClientIP would honor
	// X-Forwarded-For if Gin's TrustedProxies is configured, which
	// is spoofable by default — wrong default for a security gate.
	ip := net.ParseIP(c.RemoteIP())
	if ip == nil {
		return false
	}
	for _, n := range networks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// introspectionGate returns a gin.HandlerFunc that 404s requests
// when the Introspection gate is closed AND the peer IP is not in
// the allowlist. 404 (rather than 401/403) is intentional — it
// makes the gated routes look indistinguishable from "never
// mounted" to anonymous scanners, which removes a useful signal
// from probe traffic.
//
// Returns nil when the gate is fully open (Introspection: true) —
// the caller skips installing the middleware in that case so the
// hot path stays empty for dev/internal deploys that don't need
// gating.
func introspectionGate(introspect bool, networks []*net.IPNet) gin.HandlerFunc {
	if introspect {
		return nil
	}
	// `nexus dev` sets NEXUS_DEV=1 on its child subprocess. Lifting the
	// gate in that case removes the most common dev footgun: boot the
	// app via the CLI, the banner says "ready, opening browser", browser
	// hits /__nexus/, gets 404 because no IntrospectionNetworks was set.
	// Production binaries never see NEXUS_DEV=1.
	if devModeBypass() {
		return nil
	}
	return func(c *gin.Context) {
		if introspectionAllowed(c, false, networks) {
			c.Next()
			return
		}
		// AbortWithStatus over c.JSON: 404 carries no body so the
		// response is byte-equivalent to a missing route.
		c.AbortWithStatus(http.StatusNotFound)
	}
}
