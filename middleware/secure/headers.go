package secure

import (
	"strconv"
	"strings"

	"github.com/paulmanoni/nexus/httpx"
)

// HeadersHandler builds the per-request security-headers middleware. It
// precomputes every value once (nothing depends on the request) and
// sets them before the route handler runs, so a handler that writes its
// own header of the same name still wins. Call ApplyHeaderDefaults on
// the config first.
func HeadersHandler(h *HeadersConfig) httpx.HandlerFunc {
	frame := active(h.FrameOptions)
	referrer := active(h.ReferrerPolicy)
	nosniff := h.ContentTypeNosniff == nil || *h.ContentTypeNosniff
	csp := h.ContentSecurityPolicy
	perm := h.PermissionsPolicy
	coop := h.CrossOriginOpenerPolicy
	var hsts string
	if h.HSTS != nil {
		hsts = BuildHSTS(h.HSTS)
	}

	return func(c *httpx.Ctx) {
		if frame != "" {
			c.Header("X-Frame-Options", frame)
		}
		if nosniff {
			c.Header("X-Content-Type-Options", "nosniff")
		}
		if referrer != "" {
			c.Header("Referrer-Policy", referrer)
		}
		if hsts != "" {
			c.Header("Strict-Transport-Security", hsts)
		}
		if csp != "" {
			c.Header("Content-Security-Policy", csp)
		}
		if perm != "" {
			c.Header("Permissions-Policy", perm)
		}
		if coop != "" {
			c.Header("Cross-Origin-Opener-Policy", coop)
		}
		c.Next()
	}
}

// active resolves a header value for emission: Omit ("-", the explicit
// opt-out) and "" both mean "send nothing". Defaults run before this, so
// "" only survives for a header that has no default (correctly omitted).
func active(v string) string {
	if v == Omit {
		return ""
	}
	return v
}

// BuildHSTS renders the Strict-Transport-Security directive.
func BuildHSTS(h *HSTSConfig) string {
	age := h.MaxAge
	if age == 0 {
		age = DefaultHSTSMaxAge
	}
	var b strings.Builder
	b.WriteString("max-age=")
	b.WriteString(strconv.Itoa(age))
	if h.IncludeSubdomains || h.Preload {
		// Preload requires includeSubDomains per the preload-list rules,
		// so emit it whenever either is set.
		b.WriteString("; includeSubDomains")
	}
	if h.Preload {
		b.WriteString("; preload")
	}
	return b.String()
}
