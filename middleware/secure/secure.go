// Package secure holds the transport-neutral implementations of the two
// web-security defenses nexus applies — security response headers and
// CSRF — with no dependency beyond httpx and the standard library.
//
// It exists so BOTH the framework core (Config.Middleware.Security, read
// in package nexus) and the extension/security plugin can share one copy
// of the logic. The core can't import extension/security (that package
// imports nexus, which would cycle), so the reusable pieces live down
// here where everything can reach them.
//
// Most apps never import this package directly: they either set
// [runtime.middleware.security] in nexus.toml (core path) or load
// extension/security for the dashboard tab + per-route bundles. The
// exported surface here is what those two callers wrap.
package secure

import (
	"fmt"
	"net/http"

	"github.com/paulmanoni/nexus/httpx"
)

// Sentinel + default values shared by the header and CSRF logic.
const (
	// Omit marks a header the operator explicitly turned off. An empty
	// string means "apply the default"; Omit ("-") means "send nothing".
	Omit = "-"

	DefaultFrameOptions   = "DENY"
	DefaultReferrerPolicy = "strict-origin-when-cross-origin"
	DefaultHSTSMaxAge     = 31536000 // one year, in seconds

	DefaultCSRFCookie = "csrftoken"   // matches client.DefaultCSRFCookie
	DefaultCSRFHeader = "X-CSRFToken" // matches client.DefaultCSRFHeader
	DefaultCSRFField  = "csrf_token"  // form-body fallback field
	DefaultTokenBytes = 32            // 256-bit token
	DefaultCSRFMaxAge = 12 * 60 * 60  // 12h cookie lifetime
)

// HeadersConfig controls the security response headers. Empty string
// fields take the recommended default; set a field to Omit to drop that
// header. HSTS, CSP, Permissions-Policy, and COOP are opt-in (nil/empty
// → not sent) because a wrong value there breaks a site.
type HeadersConfig struct {
	// FrameOptions sets X-Frame-Options (clickjacking defense).
	// Default "DENY"; "SAMEORIGIN" to allow same-origin framing; "-"
	// (Omit) to send nothing (e.g. when you use CSP frame-ancestors).
	FrameOptions string

	// ContentTypeNosniff sets X-Content-Type-Options: nosniff. nil →
	// true (recommended). Point at a false to disable.
	ContentTypeNosniff *bool

	// ReferrerPolicy sets Referrer-Policy. Default
	// "strict-origin-when-cross-origin"; Omit to send nothing.
	ReferrerPolicy string

	// HSTS, when non-nil, sends Strict-Transport-Security. Opt-in
	// because a wrong max-age/preload can lock users out of a domain.
	// Browsers ignore it over plain http, so it's safe once on https.
	HSTS *HSTSConfig

	// ContentSecurityPolicy sets Content-Security-Policy verbatim.
	// Empty → not sent (CSP is too app-specific to default).
	ContentSecurityPolicy string

	// PermissionsPolicy sets Permissions-Policy verbatim. Empty → not
	// sent.
	PermissionsPolicy string

	// CrossOriginOpenerPolicy sets Cross-Origin-Opener-Policy (e.g.
	// "same-origin"). Empty → not sent.
	CrossOriginOpenerPolicy string
}

// HSTSConfig is the parsed Strict-Transport-Security directive.
type HSTSConfig struct {
	MaxAge            int  // seconds; 0 → DefaultHSTSMaxAge
	IncludeSubdomains bool // adds "; includeSubDomains"
	Preload           bool // adds "; preload"
}

// CSRFConfig controls the double-submit-cookie check.
type CSRFConfig struct {
	CookieName   string // default "csrftoken"
	HeaderName   string // default "X-CSRFToken"
	FieldName    string // form-body fallback; default "csrf_token"
	CookiePath   string // default "/"
	CookieDomain string // default "" (host-only)

	// CookieSecure controls the cookie's Secure flag. nil → auto
	// (Secure when the request arrived over https / X-Forwarded-Proto
	// https), so dev over http works without a config change.
	CookieSecure *bool

	// SameSite for the cookie. Zero value → Lax.
	SameSite http.SameSite

	MaxAge     int // cookie lifetime in seconds; default 12h
	TokenBytes int // random token size; default 32 (256-bit)

	// CheckOrigin adds a defense-in-depth Origin/Referer same-site
	// check on top of the token. Off by default: it can misfire behind
	// proxies that rewrite Host, and the token alone is sufficient.
	CheckOrigin    bool
	TrustedOrigins []string // extra hosts accepted by the Origin check

	// Skip decides per request whether to bypass the check on an unsafe
	// method. nil → DefaultSkip (skip requests with an Authorization
	// header — bearer/token auth is not CSRF-vulnerable).
	Skip func(c *httpx.Ctx) bool
}

// ApplyHeaderDefaults fills the recommended values so a zero
// HeadersConfig produces the three safe headers.
func ApplyHeaderDefaults(h *HeadersConfig) {
	if h.FrameOptions == "" {
		h.FrameOptions = DefaultFrameOptions
	}
	if h.ReferrerPolicy == "" {
		h.ReferrerPolicy = DefaultReferrerPolicy
	}
}

// ApplyCSRFDefaults fills the conventional CSRF values.
func ApplyCSRFDefaults(c *CSRFConfig) {
	if c.CookieName == "" {
		c.CookieName = DefaultCSRFCookie
	}
	if c.HeaderName == "" {
		c.HeaderName = DefaultCSRFHeader
	}
	if c.FieldName == "" {
		c.FieldName = DefaultCSRFField
	}
	if c.CookiePath == "" {
		c.CookiePath = "/"
	}
	if c.SameSite == http.SameSiteDefaultMode {
		c.SameSite = http.SameSiteLaxMode
	}
	if c.MaxAge == 0 {
		c.MaxAge = DefaultCSRFMaxAge
	}
	if c.TokenBytes == 0 {
		c.TokenBytes = DefaultTokenBytes
	}
}

// ValidateCSRF rejects a token size too small to be safe.
func ValidateCSRF(c *CSRFConfig) error {
	if c.TokenBytes < 16 {
		return fmt.Errorf("security: CSRF TokenBytes=%d is too small (min 16 for a 128-bit token)", c.TokenBytes)
	}
	return nil
}
