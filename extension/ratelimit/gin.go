package ratelimit

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

var _ = time.Second // keep the import even if Enforce ever moves

// GinMiddleware returns a gin.HandlerFunc that enforces the named bucket
// against store. Use it for global (key=GlobalKey) or per-route limits.
// Denial aborts with 429 + Retry-After header + JSON error body — same
// shape a client library would expect regardless of transport.
//
// scopeFn returns the per-request bucket scope (IP for PerIP limits, ""
// otherwise). Defaults to c.ClientIP when nil — good for most apps.
func GinMiddleware(store Store, key string, scopeFn func(*gin.Context) string) gin.HandlerFunc {
	if scopeFn == nil {
		scopeFn = func(c *gin.Context) string { return c.ClientIP() }
	}
	return func(c *gin.Context) {
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
		c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
			"error":      "rate limit exceeded",
			"retryAfter": retry.String(),
			"key":        key,
		})
	}
}

