// Package httpx is a prototype of nexus's router-agnostic seam.
//
// Today nexus binds directly to gin: endpoint chains are []gin.HandlerFunc,
// flow control is gin's c.Next()/c.Abort(), and routes are mounted on a
// *gin.Engine. Swapping routers is "hard" mainly because the middleware model
// is gin-idiomatic (see the earlier analysis: the chain rewrite was the HIGH
// item).
//
// This package removes that coupling with one move: chain execution lives in
// the seam, not in the router. A *Ctx owns the handler slice, the index, and
// Next()/Abort() — identical semantics to gin's, but ours. A Router backend
// (gin, chi, net/http) is reduced to two jobs: match a path and hand back
// path params. Everything the framework calls — JSON, Status, Header, Param,
// Query, Error/Errors, Set/Get, IsAborted, Written — is on *Ctx, so framework
// and middleware code never names the router's types.
//
// Canonical path syntax is gin's (":id", "*rest") so existing nexus route
// strings need no rewrite; the chi/std adapters translate on registration.
package httpx

import (
	"context"
	"encoding/json"
	"net/http"
)

// HandlerFunc is the one handler/middleware shape. A middleware calls
// c.Next() to run the rest of the chain; returning without Next() (or calling
// c.Abort()) stops it. This is gin's contract, kept verbatim so existing
// middleware ports mechanically (c. -> the same calls on *Ctx).
type HandlerFunc func(*Ctx)

// Router is the seam. Gin, chi, and stdlib ServeMux each satisfy it with a
// ~40-line adapter. The framework holds a Router, never a *gin.Engine.
type Router interface {
	// Handle mounts a chain at method+path. path uses canonical ":id"/"*rest"
	// syntax regardless of the backend.
	Handle(method, path string, chain ...HandlerFunc)
	// NoRoute sets the fallback handler (SPA index.html, etc.).
	NoRoute(HandlerFunc)
	// Static serves a directory under a URL prefix.
	Static(prefix, dir string)
	// Routes returns mounted "METHOD path" pairs (dashboard introspection).
	Routes() []string
	http.Handler
}

// ResponseWriter wraps the backend writer to track status + whether anything
// was written — the framework relies on c.Writer.Written() to avoid clobbering
// a handler that wrote its own response.
type ResponseWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func (w *ResponseWriter) WriteHeader(code int) {
	if w.written {
		return
	}
	w.status = code
	w.written = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *ResponseWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Status reports the committed status (200 until set).
func (w *ResponseWriter) Status() int { return w.status }

// Written reports whether the response body/header was started.
func (w *ResponseWriter) Written() bool { return w.written }

// Ctx is the transport-neutral request handle. It is the union of what
// transport_rest.go, reflect_handler.go, and the existing middleware carrier
// reach for — but with zero router types in its API.
type Ctx struct {
	W   *ResponseWriter
	R   *http.Request
	ctx context.Context

	route string
	param func(string) string

	handlers []HandlerFunc
	index    int
	aborted  bool

	bag  map[any]any
	errs []error
}

// Serve is the single entry point every adapter calls per request: build a
// Ctx over the backend's writer/request/param-getter and run the chain. The
// router has done its only job (match + params) by the time we get here.
func Serve(chain []HandlerFunc, w http.ResponseWriter, r *http.Request, route string, param func(string) string) {
	c := &Ctx{
		W:        &ResponseWriter{ResponseWriter: w, status: http.StatusOK},
		R:        r,
		ctx:      r.Context(),
		route:    route,
		param:    param,
		handlers: chain,
		index:    -1,
	}
	c.Next()
}

// --- chain flow control (owned here, not by the router) ----------------------

// Next runs the remaining handlers. Mirrors gin's loop exactly so a recovery
// middleware's `defer recover(); c.Next()` still catches downstream panics.
func (c *Ctx) Next() {
	c.index++
	for c.index < len(c.handlers) {
		c.handlers[c.index](c)
		c.index++
	}
}

// Abort stops the chain after the current handler (gin semantics: jump the
// index past the end so every active Next loop terminates).
func (c *Ctx) Abort() {
	c.aborted = true
	c.index = len(c.handlers) + 1
}

// IsAborted reports whether Abort (or an Abort* helper) was called.
func (c *Ctx) IsAborted() bool { return c.aborted }

// --- request reads -----------------------------------------------------------

func (c *Ctx) Method() string        { return c.R.Method }
func (c *Ctx) FullPath() string      { return c.route }
func (c *Ctx) Param(key string) string {
	if c.param == nil {
		return ""
	}
	return c.param(key)
}
func (c *Ctx) Query(key string) string  { return c.R.URL.Query().Get(key) }
func (c *Ctx) Header(key string) string { return c.R.Header.Get(key) }
func (c *Ctx) Request() *http.Request   { return c.R }

// ClientIP is intentionally simple here; the real seam reuses nexus's existing
// middleware.ClientIP logic (trusted proxies), which is already router-neutral.
func (c *Ctx) ClientIP() string { return c.R.RemoteAddr }

// --- response writes ---------------------------------------------------------

func (c *Ctx) SetHeader(key, val string) { c.W.Header().Set(key, val) }
func (c *Ctx) Status(code int)           { c.W.WriteHeader(code) }
func (c *Ctx) Written() bool             { return c.W.Written() }

// JSON writes a JSON body with the given status (gin's c.JSON).
func (c *Ctx) JSON(status int, body any) {
	c.W.Header().Set("Content-Type", "application/json; charset=utf-8")
	c.W.WriteHeader(status)
	_ = json.NewEncoder(c.W).Encode(body)
}

// Data writes a raw body with an explicit content type (used by the frontend
// fallback to serve index.html).
func (c *Ctx) Data(status int, contentType string, b []byte) {
	c.W.Header().Set("Content-Type", contentType)
	c.W.WriteHeader(status)
	_, _ = c.W.Write(b)
}

// AbortWithStatus / AbortWithStatusJSON mirror gin's helpers used by the CORS,
// introspection-gate, and recovery middleware.
func (c *Ctx) AbortWithStatus(status int) {
	c.Status(status)
	c.Abort()
}
func (c *Ctx) AbortWithStatusJSON(status int, body any) {
	c.JSON(status, body)
	c.Abort()
}

// --- context bag + errors ----------------------------------------------------

func (c *Ctx) Context() context.Context { return c.ctx }
func (c *Ctx) WithContext(ctx context.Context) {
	c.ctx = ctx
	c.R = c.R.WithContext(ctx)
}

func (c *Ctx) Set(key, val any) {
	if c.bag == nil {
		c.bag = make(map[any]any, 4)
	}
	c.bag[key] = val
}
func (c *Ctx) Get(key any) (any, bool) { v, ok := c.bag[key]; return v, ok }

// Error accumulates a handler error (gin's c.Error); the metrics/trace
// middleware read these after Next() to record failures.
func (c *Ctx) Error(err error) { c.errs = append(c.errs, err) }
func (c *Ctx) Errors() []error { return c.errs }
