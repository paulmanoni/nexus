// Package nexustest is the in-process testing harness for nexus apps — the
// framework's own net/http/httptest. It boots an App with every REST/GraphQL/WS
// route mounted but no network listener, then drives requests through
// App.ServeHTTP. Tests get a real router, real middleware, real reflective
// handler dispatch, and zero flaky port binding.
//
//	func TestGetUser(t *testing.T) {
//	    app := nexustest.New(t, nexus.Config{}, billing.Module)
//
//	    res := app.GET("/users/42")
//	    res.AssertStatus(200)
//	    var u User
//	    res.JSON(&u)
//
//	    data := app.GraphQL(`{ user(id:"42"){ name } }`, nil)
//	    // data is the decoded "data" object; query errors fail the test.
//	}
//
// The App is stopped automatically via t.Cleanup. For WebSocket or true
// loopback tests use Server(), which wraps the same handler in an httptest.Server.
package nexustest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/nexus"
)

// App is a started, listener-less nexus app under test. It is an http.Handler.
type App struct {
	*nexus.App
	tb   testing.TB
	gqls string // resolved GraphQL mount path
}

// New boots cfg+opts in-process and registers cleanup. It fails the test on any
// build or start error. Introspection is left as cfg sets it; routing works
// regardless.
func New(tb testing.TB, cfg nexus.Config, opts ...nexus.Option) *App {
	tb.Helper()
	app, stop, err := nexus.InProcess(cfg, opts...)
	if err != nil {
		tb.Fatalf("nexustest: boot: %v", err)
	}
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := stop(ctx); err != nil {
			tb.Errorf("nexustest: stop: %v", err)
		}
	})
	gqls := cfg.GraphQL.Path
	if gqls == "" {
		gqls = nexus.DefaultGraphQLPath
	}
	return &App{App: app, tb: tb, gqls: gqls}
}

// Do drives an arbitrary request through the mounted router and returns the
// recorded response. This is the primitive the REST/GraphQL helpers build on.
func (a *App) Do(req *http.Request) *Response {
	a.tb.Helper()
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	return &Response{tb: a.tb, rec: rec, Code: rec.Code}
}

// REST builds a request with an optional JSON body and runs it. body may be nil,
// a []byte / string (sent verbatim), or any value (JSON-encoded).
func (a *App) REST(method, path string, body any) *Response {
	a.tb.Helper()
	req := httptest.NewRequest(method, path, encodeBody(a.tb, body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return a.Do(req)
}

func (a *App) GET(path string) *Response             { return a.REST(http.MethodGet, path, nil) }
func (a *App) DELETE(path string) *Response          { return a.REST(http.MethodDelete, path, nil) }
func (a *App) POST(path string, body any) *Response  { return a.REST(http.MethodPost, path, body) }
func (a *App) PUT(path string, body any) *Response   { return a.REST(http.MethodPut, path, body) }
func (a *App) PATCH(path string, body any) *Response { return a.REST(http.MethodPatch, path, body) }

// GraphQL POSTs a query to the default GraphQL mount, asserts there are no
// top-level GraphQL errors, and returns the decoded "data" object. Use RawGraphQL
// to inspect errors yourself.
func (a *App) GraphQL(query string, vars map[string]any) map[string]any {
	a.tb.Helper()
	out := a.RawGraphQL(query, vars)
	if len(out.Errors) > 0 {
		a.tb.Fatalf("nexustest: graphql errors: %v", out.Errors)
	}
	return out.Data
}

// GraphQLResult is the decoded GraphQL envelope.
type GraphQLResult struct {
	Data   map[string]any   `json:"data"`
	Errors []map[string]any `json:"errors"`
}

// RawGraphQL POSTs a query and returns the decoded envelope without asserting on
// errors.
func (a *App) RawGraphQL(query string, vars map[string]any) GraphQLResult {
	a.tb.Helper()
	payload := map[string]any{"query": query}
	if vars != nil {
		payload["variables"] = vars
	}
	res := a.REST(http.MethodPost, a.gqls, payload)
	var out GraphQLResult
	res.JSON(&out)
	return out
}

// Server wraps the same handler in an httptest.Server for tests that need a real
// URL — WebSocket dials, redirects, or external clients. Closed via t.Cleanup.
func (a *App) Server() *httptest.Server {
	a.tb.Helper()
	srv := httptest.NewServer(a.App)
	a.tb.Cleanup(srv.Close)
	return srv
}

// Response is a recorded HTTP response with assertion + decode helpers.
type Response struct {
	tb   testing.TB
	rec  *httptest.ResponseRecorder
	Code int
}

// Body returns the raw response body.
func (r *Response) Body() []byte { return r.rec.Body.Bytes() }

// String returns the body as a string.
func (r *Response) String() string { return r.rec.Body.String() }

// Header returns the response headers.
func (r *Response) Header() http.Header { return r.rec.Header() }

// JSON decodes the body into v, failing the test on error.
func (r *Response) JSON(v any) *Response {
	r.tb.Helper()
	if err := json.Unmarshal(r.rec.Body.Bytes(), v); err != nil {
		r.tb.Fatalf("nexustest: decode body (status %d): %v\nbody: %s", r.Code, err, r.rec.Body.String())
	}
	return r
}

// AssertStatus fails the test if the status code differs from want.
func (r *Response) AssertStatus(want int) *Response {
	r.tb.Helper()
	if r.Code != want {
		r.tb.Fatalf("nexustest: status = %d, want %d\nbody: %s", r.Code, want, r.rec.Body.String())
	}
	return r
}

// AssertOK is AssertStatus(200).
func (r *Response) AssertOK() *Response { return r.AssertStatus(http.StatusOK) }

func encodeBody(tb testing.TB, body any) io.Reader {
	tb.Helper()
	switch b := body.(type) {
	case nil:
		return nil
	case []byte:
		return bytes.NewReader(b)
	case string:
		return strings.NewReader(b)
	case io.Reader:
		return b
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			tb.Fatalf("nexustest: marshal body: %v", err)
		}
		return bytes.NewReader(raw)
	}
}
