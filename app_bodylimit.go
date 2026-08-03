package nexus

import (
	"errors"
	"net/http"

	"github.com/paulmanoni/nexus/httpx"
)

// bodyLimitMiddleware caps how many bytes a request body may deliver.
//
// Without it, every JSON-binding handler is a memory-exhaustion primitive: an
// unauthenticated client opens a POST, declares nothing about length, and
// streams until the process dies. http.MaxBytesReader is the stdlib answer —
// it makes the read fail past the limit rather than buffering, so the cost is
// bounded no matter what the handler does with the body.
//
// The wrapper is transparent to handlers: they keep reading r.Body normally,
// and an over-limit read surfaces as a read error. The 413 below only fires
// when nothing downstream wrote a status of its own, so a handler that wants
// to report the overflow its own way still can.
//
// WebSocket upgrades are unaffected — they carry no body, and once the
// connection is hijacked the socket is read directly rather than through
// r.Body.
func bodyLimitMiddleware(limit int64) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		if c.Request != nil && c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		}
		c.Next()
		// MaxBytesReader already sets the status to 413 on the
		// ResponseWriter when it trips, but only if nothing was written
		// first; this covers the case where a handler swallowed the read
		// error and returned without writing anything at all.
		if !c.Writer.Written() && bodyLimitExceeded(c) {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge,
				httpx.H{"error": "request body too large"})
		}
	}
}

// bodyLimitExceeded reports whether any error recorded on the context came
// from the body cap.
func bodyLimitExceeded(c *httpx.Ctx) bool {
	for _, err := range c.Errors() {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return true
		}
	}
	return false
}
