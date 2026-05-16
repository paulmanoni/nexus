package nexus

import (
	"fmt"
	"log"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/trace"
)

// recoveryMiddleware catches handler panics, captures the runtime
// stack, and threads both the panic value and the stack through gin's
// c.Error path as a *trace.StackError. The metrics middleware + trace
// publishers extract the stack via trace.StackOf and surface it on
// the dashboard, so an operator clicking the red error badge can see
// where the panic originated without grep-ing the server logs.
//
// Functionally equivalent to gin.Recovery() for the response surface
// (HTTP 500 + abort) — the value-add is the captured-stack pipeline
// for the framework's own observability.
func recoveryMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			// metrics.ginRecorder catches panics in its inner defer,
			// builds a *trace.StackError, records the failure, and
			// re-panics with the wrapped value so we land here with
			// the stack already captured. Reuse it; otherwise (raw
			// gin route without the metrics middleware) capture our
			// own debug.Stack() in this frame.
			var err error
			if se, ok := r.(*trace.StackError); ok {
				err = se
			} else {
				msg := fmt.Sprintf("%v", r)
				if msg == "" {
					msg = "panic"
				}
				err = &trace.StackError{
					Err:   fmt.Errorf("panic: %s", msg),
					Stack: trace.CleanStack(string(debug.Stack())),
				}
			}
			// Mirror to stderr so dev / nexus dev terminals also see
			// the panic. Without this, recovery is silent server-side
			// and an operator might miss it if the dashboard isn't
			// open. Path is the request path; method gives quick
			// triage context.
			log.Printf("[nexus] panic recovered: %s %s — %s\n%s",
				c.Request.Method, c.Request.URL.Path, err.Error(), trace.StackOf(err))
			// Attach to gin.Context so any other observer downstream
			// (rate-limit fallback, custom middleware) can read the
			// error after we abort.
			_ = c.Error(err)
			// Default response: 500 + abort, matching gin.Recovery()'s
			// behaviour. Don't overwrite a status the handler may have
			// already written.
			if !c.Writer.Written() {
				c.AbortWithStatus(http.StatusInternalServerError)
			} else {
				c.Abort()
			}
		}()
		c.Next()
	}
}