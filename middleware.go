package nexus

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"strings"

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
// the bundle (Gin for REST/WS upgrade, Graph for GraphQL). Missing fields
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
// no-op so callers can flow it through Option-typed variadic slots
// (notably nexus.AsCRUD, which accepts ...Option). The option still
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
// fallback rule as applyToGql — skip the handler slot if Gin is nil, but
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
// no Gin/Graph realization, e.g. a metadata marker) is left alone: it claims
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
		if c.Request != nil && c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		}
		c.Next()
		// MaxBytesReader already sets the status to 413 on the
		// ResponseWriter when it trips, but only if nothing was written
		// first; this covers the case where a handler swallowed the read
		// error and returned without writing anything at all.
		if !c.Writer.Written() && bodyLimitExceeded(c) {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge,
				httpx.H{"error": "request body too large"})
		}
	}
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
	csrfOn := sc != nil && sc.EnableCSRF

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
		cc := secure.CSRFConfig{}
		if sc != nil {
			cc.CookieSecure = sc.CSRFCookieSecure
		}
		secure.ApplyCSRFDefaults(&cc)
		a.engine.Use(secure.CSRFHandler(&cc))
		a.registry.RegisterMiddleware(middleware.Info{
			Name:        "csrf",
			Kind:        middleware.KindBuiltin,
			Description: "CSRF double-submit check (built-in)",
		})
		a.registry.RegisterGlobalMiddleware("csrf")
	}

	a.SetValue(securityStatusKey, map[string]any{
		"headers": headersOn,
		"csrf":    csrfOn,
	})
}
