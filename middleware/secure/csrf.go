package secure

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"

	"github.com/paulmanoni/nexus/httpx"
)

// safeMethods are the HTTP methods RFC 7231 defines as safe. They can't
// change state, so they're never CSRF-checked — instead they're where
// the token cookie is minted.
var safeMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodOptions: true,
	http.MethodTrace:   true,
}

// CSRFHandler builds the double-submit-cookie CSRF middleware. Call
// ApplyCSRFDefaults (and ideally ValidateCSRF) on the config first.
//
//   - Safe method: ensure the token cookie exists (minting one on first
//     contact) so the SPA has a value to echo, then continue.
//   - Unsafe method: unless Skip says otherwise, require the request to
//     carry the same token in both the cookie and the header (or form
//     field). A cross-site attacker can drive the victim's browser to
//     send the cookie but can't read it (same-origin policy) to forge
//     the matching header, so a match proves same-origin intent.
func CSRFHandler(cfg *CSRFConfig) httpx.HandlerFunc {
	skip := cfg.Skip
	if skip == nil {
		skip = DefaultSkip
	}
	return func(c *httpx.Ctx) {
		if safeMethods[c.Request.Method] {
			ensureToken(c, cfg)
			c.Next()
			return
		}
		if skip(c) {
			c.Next()
			return
		}
		if cfg.CheckOrigin && !originOK(c, cfg) {
			reject(c, "origin does not match")
			return
		}

		cookieTok, _ := c.Cookie(cfg.CookieName)
		sent := c.GetHeader(cfg.HeaderName)
		if sent == "" {
			// Fall back to a form field for classic HTML form posts.
			// FormValue only reads the body for form content types, so
			// JSON bodies (which carry the header) are untouched.
			sent = c.Request.FormValue(cfg.FieldName)
		}
		if cookieTok == "" || sent == "" ||
			subtle.ConstantTimeCompare([]byte(cookieTok), []byte(sent)) != 1 {
			reject(c, "CSRF token missing or invalid")
			return
		}
		c.Next()
	}
}

// DefaultSkip exempts requests that authenticate with an Authorization
// header. Bearer/API-key auth is not CSRF-vulnerable: a browser never
// attaches such a header automatically on a cross-site request, so the
// forgery vector doesn't exist. This lets CSRF be enabled without
// breaking stateless API clients (curl, mobile, server-to-server).
func DefaultSkip(c *httpx.Ctx) bool {
	return c.GetHeader("Authorization") != ""
}

// ensureToken mints and sets a fresh token cookie when the request
// doesn't already carry one, so a first GET seeds the SPA.
func ensureToken(c *httpx.Ctx, cfg *CSRFConfig) {
	if existing, err := c.Cookie(cfg.CookieName); err == nil && existing != "" {
		return
	}
	setTokenCookie(c, cfg, GenerateToken(cfg.TokenBytes))
}

func setTokenCookie(c *httpx.Ctx, cfg *CSRFConfig, token string) {
	secure := requestIsHTTPS(c)
	if cfg.CookieSecure != nil {
		secure = *cfg.CookieSecure
	}
	c.SetSameSite(cfg.SameSite)
	// HttpOnly is deliberately false: the SPA's JavaScript must read the
	// cookie to echo it in the header. That readability is safe — the
	// token is per-session and only proves same-origin, not a credential.
	c.SetCookie(cfg.CookieName, token, cfg.MaxAge, cfg.CookiePath, cfg.CookieDomain, secure, false)
}

// GenerateToken returns a base64url-encoded cryptographically random
// token. A rand.Read failure is unrecoverable for a security token, so
// it panics rather than emit a weak value.
func GenerateToken(n int) string {
	if n <= 0 {
		n = DefaultTokenBytes
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("security: crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// requestIsHTTPS reports whether the request reached the server over TLS,
// honoring a reverse proxy's X-Forwarded-Proto.
func requestIsHTTPS(c *httpx.Ctx) bool {
	if c.Request.TLS != nil {
		return true
	}
	return strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
}

// originOK is the optional Origin/Referer same-site check. It only
// rejects when an Origin (or Referer) is present AND its host differs
// from the request host and every trusted origin — absent headers fall
// through to the token check so non-browser clients aren't blocked here.
func originOK(c *httpx.Ctx, cfg *CSRFConfig) bool {
	src := c.GetHeader("Origin")
	if src == "" {
		src = c.GetHeader("Referer")
	}
	if src == "" {
		return true
	}
	host := hostOf(src)
	if host == "" {
		return true
	}
	if strings.EqualFold(host, c.Request.Host) {
		return true
	}
	for _, t := range cfg.TrustedOrigins {
		if strings.EqualFold(host, t) || strings.EqualFold(host, hostOf(t)) {
			return true
		}
	}
	return false
}

// hostOf extracts host[:port] from an origin/referer/url string.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

func reject(c *httpx.Ctx, reason string) {
	c.AbortWithStatusJSON(http.StatusForbidden, httpx.H{
		"error":  "forbidden",
		"reason": reason,
	})
}
