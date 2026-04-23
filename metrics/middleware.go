package metrics

import (
	"context"
	"strings"

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

// ginRecorder runs the next handler and records the outcome. Status >= 500
// counts as a server error; everything else as a success. Gin's
// c.ClientIP() honors trusted-proxy headers so it's the right source
// for the IP we surface in the error dialog.
func ginRecorder(store Store, key string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		ip := c.ClientIP()
		var recErr error
		if status := c.Writer.Status(); status >= 500 {
			if len(c.Errors) > 0 {
				recErr = c.Errors.Last().Err
			} else {
				recErr = statusError{code: status}
			}
		}
		store.Record(key, ip, recErr)
		publishOpEvent(c.Request.Context(), key, "rest", ip, recErr)
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
			store.Record(key, ip, err)
			publishOpEvent(p.Context, key, "graphql", ip, err)
			return res, err
		}
	}
}

// publishOpEvent emits a per-op request.op trace event so dashboard
// consumers (packet animations, per-endpoint trace filters) see the
// specific op name, not the coarse HTTP path. No-op when the context
// carries no bus (tests, non-traced apps).
func publishOpEvent(ctx context.Context, key, transport, ip string, err error) {
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
	}
	if ip != "" {
		ev.Meta = map[string]any{"ip": ip}
	}
	if err != nil {
		ev.Error = err.Error()
		ev.Status = 500
	} else {
		ev.Status = 200
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
