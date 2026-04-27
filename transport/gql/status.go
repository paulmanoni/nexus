package gql

import (
	"context"
	"net/http"
	"sync/atomic"
)

// statusKey is the unexported context key used by the status holder.
// Unique struct type guarantees no collision with other context keys.
type statusKey struct{}

// statusHolder carries a mutable HTTP status code through the request
// context. The framework installs one per GraphQL request before
// graphql.Do runs; field middlewares (or resolvers) call
// SetStatusCode to override the default 200 OK based on whatever
// authorization / validation / rate-limit decision they made.
//
// atomic.Int32 lets concurrent middlewares race-safely. In practice
// there's no concurrency in a single request's resolver chain, but
// atomic costs nothing and keeps future-resolver-fanout safe.
type statusHolder struct {
	code atomic.Int32
}

// withStatusHolder attaches a fresh holder to ctx. Called once per
// GraphQL request from the gin middleware in adapter.go.
func withStatusHolder(ctx context.Context) (context.Context, *statusHolder) {
	h := &statusHolder{}
	return context.WithValue(ctx, statusKey{}, h), h
}

// SetStatusCode overrides the HTTP status code for the current
// GraphQL request. Call from a graph.FieldMiddleware or resolver
// to translate a decision (authorization failure, rate-limit hit,
// validation rejection, etc.) into a non-200 response code:
//
//	authMw := graph.FieldMiddleware(func(p graphql.ResolveParams, next graphql.FieldResolveFn) (any, error) {
//	    if !authed(p.Context) {
//	        gql.SetStatusCode(p.Context, http.StatusUnauthorized)
//	        return nil, errors.New("unauthorized")
//	    }
//	    return next(p)
//	})
//
// Without this call, the framework returns 200 OK with errors in
// the GraphQL response body — the GraphQL-spec default.
//
// When SetStatusCode is called multiple times within one request,
// the LAST value wins. That matches the natural "innermost
// middleware decides" expectation: outer-layer auth runs first
// (sets 401 if it rejects), inner rate-limit runs after (overrides
// to 429 if it rejects). When neither rejects, no override fires.
//
// No-op when ctx didn't pass through the framework's GraphQL
// adapter — useful for resolver code that runs in tests with a
// bare graphql.Do call.
func SetStatusCode(ctx context.Context, code int) {
	if h, ok := ctx.Value(statusKey{}).(*statusHolder); ok {
		h.code.Store(int32(code))
	}
}

// statusFromCtx returns the override stored on ctx, or 0 when none
// was set. The gin middleware reads this after graphql.Do finishes
// to decide whether to override the default 200.
func statusFromCtx(ctx context.Context) int {
	if h, ok := ctx.Value(statusKey{}).(*statusHolder); ok {
		return int(h.code.Load())
	}
	return 0
}

// statusCaptureWriter wraps gin's ResponseWriter so the inner
// handler's WriteHeader call doesn't reach the underlying socket
// until the framework has had a chance to apply the override.
//
// Buffering + replay is the right shape here because graph.NewHTTP
// (the goGraphHandler path) writes the response directly via the
// raw http.ResponseWriter — there's no hook between "compute
// result" and "send headers" the way there is for the simple
// handler. We capture the whole response, then re-emit it with the
// override status if one was set.
type statusCaptureWriter struct {
	http.ResponseWriter
	status int
	header http.Header
	body   []byte
}

func newStatusCaptureWriter(w http.ResponseWriter) *statusCaptureWriter {
	return &statusCaptureWriter{ResponseWriter: w, status: http.StatusOK, header: http.Header{}}
}

// Header returns the buffered header map so the inner handler can
// set Content-Type, Vary, etc. before WriteHeader. We replay these
// onto the underlying writer at flush time.
func (w *statusCaptureWriter) Header() http.Header { return w.header }

func (w *statusCaptureWriter) WriteHeader(code int) { w.status = code }

func (w *statusCaptureWriter) Write(b []byte) (int, error) {
	w.body = append(w.body, b...)
	return len(b), nil
}

// flush replays the buffered response onto the underlying writer.
// When override > 0, that status replaces whatever the inner
// handler wrote.
func (w *statusCaptureWriter) flush(override int) {
	dst := w.ResponseWriter.Header()
	for k, vs := range w.header {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	status := w.status
	if override > 0 {
		status = override
	}
	w.ResponseWriter.WriteHeader(status)
	if len(w.body) > 0 {
		_, _ = w.ResponseWriter.Write(w.body)
	}
}
