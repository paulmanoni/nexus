// Package httpx is nexus's router-agnostic HTTP seam.
//
// Historically nexus bound directly to gin: endpoint chains were
// []gin.HandlerFunc, flow control was gin's c.Next()/c.Abort(), and routes
// were mounted on a *gin.Engine. httpx removes that coupling. The framework
// holds an httpx.Router and every handler/middleware sees an *httpx.Ctx; the
// concrete router (gin, chi, net/http) lives behind a thin adapter under
// httpx/ginrouter, httpx/chirouter, httpx/stdrouter.
//
// The pivotal design choice: chain execution lives HERE, not in the router.
// *Ctx owns the handler slice, the index, and Next()/Abort() — gin's exact
// semantics, reimplemented once. An adapter therefore only matches a path and
// returns its params; it never runs middleware. That is what lets the same
// middleware run unchanged on any backend.
//
// Canonical route syntax is gin's (":id", "*rest"); chi/std adapters translate
// on registration so existing nexus route strings need no rewrite.
package httpx

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// H is the JSON map shorthand (drop-in for gin.H).
type H = map[string]any

// HandlerFunc is the one handler/middleware shape. Middleware calls c.Next()
// to advance; returning without it (or calling c.Abort()) stops the chain.
type HandlerFunc func(*Ctx)

// RouteInfo is one mounted route, for dashboard introspection (gin.RouteInfo).
type RouteInfo struct {
	Method string
	Path   string
}

// Router is the seam every backend implements. The framework never names a
// concrete router type — it holds this.
type Router interface {
	// Handle mounts a chain at method+path (canonical ":id"/"*rest" syntax).
	Handle(method, path string, chain ...HandlerFunc)
	// GET/POST/... are method sugar over Handle.
	GET(path string, chain ...HandlerFunc)
	POST(path string, chain ...HandlerFunc)
	PUT(path string, chain ...HandlerFunc)
	DELETE(path string, chain ...HandlerFunc)
	PATCH(path string, chain ...HandlerFunc)
	OPTIONS(path string, chain ...HandlerFunc)
	HEAD(path string, chain ...HandlerFunc)
	// Any mounts the chain for every standard method (gin's Any).
	Any(path string, chain ...HandlerFunc)
	// Use appends app-wide middleware applied to routes registered afterward.
	Use(mw ...HandlerFunc)
	// Group returns a sub-router rooted at prefix, carrying mw + parent state.
	Group(prefix string, mw ...HandlerFunc) Group
	// NoRoute sets the fallback chain (SPA index.html).
	NoRoute(chain ...HandlerFunc)
	// Static serves a directory under a URL prefix.
	Static(prefix, dir string)
	// Routes lists mounted routes for the dashboard.
	Routes() []RouteInfo
	// Run binds and serves (backend's ListenAndServe).
	Run(addr string) error
	http.Handler
}

// Group is a sub-router (gin.RouterGroup). Dashboards mount under one.
type Group interface {
	Handle(method, path string, chain ...HandlerFunc)
	GET(path string, chain ...HandlerFunc)
	POST(path string, chain ...HandlerFunc)
	PUT(path string, chain ...HandlerFunc)
	DELETE(path string, chain ...HandlerFunc)
	PATCH(path string, chain ...HandlerFunc)
	OPTIONS(path string, chain ...HandlerFunc)
	HEAD(path string, chain ...HandlerFunc)
	Any(path string, chain ...HandlerFunc)
	Use(mw ...HandlerFunc)
	Group(prefix string, mw ...HandlerFunc) Group
	Static(prefix, dir string)
}

// StdMethods is the set Any mounts over.
var StdMethods = []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS", "HEAD"}

// ResponseWriter wraps the backend writer to track status + bytes-written
// (the framework relies on Written() to avoid clobbering a handler that wrote
// its own response). It transparently forwards Flush/Hijack so SSE streaming
// and WebSocket upgrades keep working on any backend.
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

// Written reports whether the header/body was started.
func (w *ResponseWriter) Written() bool { return w.written }

// Flush forwards to the backend writer (SSE: ext_devreload streams over this).
func (w *ResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the backend writer so gorilla/websocket can upgrade.
func (w *ResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("httpx: ResponseWriter does not support Hijack")
}

// Ctx is the transport-neutral request handle — the union of the gin.Context
// surface nexus actually used, with no router type in its API.
type Ctx struct {
	Writer  *ResponseWriter
	Request *http.Request

	route string
	param func(string) string

	handlers []HandlerFunc
	index    int
	aborted  bool

	keys     map[string]any
	errs     []error
	sameSite http.SameSite
}

// Serve is the single per-request entry every adapter calls: build a Ctx over
// the backend's writer/request/param-getter and run the chain. By the time we
// get here the router has done its only job — match + params.
func Serve(chain []HandlerFunc, w http.ResponseWriter, r *http.Request, route string, param func(string) string) {
	rw, ok := w.(*ResponseWriter)
	if !ok {
		rw = &ResponseWriter{ResponseWriter: w, status: http.StatusOK}
	}
	c := &Ctx{
		Writer:   rw,
		Request:  r,
		route:    route,
		param:    param,
		handlers: chain,
		index:    -1,
	}
	c.Next()
}

// NewCtx builds a standalone Ctx over a writer/request — for tests and for
// callers that have an http.ResponseWriter + *http.Request and want the neutral
// handle without a router (no chain; Next is a no-op). Optional param resolves
// path params.
func NewCtx(w http.ResponseWriter, r *http.Request, param ...func(string) string) *Ctx {
	rw, ok := w.(*ResponseWriter)
	if !ok {
		rw = &ResponseWriter{ResponseWriter: w, status: http.StatusOK}
	}
	var pf func(string) string
	if len(param) > 0 {
		pf = param[0]
	}
	return &Ctx{Writer: rw, Request: r, route: r.URL.Path, param: pf, index: -1}
}

// --- chain flow control (owned here, not by the router) ----------------------

// Next runs the remaining handlers. Mirrors gin's loop so a recovery
// middleware's `defer recover(); c.Next()` still catches downstream panics.
func (c *Ctx) Next() {
	c.index++
	for c.index < len(c.handlers) {
		c.handlers[c.index](c)
		c.index++
	}
}

// Abort stops the chain after the current handler (gin semantics).
func (c *Ctx) Abort() {
	c.aborted = true
	c.index = len(c.handlers) + 1
}

func (c *Ctx) IsAborted() bool { return c.aborted }

// --- request reads -----------------------------------------------------------

func (c *Ctx) Method() string   { return c.Request.Method }
func (c *Ctx) FullPath() string { return c.route }
func (c *Ctx) Path() string     { return c.Request.URL.Path }

func (c *Ctx) Param(key string) string {
	if c.param == nil {
		return ""
	}
	return c.param(key)
}

// WildcardName returns the name of the trailing wildcard segment in a canonical
// route path ("/assets/*filepath" -> "filepath"), or "" when the path has no
// wildcard. Router adapters use it to normalize the wildcard param value to
// gin's convention — a leading-slash suffix (gin's c.Param("filepath") for
// /assets/app.js is "/app.js") — on every backend, so handlers that build paths
// like "assets"+c.Param("filepath") behave identically on gin, chi, and stdlib.
func WildcardName(path string) string {
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, "*") {
			return seg[1:]
		}
	}
	return ""
}

func (c *Ctx) Query(key string) string { return c.Request.URL.Query().Get(key) }

func (c *Ctx) DefaultQuery(key, def string) string {
	if v := c.Request.URL.Query(); v.Has(key) {
		return v.Get(key)
	}
	return def
}

// GetHeader reads a request header (gin's c.GetHeader).
func (c *Ctx) GetHeader(key string) string { return c.Request.Header.Get(key) }

// RemoteIP returns the actual TCP peer IP (host part of RemoteAddr), ignoring
// X-Forwarded-For — unspoofable, for security gates. Mirrors gin's c.RemoteIP().
func (c *Ctx) RemoteIP() string {
	if host, _, err := net.SplitHostPort(c.Request.RemoteAddr); err == nil {
		return host
	}
	return c.Request.RemoteAddr
}

// FileFromFS serves a single file from an http.FileSystem (gin's c.FileFromFS).
func (c *Ctx) FileFromFS(filepath string, fs http.FileSystem) {
	orig := c.Request.URL.Path
	c.Request.URL.Path = filepath
	defer func() { c.Request.URL.Path = orig }()
	http.FileServer(fs).ServeHTTP(c.Writer, c.Request)
}

// Cookie reads a request cookie value.
func (c *Ctx) Cookie(name string) (string, error) {
	ck, err := c.Request.Cookie(name)
	if err != nil {
		return "", err
	}
	return ck.Value, nil
}

// Context returns the request's context (gin's c.Request.Context()).
func (c *Ctx) Context() context.Context { return c.Request.Context() }

// SetRequestContext swaps the request's context (the auth/trace pattern of
// c.Request = c.Request.WithContext(ctx)).
func (c *Ctx) SetRequestContext(ctx context.Context) {
	c.Request = c.Request.WithContext(ctx)
}

// ClientIP returns a best-effort client IP (X-Forwarded-For / X-Real-IP /
// RemoteAddr). The framework's trusted-proxy policy lives in middleware.ClientIP;
// this is the simple default used when a Ctx is asked directly.
func (c *Ctx) ClientIP() string {
	if xff := c.Request.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xr := c.Request.Header.Get("X-Real-IP"); xr != "" {
		return strings.TrimSpace(xr)
	}
	if host, _, err := net.SplitHostPort(c.Request.RemoteAddr); err == nil {
		return host
	}
	return c.Request.RemoteAddr
}

// --- response writes ---------------------------------------------------------

// Header sets a response header (gin's c.Header). With an empty value it
// deletes the header, matching gin.
func (c *Ctx) Header(key, val string) {
	if val == "" {
		c.Writer.Header().Del(key)
		return
	}
	c.Writer.Header().Set(key, val)
}

func (c *Ctx) Status(code int) { c.Writer.WriteHeader(code) }
func (c *Ctx) Written() bool   { return c.Writer.Written() }

// setContentType sets Content-Type only when the handler hasn't already chosen
// one — matching gin's render.writeContentType, so a c.Header("Content-Type",…)
// set before c.JSON/String/Data is honored rather than clobbered.
func (c *Ctx) setContentType(ct string) {
	h := c.Writer.Header()
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", ct)
	}
}

// JSON writes a JSON body with the given status (gin's c.JSON).
func (c *Ctx) JSON(status int, body any) {
	c.setContentType("application/json; charset=utf-8")
	c.Writer.WriteHeader(status)
	_ = json.NewEncoder(c.Writer).Encode(body)
}

// String writes a plain-text body (gin's c.String, simplified: nexus only ever
// writes a finished string, never a printf format — keeping it non-variadic
// also avoids vet's format-string check on dynamic bodies).
func (c *Ctx) String(status int, s string) {
	c.setContentType("text/plain; charset=utf-8")
	c.Writer.WriteHeader(status)
	_, _ = c.Writer.Write([]byte(s))
}

// Stringf writes a printf-formatted text body, for the rare caller that needs
// formatting (kept separate from String so the common path stays vet-clean).
func (c *Ctx) Stringf(status int, format string, args ...any) {
	c.setContentType("text/plain; charset=utf-8")
	c.Writer.WriteHeader(status)
	fmt.Fprintf(c.Writer, format, args...)
}

// Data writes a raw body with an explicit content type (gin's c.Data).
func (c *Ctx) Data(status int, contentType string, b []byte) {
	// Data takes an explicit content type, so it sets unconditionally (gin's
	// c.Data behavior) — the caller named the type they want.
	c.Writer.Header().Set("Content-Type", contentType)
	c.Writer.WriteHeader(status)
	_, _ = c.Writer.Write(b)
}

// Redirect issues an HTTP redirect (gin's c.Redirect).
func (c *Ctx) Redirect(status int, location string) {
	http.Redirect(c.Writer, c.Request, location, status)
}

// SetSameSite + SetCookie mirror gin's cookie helpers used by auth/cookie.go.
func (c *Ctx) SetSameSite(s http.SameSite) { c.sameSite = s }

func (c *Ctx) SetCookie(name, value string, maxAge int, path, domain string, secure, httpOnly bool) {
	if path == "" {
		path = "/"
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    value,
		MaxAge:   maxAge,
		Path:     path,
		Domain:   domain,
		Secure:   secure,
		HttpOnly: httpOnly,
		SameSite: c.sameSite,
	})
}

// AbortWithStatus / AbortWithStatusJSON mirror gin's abort helpers.
func (c *Ctx) AbortWithStatus(status int) {
	c.Status(status)
	c.Abort()
}

func (c *Ctx) AbortWithStatusJSON(status int, body any) {
	c.JSON(status, body)
	c.Abort()
}

// --- context bag + errors ----------------------------------------------------

// Set/Get share typed values across the chain (gin's c.Set/c.Get, string keys).
func (c *Ctx) Set(key string, val any) {
	if c.keys == nil {
		c.keys = make(map[string]any, 4)
	}
	c.keys[key] = val
}

func (c *Ctx) Get(key string) (any, bool) {
	v, ok := c.keys[key]
	return v, ok
}

// Error accumulates a handler error (gin's c.Error) and returns it, so callers
// can keep gin's `_ = c.Error(err)` form. metrics/trace read these after Next()
// to record failures.
func (c *Ctx) Error(err error) error {
	if err != nil {
		c.errs = append(c.errs, err)
	}
	return err
}

func (c *Ctx) Errors() []error { return c.errs }

// LastError returns the most recent accumulated error, or nil (replaces gin's
// c.Errors.Last().Err).
func (c *Ctx) LastError() error {
	if len(c.errs) == 0 {
		return nil
	}
	return c.errs[len(c.errs)-1]
}

// ErrorsString joins accumulated errors (replaces gin's c.Errors.String()).
func (c *Ctx) ErrorsString() string {
	if len(c.errs) == 0 {
		return ""
	}
	parts := make([]string, len(c.errs))
	for i, e := range c.errs {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "\n")
}
