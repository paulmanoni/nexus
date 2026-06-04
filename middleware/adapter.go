package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/graph"
)

// FromHandler turns one unified Handler into a transport bundle, generating
// exactly the realizations the Handler declares it can serve (redesign §3–4).
// A Handler that declares AllTransports gets both a Gin and a Graph
// realization from a SINGLE implementation — this is what lets the built-ins
// (auth, ratelimit, …) drop their duplicated Gin/Graph pairs.
//
// The returned bundle's Name/Kind come from the Handler; callers may set
// Description (and override Kind) on the value before attaching:
//
//	mw := middleware.FromHandler(h)
//	mw.Description = "30 rpm, per-IP"
//	mw.Kind = middleware.KindBuiltin
//
// The current execution path is unchanged: REST/WS still run mw.Gin in the
// gin chain, GraphQL still wraps mw.Graph around the resolver. FromHandler is
// the authoring layer; the carriers below bridge each transport into the
// neutral RequestCtx the Handler sees.
func FromHandler(h Handler) Middleware {
	m := Middleware{Name: h.Name(), Kind: KindCustom}
	set := h.Transports()
	if set.Has(TransportREST) || set.Has(TransportWebSocket) {
		m.Gin = ginAdapter(h)
	}
	if set.Has(TransportGraphQL) {
		m.Graph = graphAdapter(h)
	}
	return m
}

// --- gin (REST + WS upgrade) -------------------------------------------------

type ginCarrier struct{ c *gin.Context }

func (g ginCarrier) header(key string) string  { return g.c.GetHeader(key) }
func (g ginCarrier) clientIP() string          { return g.c.ClientIP() }
func (g ginCarrier) path() string              { return g.c.FullPath() }
func (g ginCarrier) setHeader(key, val string) { g.c.Header(key, val) }

func (g ginCarrier) reject(status int, err error) error {
	// A REST-aware extension can claim this reject (run its app hooks); if it
	// fully handles the response we render nothing more. Otherwise default.
	if h, ok := rejectHookFrom(g.c.Request.Context()); ok && h(g.c, status, err) {
		return err
	}
	g.c.AbortWithStatusJSON(status, gin.H{"error": err.Error()})
	return err
}

// ginAdapter runs a Handler inside gin's chain. next bridges to c.Next() so
// downstream gin handlers run; a Handler that calls rc.Reject aborts via the
// carrier (which sets the response), and we skip the fallback 500.
func ginAdapter(h Handler) gin.HandlerFunc {
	return func(c *gin.Context) {
		rc := newRequestCtx(c.Request.Context(), TransportREST, ginCarrier{c: c})
		err := h.Handle(rc, func(r *RequestCtx) error {
			c.Request = c.Request.WithContext(r.Context)
			c.Next()
			return nil
		})
		// A Handler that returned an error WITHOUT rejecting (didn't write a
		// response) gets a generic 500 so the failure isn't swallowed.
		if err != nil && !c.IsAborted() && !c.Writer.Written() {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
	}
}

// --- graphql -----------------------------------------------------------------

type graphCarrier struct{ p *graph.ResolveParams }

// header has no general source on a GraphQL resolve — headers are an HTTP
// concern. Returns empty; auth/identity flows read from Context, not here.
func (g graphCarrier) header(string) string { return "" }
func (g graphCarrier) clientIP() string     { return ClientIPFromCtx(g.p.Context) }
func (g graphCarrier) path() string         { return g.p.Info.FieldName }

// setHeader is a no-op: a GraphQL field resolve has no response headers.
func (g graphCarrier) setHeader(string, string) {}

// reject just surfaces the error — GraphQL has no status code; the error
// bubbles into the response's errors array.
func (g graphCarrier) reject(_ int, err error) error { return err }

// graphAdapter runs a Handler as a field middleware. next invokes the wrapped
// resolver and threads any context the Handler injected via rc.WithContext.
func graphAdapter(h Handler) graph.FieldMiddleware {
	return func(next graph.FieldResolveFn) graph.FieldResolveFn {
		return func(p graph.ResolveParams) (any, error) {
			var result any
			var resErr error
			rc := newRequestCtx(p.Context, TransportGraphQL, graphCarrier{p: &p})
			err := h.Handle(rc, func(r *RequestCtx) error {
				p.Context = r.Context
				result, resErr = next(p)
				return resErr
			})
			if err != nil {
				return nil, err
			}
			return result, resErr
		}
	}
}
