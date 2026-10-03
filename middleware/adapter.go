package middleware

import (
	"net/http"

	"github.com/paulmanoni/nexus/v2/httpx"

	"github.com/paulmanoni/nexus/v2/gql"
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
// The current execution path is unchanged: REST/WS still run mw.HTTP in the
// gin chain, GraphQL still wraps mw.Graph around the resolver. FromHandler is
// the authoring layer; the carriers below bridge each transport into the
// neutral RequestCtx the Handler sees.
func FromHandler(h Handler) Middleware {
	m := Middleware{Name: h.Name(), Kind: KindCustom}
	set := h.Transports()
	if set.Has(TransportREST) || set.Has(TransportWebSocket) {
		m.HTTP = ginAdapter(h)
	}
	if set.Has(TransportGraphQL) {
		m.Graph = graphAdapter(h)
	}
	return m
}

// --- gin (REST + WS upgrade) -------------------------------------------------

type ginCarrier struct{ c *httpx.Ctx }

func (g ginCarrier) header(key string) string  { return g.c.GetHeader(key) }
func (g ginCarrier) clientIP() string          { return g.c.ClientIP() }
func (g ginCarrier) path() string              { return g.c.FullPath() }
func (g ginCarrier) setHeader(key, val string) { g.c.Header(key, val) }

func (g ginCarrier) reject(status int, err error) error {
	g.c.AbortWithStatusJSON(ErrorBody(status, err))
	return err
}

// ErrorBody renders a rejection on REST/WS: the response status and JSON
// body for err, given the status the middleware asked for. The nexus
// package installs its error model here, so a middleware's rejection reads
// like any other failed request ({"code", "message", "errors"}); the
// default is {"message": …}. A status of 0 asks for the error's own.
var ErrorBody = func(status int, err error) (int, any) {
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return status, httpx.H{"message": err.Error()}
}

// rejectJSON writes a caller-supplied body verbatim (the extension's own
// error envelope) and aborts the gin chain.
func (g ginCarrier) rejectJSON(status int, body any) error {
	g.c.AbortWithStatusJSON(status, body)
	return errRejected
}

// ginAdapter runs a Handler inside gin's chain. next bridges to c.Next() so
// downstream gin handlers run; a Handler that calls rc.Reject aborts via the
// carrier (which sets the response), and we skip the fallback 500.
func ginAdapter(h Handler) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		rc := newRequestCtx(c.Request.Context(), TransportREST, ginCarrier{c: c})
		err := h.Handle(rc, func(r *RequestCtx) error {
			c.Request = c.Request.WithContext(r.Context)
			c.Next()
			return nil
		})
		// A Handler that returned an error WITHOUT rejecting (didn't write a
		// response) gets a generic 500 so the failure isn't swallowed.
		if err != nil && !c.IsAborted() && !c.Writer.Written() {
			c.AbortWithStatusJSON(ErrorBody(0, err))
		}
	}
}

// --- graphql -----------------------------------------------------------------

type graphCarrier struct{ f *gql.Field }

// header has no general source on a GraphQL resolve — headers are an HTTP
// concern. Returns empty; auth/identity flows read from Context, not here.
func (g graphCarrier) header(string) string { return "" }
func (g graphCarrier) clientIP() string     { return ClientIPFromCtx(g.f.Context) }
func (g graphCarrier) path() string         { return g.f.Info.FieldName }

// setHeader is a no-op: a GraphQL field resolve has no response headers.
func (g graphCarrier) setHeader(string, string) {}

// reject surfaces the error with the status it was rejected with — GraphQL
// has no status code, but the error's rendering (extensions.code) uses it.
func (g graphCarrier) reject(status int, err error) error {
	return &Rejection{Status: status, Err: err}
}

// Rejection is the error a middleware's Reject returns on GraphQL: the
// error, and the HTTP status it would have answered with on REST.
type Rejection struct {
	Status int
	Err    error
}

func (r *Rejection) Error() string { return r.Err.Error() }
func (r *Rejection) Unwrap() error { return r.Err }

// rejectJSON cannot render a body on GraphQL (a field resolve has no
// response envelope), so it only short-circuits. A GraphQL ErrorHandler
// should return a wrapped error instead of calling RejectJSON.
func (g graphCarrier) rejectJSON(_ int, _ any) error { return errRejected }

// graphAdapter runs a Handler as a field middleware. next invokes the wrapped
// resolver and threads any context the Handler injected via rc.WithContext.
func graphAdapter(h Handler) gql.Middleware {
	return func(next gql.Resolver) gql.Resolver {
		return func(f gql.Field) (any, error) {
			var result any
			var resErr error
			rc := newRequestCtx(f.Context, TransportGraphQL, graphCarrier{f: &f})
			err := h.Handle(rc, func(r *RequestCtx) error {
				f.Context = r.Context
				result, resErr = next(f)
				return resErr
			})
			if err != nil {
				return nil, err
			}
			return result, resErr
		}
	}
}
