package nexus

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// corsMiddleware builds a gin.HandlerFunc from a CORSConfig. The
// implementation is deliberately compact — it covers the cases
// browsers actually exercise (origin allowlist, preflight cache,
// credentials, custom headers) without bringing in the
// gin-contrib/cors dep.
//
// Nil CORSConfig is filtered out by the caller, so this always
// receives a populated struct.
func corsMiddleware(cfg CORSConfig) gin.HandlerFunc {
	allowed := buildOriginMatcher(cfg.AllowOrigins)
	methods := strings.Join(defaultStrings(cfg.AllowMethods, defaultCORSMethods), ", ")
	headers := strings.Join(defaultStrings(cfg.AllowHeaders, defaultCORSHeaders), ", ")
	exposed := strings.Join(cfg.ExposeHeaders, ", ")
	maxAge := cfg.MaxAge
	if maxAge <= 0 {
		maxAge = 12 * time.Hour
	}
	maxAgeSecs := strconv.Itoa(int(maxAge.Seconds()))

	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if origin == "" {
			c.Next()
			return
		}
		matched, allowOrigin := allowed(origin)
		if !matched {
			// Don't set the Access-Control-Allow-Origin header —
			// the browser blocks the response. Don't 403 either:
			// same-origin tooling and curl scripts that send Origin
			// shouldn't break. Let the request through; browsers
			// reject without our help.
			c.Next()
			return
		}
		h := c.Writer.Header()
		h.Set("Access-Control-Allow-Origin", allowOrigin)
		h.Add("Vary", "Origin")
		if cfg.AllowCredentials {
			h.Set("Access-Control-Allow-Credentials", "true")
		}
		if exposed != "" {
			h.Set("Access-Control-Expose-Headers", exposed)
		}

		if c.Request.Method == http.MethodOptions {
			h.Set("Access-Control-Allow-Methods", methods)
			h.Set("Access-Control-Allow-Headers", headers)
			h.Set("Access-Control-Max-Age", maxAgeSecs)
			h.Add("Vary", "Access-Control-Request-Method")
			h.Add("Vary", "Access-Control-Request-Headers")
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// buildOriginMatcher returns a function that, given a request's
// Origin header, reports whether the origin is allowed and what
// value to echo back in Access-Control-Allow-Origin.
//
// "*" with credentials disabled echoes "*"; with credentials
// enabled or for a specific allowlist, the matched origin is
// echoed back (the spec disallows "*" alongside credentials, and
// echoing the origin makes the policy work without per-deployment
// branching).
func buildOriginMatcher(allowed []string) func(origin string) (bool, string) {
	if len(allowed) == 0 || (len(allowed) == 1 && allowed[0] == "*") {
		return func(origin string) (bool, string) { return true, origin }
	}
	set := make(map[string]struct{}, len(allowed))
	wildcard := false
	for _, a := range allowed {
		if a == "*" {
			wildcard = true
			continue
		}
		set[a] = struct{}{}
	}
	return func(origin string) (bool, string) {
		if _, ok := set[origin]; ok {
			return true, origin
		}
		if wildcard {
			return true, origin
		}
		return false, ""
	}
}

func defaultStrings(in, fallback []string) []string {
	if len(in) > 0 {
		return in
	}
	return fallback
}

var (
	defaultCORSMethods = []string{
		http.MethodGet, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions,
	}
	defaultCORSHeaders = []string{
		"Origin", "Content-Type", "Accept", "Authorization", "X-Requested-With",
	}
)
