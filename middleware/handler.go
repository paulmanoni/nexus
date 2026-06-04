package middleware

import "context"

// Handler is the unified middleware shape (see
// docs/design/middleware-redesign.md §3). Transitional name — it becomes
// Middleware in step 5, once the current Middleware struct is renamed to
// Bundle. A single implementation serves every transport it declares in
// Transports().
type Handler interface {
	Name() string
	Transports() TransportSet
	Handle(rc *RequestCtx, next Next) error
}

// Next advances the chain. Returning an error (or calling rc.Reject)
// short-circuits; the active carrier translates the outcome per transport.
type Next func(*RequestCtx) error

// Func is the closure adapter — most middleware is one of these.
type Func struct {
	name string
	set  TransportSet
	fn   func(rc *RequestCtx, next Next) error
}

// NewFunc builds a Func Handler.
func NewFunc(name string, set TransportSet, fn func(rc *RequestCtx, next Next) error) Func {
	return Func{name: name, set: set, fn: fn}
}

func (f Func) Name() string                        { return f.name }
func (f Func) Transports() TransportSet            { return f.set }
func (f Func) Handle(rc *RequestCtx, n Next) error { return f.fn(rc, n) }

// carrier is the per-transport backing behind RequestCtx. Each adapter (step
// 3+) implements it over *gin.Context, graph.ResolveParams, or a WS frame.
// Unexported for now: in steps 1–2 only in-package tests construct a
// RequestCtx. Step 3 decides whether to export it (or move adapters to an
// internal sub-package) so package nexus can build one.
type carrier interface {
	header(key string) string
	clientIP() string
	path() string
	setHeader(key, val string)
	reject(status int, err error) error
}

// RequestCtx is the transport-neutral request handle middleware sees (§3.2).
// Middleware never touches gin.Context or graph.ResolveParams — those are
// carrier internals.
type RequestCtx struct {
	Context   context.Context
	Transport Transport

	carrier carrier
	bag     map[any]any
}

// newRequestCtx is the constructor adapters (and in-package tests) call.
func newRequestCtx(ctx context.Context, t Transport, c carrier) *RequestCtx {
	return &RequestCtx{Context: ctx, Transport: t, carrier: c}
}

// Header returns the named request header, transport-backed.
func (rc *RequestCtx) Header(key string) string { return rc.carrier.header(key) }

// ClientIP returns the request's client IP via a single shared implementation.
func (rc *RequestCtx) ClientIP() string { return rc.carrier.clientIP() }

// Path returns the REST route or GraphQL field path.
func (rc *RequestCtx) Path() string { return rc.carrier.path() }

// SetHeader sets a response header on REST/WS (e.g. Retry-After). On GraphQL
// — which has no per-field response headers — it is a no-op, so middleware
// can call it unconditionally without branching on transport.
func (rc *RequestCtx) SetHeader(key, val string) { rc.carrier.setHeader(key, val) }

// WithContext replaces the underlying context — the functional equivalent of
// stashing a value on gin.Context for downstream middleware and the handler.
func (rc *RequestCtx) WithContext(ctx context.Context) { rc.Context = ctx }

// Set shares a typed value across the chain without internal-package coupling.
func (rc *RequestCtx) Set(key, val any) {
	if rc.bag == nil {
		rc.bag = make(map[any]any, 4)
	}
	rc.bag[key] = val
}

// Get reads a value stashed by Set.
func (rc *RequestCtx) Get(key any) (any, bool) {
	v, ok := rc.bag[key]
	return v, ok
}

// Reject short-circuits the chain. Returns the error so callers can write
// `return rc.Reject(401, ErrUnauthenticated)`; the carrier renders the
// transport-appropriate response.
func (rc *RequestCtx) Reject(status int, err error) error {
	return rc.carrier.reject(status, err)
}
