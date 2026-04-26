package nexus

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/trace"
)

// LocalInvoker is the in-process variant of a generated cross-module
// client: instead of speaking HTTP to a peer service, it synthesizes
// an httptest.Recorder request and routes it directly through the
// same Gin engine that serves the real endpoints.
//
// This costs ~10-20µs per call (request building + middleware chain)
// vs ~5ns for a direct function call, but buys behavior parity:
// auth, rate limits, metrics, and trace events all run identically
// to a real HTTP request. Monolith and split deployments produce
// the same dashboard signals — you can develop and debug the same
// way.
type LocalInvoker struct {
	engine *gin.Engine
	auth   AuthPropagator
}

// NewLocalInvoker grabs the engine from the App. Generated client
// constructors call this when the target module's deployment matches
// the running binary's deployment (or both are blank — monolith mode).
func NewLocalInvoker(app *App, opts ...LocalInvokerOption) *LocalInvoker {
	li := &LocalInvoker{
		engine: app.Engine(),
		auth:   DefaultAuthPropagator(),
	}
	for _, opt := range opts {
		opt(li)
	}
	return li
}

// LocalInvokerOption tunes a LocalInvoker. Currently only auth
// propagation; kept as a functional option to leave room for
// future knobs (e.g. skip-middleware fast path) without changing
// the constructor signature.
type LocalInvokerOption func(*LocalInvoker)

// WithLocalAuthPropagator swaps the default auth propagator. Same
// reasoning as the remote variant: most apps forward the inbound
// Authorization header; service-token minters override.
func WithLocalAuthPropagator(p AuthPropagator) LocalInvokerOption {
	return func(li *LocalInvoker) { li.auth = p }
}

// Invoke runs args through the Gin engine as if it were a real HTTP
// request to (method, path), then decodes the response into out.
//
// The body/query/path encoding rules match RemoteCaller exactly so a
// handler whose generated client targets either path produces the
// same wire shape — the framework guarantees parity.
//
// Non-2xx responses become *RemoteError, same shape as the remote
// path; callers can type-assert and react identically.
func (li *LocalInvoker) Invoke(ctx context.Context, method, path string, args, out any) (rerr error) {
	// Open a child span so the in-process cross-module call shows up
	// as its own bar on the dashboard waterfall — same shape as a
	// remote HTTP call. Status + error land on the span via attrs +
	// span.End(err) so the bar colors red on failure.
	ctx, span := trace.StartSpan(ctx, "local "+method+" "+path,
		trace.Str("transport", "local"),
		trace.Str("method", method),
		trace.Str("path", path),
	)
	defer func() {
		attachUserErrorAttrs(span, rerr)
		span.End(rerr)
	}()

	finalPath, body, contentType, err := encodeRequest(method, path, args)
	if err != nil {
		return err
	}

	req := httptest.NewRequest(strings.ToUpper(method), finalPath, body).WithContext(ctx)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if li.auth != nil {
		if err := li.auth.Inject(ctx, req); err != nil {
			return fmt.Errorf("nexus: auth propagator: %w", err)
		}
	}

	rec := httptest.NewRecorder()
	li.engine.ServeHTTP(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()

	span.Set("status", resp.StatusCode)
	respBody, _ := io.ReadAll(resp.Body)
	return decodeResponse(resp.StatusCode, respBody, method, path, "local://"+finalPath, out)
}

// silence the http import unused-warning on builds that don't reach
// the err-return path. http is needed transitively because the gin
// engine uses it; this keeps the import from being flagged when a
// future refactor narrows the surface.
var _ = http.StatusOK