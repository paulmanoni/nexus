package cors

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// ginHandler builds the per-request CORS middleware. Two paths run
// inside:
//
//   - Preflight (OPTIONS + Access-Control-Request-Method): write the
//     full preflight response, return 204, do NOT call c.Next().
//   - Actual request: write Access-Control-Allow-Origin (and friends
//     for credentials/expose-headers), then call c.Next() so the
//     route handler runs normally.
//
// Same-origin / no-Origin requests skip every CORS header — the
// browser only enforces CORS when the page making the request is
// cross-origin, and a same-origin request shouldn't be touched.
//
// We always set "Vary: Origin" on the response when we're going to
// reflect a specific origin. Without it, an HTTP cache that saw an
// allowed origin's response could serve it back to a blocked
// origin's request — silent leak. The fetch spec demands this.
func ginHandler(cfg *Config, m matcher) gin.HandlerFunc {
	// Pre-build the static header values that don't depend on the
	// request. Saves an allocation per request for the common case.
	allowMethods := strings.Join(cfg.AllowMethods, ", ")
	maxAge := strconv.Itoa(cfg.MaxAge)
	exposeHeaders := strings.Join(cfg.ExposeHeaders, ", ")
	// AllowHeaders may be "*" — handled at request time because
	// "*" gets echoed differently in preflight than a fixed list.
	allowHeadersStatic := strings.Join(cfg.AllowHeaders, ", ")
	wildHeaders := len(cfg.AllowHeaders) == 1 && cfg.AllowHeaders[0] == "*"

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			// Not cross-origin — pass through with no CORS headers.
			c.Next()
			return
		}

		allow, echo := m.match(origin)
		if !allow {
			// Origin not allowed: write NO CORS headers, let the
			// request continue. The browser will fail the response
			// at the CORS check — server-side we don't 403 because
			// non-browser clients (curl, server-to-server) ignore
			// CORS entirely and shouldn't be blocked. Preflight
			// gets a 204 with no allow header so the browser knows
			// to fail without spending bandwidth on the body.
			if isPreflight(c) {
				c.AbortWithStatus(http.StatusNoContent)
				return
			}
			c.Next()
			return
		}

		// Vary: Origin — set whenever the response depends on the
		// request's Origin. Even for the wildcard echo, downstream
		// proxies should not assume the response is identical for
		// every origin. Append rather than overwrite in case a
		// route handler also set Vary on a different header.
		c.Writer.Header().Add("Vary", "Origin")
		c.Header("Access-Control-Allow-Origin", echo)
		if cfg.AllowCredentials {
			c.Header("Access-Control-Allow-Credentials", "true")
		}
		if exposeHeaders != "" {
			c.Header("Access-Control-Expose-Headers", exposeHeaders)
		}

		if isPreflight(c) {
			// Preflight response: tell the browser what's permitted
			// and short-circuit so the route handler doesn't run.
			// Browsers send OPTIONS with Access-Control-Request-*
			// headers; we don't pass these through to the handler.
			c.Header("Access-Control-Allow-Methods", allowMethods)
			c.Header("Access-Control-Max-Age", maxAge)

			// Allowed headers: when configured as ["*"], echo what
			// the browser asked for so the preflight succeeds for
			// any header (including Authorization on first-party
			// apps that haven't enumerated their full header set).
			// Otherwise advertise the configured list.
			if wildHeaders {
				if req := c.GetHeader("Access-Control-Request-Headers"); req != "" {
					c.Header("Access-Control-Allow-Headers", req)
					c.Writer.Header().Add("Vary", "Access-Control-Request-Headers")
				}
			} else if allowHeadersStatic != "" {
				c.Header("Access-Control-Allow-Headers", allowHeadersStatic)
			}
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// isPreflight reports whether this request is a CORS preflight. The
// spec's identifying combination: OPTIONS method + a non-empty
// Access-Control-Request-Method header. Other OPTIONS uses (clients
// querying allowed methods on their own server) don't carry that
// header and shouldn't be hijacked.
func isPreflight(c *gin.Context) bool {
	if c.Request.Method != http.MethodOptions {
		return false
	}
	return c.GetHeader("Access-Control-Request-Method") != ""
}
