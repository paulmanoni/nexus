package metrics

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"

	"braces.dev/errtrace"
	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/graph"
	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/ratelimit"
	"github.com/paulmanoni/nexus/trace"
)

// NewMiddleware returns a transport-agnostic middleware bundle that
// counts requests + errors for key. Same bundle serves REST (via Gin)
// and GraphQL (via Graph); nexus auto-attaches it to every reflective
// registration so the dashboard populates without extra wiring.
//
// For custom transports (raw gin routes not registered via AsRest, etc.)
// you can call NewMiddleware directly and thread the Gin realization
// through c.Next.
func NewMiddleware(store Store, key string) middleware.Middleware {
	return middleware.Middleware{
		Name:        "metrics",
		Description: "Request + error counts per endpoint",
		Kind:        middleware.KindBuiltin,
		Gin:         ginRecorder(store, key),
		Graph:       graphRecorder(store, key),
	}
}

// ginRecorder runs the next handler and records the outcome. Any 4xx /
// 5xx status counts as a failure for dashboard animation purposes
// (red pulse + error count increment); 4xx failures also carry their
// status through to the request.op event so operators can tell
// "client-error rejection" from "server-error meltdown" in the
// Traces tab. Gin's c.ClientIP() honors trusted-proxy headers so
// it's the right source for the IP surfaced in the error dialog.
//
// Deferred + recover pattern: the recording must run whether c.Next()
// returns normally OR a panic propagates up through it. Without the
// defer, a panicking handler skipped recording entirely — its error
// only surfaced in the gin.Recovery() default 500 page, never on the
// dashboard. With the defer we capture the panic value + stack here,
// record it as the request's error, and re-panic so the outer
// recoveryMiddleware can still finalize the 500 response.
func ginRecorder(store Store, key string) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			ip := c.ClientIP()
			if r := recover(); r != nil {
				// Build a stack-aware error here in the panic frame so
				// debug.Stack() points at the failing handler. Recover
				// + re-panic with the wrapped value so the outer
				// recoveryMiddleware sees a *trace.StackError directly
				// (it'll skip its own debug.Stack() round-trip).
				//
				// "panic: <value>" prefix makes the error message
				// clearly distinguishable from a plain returned error
				// in the dashboard — operators see "panic: runtime
				// error: invalid memory address..." instead of just
				// the runtime text.
				msg := fmt.Sprintf("%v", r)
				if msg == "" {
					msg = "panic"
				}
				rerr := &trace.StackError{
					Err:   fmt.Errorf("panic: %s", msg),
					Stack: trace.CleanStack(string(debug.Stack())),
				}
				store.Record(key, ip, rerr)
				publishOpEventWithStatus(c.Request.Context(), key, "rest", ip, 500, rerr)
				panic(rerr)
			}
			status := c.Writer.Status()
			var recErr error
			if status >= 400 {
				if len(c.Errors) > 0 {
					recErr = c.Errors.Last().Err
				} else {
					recErr = statusError{code: status}
				}
			}
			// Surface errtrace frames as a *trace.StackError so the
			// dashboard's stack panel populates for plain returned
			// errors — not just panics. AsRest's buildGinHandler
			// wraps with errtrace.Wrap before c.Error, so any err
			// reaching here from a framework-mounted handler carries
			// at least the boundary frame; user code that chained
			// errtrace.Wrap at intermediate returns adds frames above.
			if recErr != nil {
				if traced := wrapErrtrace(recErr); traced != nil {
					recErr = traced
				}
			}
			store.Record(key, ip, recErr)
			publishOpEventWithStatus(c.Request.Context(), key, "rest", ip, status, recErr)
		}()
		c.Next()
	}
}

// graphRecorder wraps a go-graph resolver with the same record-on-exit
// pattern. IP comes from ratelimit.ClientIPFromCtx — the graphql-go
// adapter stashes it there pre-resolve.
func graphRecorder(store Store, key string) graph.FieldMiddleware {
	return func(next graph.FieldResolveFn) graph.FieldResolveFn {
		return func(p graph.ResolveParams) (any, error) {
			res, err := next(p)
			ip := clientIPFromGraphCtx(p)
			recErr := err
			if err != nil {
				// Same boundary-wrap as AsRest's buildGinHandler:
				// captures the GraphQL resolver call site so the
				// dashboard's stack panel has at least one frame
				// even when the resolver didn't chain errtrace.Wrap.
				wrapped := errtrace.Wrap(err)
				if traced := wrapErrtrace(wrapped); traced != nil {
					recErr = traced
				} else {
					recErr = wrapped
				}
			}
			store.Record(key, ip, recErr)
			publishOpEvent(p.Context, key, "graphql", ip, recErr)
			return res, err
		}
	}
}

// wrapErrtrace converts an errtrace-bearing err into a *trace.StackError
// whose Stack carries the formatted multi-frame trace. Returns nil when
// FormatString produced nothing more than the bare error message — keeps
// downstream observers from synthesising a one-line "stack" that would
// just repeat the message.
func wrapErrtrace(err error) *trace.StackError {
	if err == nil {
		return nil
	}
	formatted := errtrace.FormatString(err)
	if formatted == "" || formatted == err.Error() {
		return nil
	}
	return &trace.StackError{Err: err, Stack: formatted}
}

// publishOpEvent emits a per-op request.op trace event so dashboard
// consumers (packet animations, per-endpoint trace filters) see the
// specific op name, not the coarse HTTP path. No-op when the context
// carries no bus (tests, non-traced apps).
//
// Kept for the GraphQL recorder's binary "ok / error" case — REST
// goes through publishOpEventWithStatus so the actual HTTP status
// surfaces unchanged.
func publishOpEvent(ctx context.Context, key, transport, ip string, err error) {
	status := 200
	if err != nil {
		status = 500
	}
	publishOpEventWithStatus(ctx, key, transport, ip, status, err)
}

// publishOpEventWithStatus publishes a request.op event carrying the
// exact HTTP status. Lets REST 4xx failures (bad-request, not-found)
// register as failures on the dashboard distinctly from 5xx meltdowns.
func publishOpEventWithStatus(ctx context.Context, key, transport, ip string, status int, err error) {
	bus, ok := trace.BusFromCtx(ctx)
	if !ok || bus == nil {
		return
	}
	service, op := splitKey(key)
	ev := trace.Event{
		Kind:      trace.KindRequestOp,
		Service:   service,
		Endpoint:  op,
		Transport: transport,
		Status:    status,
	}
	if ip != "" {
		ev.Meta = map[string]any{"ip": ip}
	}
	if err != nil {
		ev.Error = err.Error()
		ev.Stack = trace.StackOf(err)
	}
	bus.Publish(ev)
}

// splitKey unpacks a "<service>.<op>" metrics key. Service may be
// empty (auto-routed ops pre-mount); we still publish so the UI can
// make best-effort decisions.
func splitKey(key string) (service, op string) {
	if i := strings.Index(key, "."); i >= 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}

// clientIPFromGraphCtx extracts the caller IP from the resolve context
// if a transport layer stashed one. Delegates to ratelimit's helper so
// a single ctx-value key is shared across middleware that needs the IP.
func clientIPFromGraphCtx(p graph.ResolveParams) string {
	return ratelimit.ClientIPFromCtx(p.Context)
}

// statusError is used when gin didn't attach a specific error but the
// status still indicates a server failure. Keeps LastError meaningful on
// the dashboard even when handlers return status codes without Error().
type statusError struct{ code int }

func (e statusError) Error() string {
	return httpStatusText(e.code)
}

// httpStatusText is a tiny subset of net/http.StatusText so metrics
// doesn't drag in net/http just for the message. Extend as needed.
func httpStatusText(code int) string {
	switch code {
	case 500:
		return "internal server error"
	case 501:
		return "not implemented"
	case 502:
		return "bad gateway"
	case 503:
		return "service unavailable"
	case 504:
		return "gateway timeout"
	}
	return "server error"
}
