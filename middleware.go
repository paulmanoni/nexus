package nexus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/di"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/middleware"
	"github.com/paulmanoni/nexus/v2/middleware/secure"
	"github.com/paulmanoni/nexus/v2/registry"
	"github.com/paulmanoni/nexus/v2/trace"
)

// Use attaches a transport-agnostic middleware bundle to a registration.
// Works on AsRest, AsQuery, AsMutation, (future AsSubscription /
// AsWebSocket) — each transport picks the realization it understands from
// the bundle (HTTP for REST/WS upgrade, Graph for GraphQL). Missing fields
// are silently ignored so a single bundle can degrade gracefully across
// transports.
//
//	rl := ratelimit.NewMiddleware(store, key, ratelimit.Limit{RPM: 30})
//	di.Provide(
//	    nexus.AsMutation(NewCreateOrder, nexus.Use(rl)),
//	    nexus.AsRest("POST", "/quick", NewQuick, nexus.Use(rl)),
//	)
//
// For app-wide coverage (every REST endpoint + GraphQL POST + WS upgrade
// + the dashboard itself) put the middleware in Config.GlobalMiddleware
// instead of naming it on each registration.
func Use(m middleware.Middleware) MiddlewareOption {
	return MiddlewareOption{mw: m}
}

// MiddlewareOption carries a Middleware across the AsRest/AsQuery/... call
// sites. Each transport's option type embeds / converts this, so a single
// nexus.Use(...) expression can appear wherever the transport accepts it.
//
// MiddlewareOption also satisfies the top-level Option interface as a
// no-op so callers can flow it through Option-typed variadic slots. The
// option still
// only takes effect via applyToRest / applyToGql / applyToWS — the
// no-op nexusOption() exists purely for type-system passage.
type MiddlewareOption struct{ mw middleware.Middleware }

// nexusOption satisfies Option. Empty di.Options because this slot is
// for transport-attaching options (RestOption/GqlOption/WSOption);
// middleware doesn't register anything globally on its own.
func (m MiddlewareOption) nexusOption() di.Option { return di.Options() }

// applyToGql wires this middleware into a GraphQL registration. Called
// by asGqlField for each MiddlewareOption passed to AsQuery/AsMutation.
// Leaves the GqlOption slice untouched when the bundle has no Graph
// realization (e.g. a gin-only rate limit); the registry still records
// the name so the dashboard's middleware list stays accurate.
func (m MiddlewareOption) applyToGql(c *gqlConfig) {
	info := m.mw.AsInfo()
	if m.mw.Graph != nil {
		c.middlewares = append(c.middlewares, namedMw{
			name:        info.Name,
			description: info.Description,
			mw:          m.mw.Graph,
		})
	}
	c.bundles = append(c.bundles, m.mw)
	stampRequiresTag(&c.baseEndpointConfig, m.mw.Requires)
}

// applyToRest wires this middleware into a REST registration. Same
// fallback rule as applyToGql — skip the handler slot if HTTP is nil, but
// always record the name for the dashboard.
func (m MiddlewareOption) applyToRest(c *restConfig) {
	c.bundles = append(c.bundles, m.mw)
	stampRequiresTag(&c.baseEndpointConfig, m.mw.Requires)
}

// applyToWS wires this middleware into an AsWS registration. Only the
// first AsWS call for a given path actually installs middleware on the
// upgrade route — subsequent registrations' bundles are ignored (with a
// warning log).
func (m MiddlewareOption) applyToWS(c *wsConfig) {
	c.bundles = append(c.bundles, m.mw)
	stampRequiresTag(&c.baseEndpointConfig, m.mw.Requires)
}

// stampRequiresTag folds a bundle's Requires metadata into the endpoint's
// registry tags. Multiple bundles append (comma-joined) — each middleware
// gates independently, so the effective requirement is the union, which is
// exactly what the joined list expresses under Requires' all-of semantics.
func stampRequiresTag(b *baseEndpointConfig, perms []string) {
	if len(perms) == 0 {
		return
	}
	joined := strings.Join(perms, ",")
	if existing := b.tags[registry.AuthRequiresTag]; existing != "" {
		joined = existing + "," + joined
	}
	b.setTag(registry.AuthRequiresTag, joined)
}

// checkBundleTransports enforces fail-closed attachment (redesign §5): a
// bundle that declares some transports but not t is misattached, and the
// registration errors at boot rather than silently no-opping — which is the
// auth-bypass footgun the redesign exists to kill. opID identifies the
// endpoint for the diagnostic ("POST /quick", "createOrder", …).
//
// A bundle that declares NO transports at all (a pure dashboard label with
// no HTTP/Graph realization, e.g. a metadata marker) is left alone: it claims
// to protect nothing, so attaching it anywhere is harmless. Only middleware
// that genuinely enforces something on transport X — and is attached to
// transport Y where it would silently not run — is rejected.
func checkBundleTransports(bundles []middleware.Middleware, t middleware.Transport, opID string) error {
	for _, b := range bundles {
		set := middleware.AsHandler(b).Transports()
		if set != 0 && !set.Has(t) {
			return fmt.Errorf(
				"nexus: middleware %q on %s op %q declares Transports = %s and cannot run on %s; "+
					"scope it with nexus.UseOnRest/UseOnGraph/UseOnWS, or give it a %s realization",
				b.Name, t, opID, set, t, t)
		}
	}
	return nil
}

// bodyLimitMiddleware caps how many bytes a request body may deliver.
//
// Without it, every JSON-binding handler is a memory-exhaustion primitive: an
// unauthenticated client opens a POST, declares nothing about length, and
// streams until the process dies. http.MaxBytesReader is the stdlib answer —
// it makes the read fail past the limit rather than buffering, so the cost is
// bounded no matter what the handler does with the body.
//
// The wrapper is transparent to handlers: they keep reading r.Body normally,
// and an over-limit read surfaces as a read error. The 413 below only fires
// when nothing downstream wrote a status of its own, so a handler that wants
// to report the overflow its own way still can.
//
// WebSocket upgrades are unaffected — they carry no body, and once the
// connection is hijacked the socket is read directly rather than through
// r.Body.
func bodyLimitMiddleware(limit int64) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		if c.Request != nil && c.Request.Body != nil && c.Request.Body != http.NoBody {
			c.Request.Body = &limitedBody{rc: c.Request.Body, w: c.Writer, limit: limit}
		}
		c.Next()
		// MaxBytesReader already sets the status to 413 on the
		// ResponseWriter when it trips, but only if nothing was written
		// first; this covers the case where a handler swallowed the read
		// error and returned without writing anything at all.
		if !c.Writer.Written() && bodyLimitExceeded(c) {
			c.AbortWithStatusJSON(middleware.ErrorBody(http.StatusRequestEntityTooLarge,
				errors.New("request body too large")))
		}
	}
}

// limitedBody applies the body cap at the first read, so an endpoint's
// MaxBody — which runs after the app-wide middleware but before its
// handler reads — can still raise or lower it.
type limitedBody struct {
	rc     io.ReadCloser
	w      http.ResponseWriter
	limit  int64 // ≤ 0: no cap
	reader io.ReadCloser
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.reader == nil {
		b.reader = b.rc
		if b.limit > 0 {
			b.reader = http.MaxBytesReader(b.w, b.rc, b.limit)
		}
	}
	return b.reader.Read(p)
}

func (b *limitedBody) Close() error { return b.rc.Close() }

// MaxBody sets the request-body cap for one endpoint, replacing the app's
// ([runtime.server] max_body_bytes, 32MB by default); n ≤ 0 removes it —
// for an upload endpoint that streams to storage, say:
//
//	nexus.AsRest("POST", "/files", (*Files).Upload, nexus.MaxBody(2<<30))
func MaxBody(n int64) MiddlewareOption {
	return Use(middleware.Middleware{
		Name:        "max-body",
		Kind:        middleware.KindBuiltin,
		Description: fmt.Sprintf("request body cap: %d bytes", n),
		HTTP: func(c *httpx.Ctx) {
			if lb, ok := c.Request.Body.(*limitedBody); ok && lb.reader == nil {
				lb.limit = n
			}
			c.Next()
		},
	})
}

// Timeout bounds one endpoint: its context is cancelled after d and the
// connection's read and write deadlines are set to match, so a slow client
// can't hold the handler past it. Long streams (SSE, downloads) simply
// don't set one; there is no app-wide write timeout by default.
func Timeout(d time.Duration) MiddlewareOption {
	return Use(middleware.Middleware{
		Name:        "timeout",
		Kind:        middleware.KindBuiltin,
		Description: "request timeout: " + d.String(),
		HTTP: func(c *httpx.Ctx) {
			deadline := time.Now().Add(d)
			rc := http.NewResponseController(c.Writer)
			_ = rc.SetReadDeadline(deadline)
			_ = rc.SetWriteDeadline(deadline)
			ctx, cancel := context.WithDeadline(c.Request.Context(), deadline)
			defer cancel()
			c.SetRequestContext(ctx)
			c.Next()
		},
	})
}

// bodyLimitExceeded reports whether any error recorded on the context came
// from the body cap.
func bodyLimitExceeded(c *httpx.Ctx) bool {
	for _, err := range c.Errors() {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return true
		}
	}
	return false
}

// recoveryMiddleware catches handler panics, captures the runtime
// stack, and threads both the panic value and the stack through gin's
// c.Error path as a *trace.StackError. The metrics middleware + trace
// publishers extract the stack via trace.StackOf and surface it on
// the dashboard, so an operator clicking the red error badge can see
// where the panic originated without grep-ing the server logs.
//
// Functionally equivalent to gin.Recovery() for the response surface
// (HTTP 500 + abort) — the value-add is the captured-stack pipeline
// for the framework's own observability.
func recoveryMiddleware() httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			// metrics.ginRecorder catches panics in its inner defer,
			// builds a *trace.StackError, records the failure, and
			// re-panics with the wrapped value so we land here with
			// the stack already captured. Reuse it; otherwise (raw
			// gin route without the metrics middleware) capture our
			// own debug.Stack() in this frame.
			var err error
			if se, ok := r.(*trace.StackError); ok {
				err = se
			} else {
				msg := fmt.Sprintf("%v", r)
				if msg == "" {
					msg = "panic"
				}
				err = &trace.StackError{
					Err:   fmt.Errorf("panic: %s", msg),
					Stack: trace.CleanStack(string(debug.Stack())),
				}
			}
			// Mirror to stderr so dev / nexus dev terminals also see
			// the panic. Without this, recovery is silent server-side
			// and an operator might miss it if the dashboard isn't
			// open. Path is the request path; method gives quick
			// triage context.
			log.Printf("[nexus] panic recovered: %s %s — %s\n%s",
				c.Request.Method, c.Request.URL.Path, err.Error(), trace.StackOf(err))
			// Attach to gin.Context so any other observer downstream
			// (rate-limit fallback, custom middleware) can read the
			// error after we abort.
			_ = c.Error(err)
			// Default response: 500 + abort, matching gin.Recovery()'s
			// behaviour. Don't overwrite a status the handler may have
			// already written.
			if !c.Writer.Written() {
				c.AbortWithStatus(http.StatusInternalServerError)
			} else {
				c.Abort()
			}
		}()
		c.Next()
	}
}

// securityStatusKey is the extValues key under which installSecurity
// stashes the resolved security posture. The extension/security
// dashboard tab reads it via App.Value so it can show what's actually
// active without re-deriving config.
const securityStatusKey = "nexus.security.status"

// installSecurity wires the built-in security middleware from
// Config.Middleware.Security. Headers are on unless explicitly disabled
// (so a nil config still hardens the app); CSRF is opt-in. It records a
// status map for the dashboard either way.
func (a *App) installSecurity(sc *config.Security) {
	headersOn := sc == nil || !sc.DisableHeaders
	csrfOn := sc != nil && sc.CSRF != nil && *sc.CSRF
	// Unset: decided once every option has run, by whether anything the
	// app uses asked for it (RequireCSRF).
	a.csrfAuto = sc == nil || sc.CSRF == nil
	a.securityConfig = sc

	if headersOn {
		hc := secure.HeadersConfig{}
		if sc != nil {
			hc.FrameOptions = sc.FrameOptions
			hc.ReferrerPolicy = sc.ReferrerPolicy
			hc.ContentSecurityPolicy = sc.CSP
			if sc.HSTSMaxAge > 0 {
				hc.HSTS = &secure.HSTSConfig{MaxAge: sc.HSTSMaxAge}
			}
		}
		secure.ApplyHeaderDefaults(&hc)
		a.engine.Use(secure.HeadersHandler(&hc))
		a.registry.RegisterMiddleware(middleware.Info{
			Name:        "security-headers",
			Kind:        middleware.KindBuiltin,
			Description: "Security response headers (built-in)",
		})
		a.registry.RegisterGlobalMiddleware("security-headers")
	}

	if csrfOn {
		a.installCSRF("forced on by config")
	}

	a.SetValue(securityStatusKey, map[string]any{
		"headers": headersOn,
		"csrf":    csrfOn,
	})
}

// RequireCSRF records that something the app uses — cookie sessions, an
// auth scheme reading a cookie, Inertia or server-rendered forms — needs
// CSRF protection. Extensions call it from their module; unless the
// config forces CSRF off, the double-submit check is then installed once
// every option has run.
func (a *App) RequireCSRF(reason string) {
	a.csrfReasons = append(a.csrfReasons, reason)
	if a.csrfInstalled || !a.csrfAuto || !a.csrfLate {
		return
	}
	a.installCSRF(reason)
}

// installAutoCSRF runs after every option: CSRF goes on when it was left
// to the app and something asked for it.
func (a *App) installAutoCSRF() {
	a.csrfLate = true
	if a.csrfAuto && !a.csrfInstalled && len(a.csrfReasons) > 0 {
		a.installCSRF(strings.Join(a.csrfReasons, ", "))
	}
}

func (a *App) installCSRF(reason string) {
	cc := secure.CSRFConfig{}
	if a.securityConfig != nil {
		cc.CookieSecure = a.securityConfig.CSRFCookieSecure
	}
	secure.ApplyCSRFDefaults(&cc)
	a.engine.Use(secure.CSRFHandler(&cc))
	a.registry.RegisterMiddleware(middleware.Info{
		Name:        "csrf",
		Kind:        middleware.KindBuiltin,
		Description: "CSRF double-submit check (built-in): " + reason,
	})
	a.registry.RegisterGlobalMiddleware("csrf")
	a.csrfInstalled = true
	if v, ok := a.Value(securityStatusKey); ok {
		if st, ok := v.(map[string]any); ok {
			st["csrf"] = true
		}
	}
}

// Middleware registers app-wide middleware: it runs on every request —
// REST, GraphQL, WebSocket upgrades, unmatched paths — ahead of any
// endpoint's own. Each entry is a middleware.Middleware, or a constructor
// returning one (optionally with an error) whose parameters come from DI
// like a provider's, so a middleware can depend on services:
//
//	var Middleware = nexus.Middleware(
//	    edge.Compress,      // middleware.Middleware with Stage: middleware.Edge
//	    admin.ThemeHead,    // func(doc *nexus.Document) middleware.Middleware
//	    auth.RequestedTarget,
//	)
//
// Order comes from each middleware's Stage (Edge, Session, Auth, App),
// then from declaration order across the whole app — never from where a
// module happens to sit in Boot's list.
func Middleware(entries ...any) Option {
	opts := make([]Option, 0, len(entries))
	for _, e := range entries {
		if m, ok := e.(middleware.Middleware); ok {
			opts = append(opts, Invoke(func(a *App) { a.addAppMiddleware(m) }))
			continue
		}
		opts = append(opts, middlewareConstructor(e))
	}
	return Options(opts...)
}

var (
	middlewareType = reflect.TypeOf(middleware.Middleware{})
	errorType      = reflect.TypeOf((*error)(nil)).Elem()
	appPtrType     = reflect.TypeOf((*App)(nil))
)

// middlewareConstructor invokes fn with its parameters from DI and adds the
// middleware it returns.
func middlewareConstructor(fn any) Option {
	v := reflect.ValueOf(fn)
	t := v.Type()
	if t.Kind() != reflect.Func || t.IsVariadic() || t.NumOut() == 0 || t.NumOut() > 2 ||
		t.Out(0) != middlewareType || (t.NumOut() == 2 && t.Out(1) != errorType) {
		return rawOption{di.Error(fmt.Errorf("nexus.Middleware: %T is neither a middleware.Middleware nor a func(deps…) middleware.Middleware [, error]", fn))}
	}
	in := []reflect.Type{appPtrType}
	for i := 0; i < t.NumIn(); i++ {
		in = append(in, t.In(i))
	}
	invoke := reflect.MakeFunc(reflect.FuncOf(in, []reflect.Type{errorType}, false), func(args []reflect.Value) []reflect.Value {
		out := v.Call(args[1:])
		if len(out) == 2 && !out[1].IsNil() {
			return []reflect.Value{out[1]}
		}
		args[0].Interface().(*App).addAppMiddleware(out[0].Interface().(middleware.Middleware))
		return []reflect.Value{reflect.Zero(errorType)}
	})
	return Invoke(invoke.Interface())
}

func (a *App) addAppMiddleware(m middleware.Middleware) {
	if a.appMiddlewareInstalled {
		a.installMiddleware(m)
		return
	}
	a.appMiddleware = append(a.appMiddleware, m)
}

// installAppMiddleware puts the declared app-wide middleware on the router
// in stage order. It runs once, after every option (fxLateOptions).
func (a *App) installAppMiddleware() {
	sort.SliceStable(a.appMiddleware, func(i, j int) bool {
		return a.appMiddleware[i].Stage.Rank() < a.appMiddleware[j].Stage.Rank()
	})
	for _, m := range a.appMiddleware {
		a.installMiddleware(m)
	}
	a.appMiddlewareInstalled = true
}

func (a *App) installMiddleware(m middleware.Middleware) {
	if m.HTTP != nil {
		a.engine.Use(m.HTTP)
	}
	info := m.AsInfo()
	info.Stage = m.Stage.String()
	a.registry.RegisterMiddleware(info)
	a.registry.RegisterGlobalMiddleware(m.Name)
}
