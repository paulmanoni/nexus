package nexus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/gin-gonic/gin"
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
func (li *LocalInvoker) Invoke(ctx context.Context, method, path string, args, out any) error {
	expanded, err := expandPath(path, args)
	if err != nil {
		return err
	}

	url := expanded
	var body io.Reader
	contentType := ""
	if methodHasBody(method) {
		b, err := encodeJSONBody(args)
		if err != nil {
			return fmt.Errorf("nexus: encode body: %w", err)
		}
		if b != nil {
			body = bytes.NewReader(b)
			contentType = "application/json"
		}
	} else {
		qs, err := encodeQuery(args)
		if err != nil {
			return fmt.Errorf("nexus: encode query: %w", err)
		}
		if qs != "" {
			url += "?" + qs
		}
	}

	req := httptest.NewRequest(strings.ToUpper(method), url, body).WithContext(ctx)
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

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		re := &RemoteError{
			Status:     resp.StatusCode,
			RawBody:    respBody,
			Method:     method,
			TargetPath: path,
			TargetURL:  "local://" + expanded,
		}
		var env struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(respBody, &env) == nil {
			if env.Error != "" {
				re.Message = env.Error
			} else if env.Message != "" {
				re.Message = env.Message
			}
		}
		return re
	}

	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("nexus: decode local response: %w", err)
	}
	return nil
}

// silence the http import unused-warning on builds that don't reach
// the err-return path. http is needed transitively because the gin
// engine uses it; this keeps the import from being flagged when a
// future refactor narrows the surface.
var _ = http.StatusOK