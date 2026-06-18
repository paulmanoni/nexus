package ratelimit

import (
	"net/http"
	"strconv"
	"time"

	"github.com/paulmanoni/nexus/httpx"
)

var _ = time.Second // keep the import even if Enforce ever moves

// GinMiddleware returns a httpx.HandlerFunc that enforces the named bucket
// against store. Use it for global (key=GlobalKey) or per-route limits.
// Denial aborts with 429 + Retry-After header + JSON error body — same
// shape a client library would expect regardless of transport.
//
// scopeFn returns the per-request bucket scope (IP for PerIP limits, ""
// otherwise). Defaults to c.ClientIP when nil — good for most apps.
func GinMiddleware(store Store, key string, scopeFn func(*httpx.Ctx) string) httpx.HandlerFunc {
	if scopeFn == nil {
		scopeFn = func(c *httpx.Ctx) string { return c.ClientIP() }
	}
	return func(c *httpx.Ctx) {
		ok, retry := store.Allow(c.Request.Context(), key, scopeFn(c))
		if ok {
			c.Next()
			return
		}
		secs := int(retry.Round(time.Second) / time.Second)
		if secs < 1 {
			secs = 1
		}
		c.Header("Retry-After", strconv.Itoa(secs))
		c.AbortWithStatusJSON(http.StatusTooManyRequests, httpx.H{
			"error":      "rate limit exceeded",
			"retryAfter": retry.String(),
			"key":        key,
		})
	}
}
