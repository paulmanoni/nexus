package auth

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/graph"
	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/trace"
)

// ginAuthMiddleware is the global Gin middleware installed by Module.
// Per request:
//   - Stash moduleState on ctx so per-op bundles read the right config
//   - Extract token (if any). Absent token → anonymous request; let
//     Required/Requires at the per-op layer decide whether that's OK.
//   - Resolve (and cache) → attach Identity to ctx.
//   - On resolver failure we do NOT 401 here — that's per-op Required's
//     job. A public endpoint on the same app should stay accessible
//     even if a bogus Authorization header comes along.
func ginAuthMiddleware(state *moduleState) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := withState(c.Request.Context(), state)

		token, hasToken := state.cfg.Extract.Extract(c.Request)
		if hasToken {
			id, err := state.resolve(ctx, token)
			if err != nil {
				if state.cfg.OnFail != nil {
					state.cfg.OnFail(ctx, token, err)
				}
			} else if id != nil {
				ctx = WithIdentity(ctx, id)
				if state.cfg.OnResolve != nil {
					state.cfg.OnResolve(ctx, id)
				}
			}
		}

		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// Required returns a cross-transport middleware bundle that rejects any
// request lacking a resolved Identity on ctx. 401 on REST, a graphql-
// native error on GraphQL — same bundle attaches cleanly to both.
//
//	nexus.AsMutation(NewCreateAdvert, auth.Required())
func Required() nexus.MiddlewareOption {
	return nexus.Use(middleware.Middleware{
		Name:        "auth:required",
		Description: "Requires an authenticated identity on ctx",
		Kind:        middleware.KindBuiltin,
		Gin:         ginRequired,
		Graph:       graphRequired,
	})
}

// Requires returns a cross-transport bundle that rejects requests whose
// Identity doesn't satisfy every listed permission (roles / scopes).
// Implies authentication — attaching Requires without Required still
// 401s on anonymous requests, because you can't evaluate permissions
// on a nil identity.
//
//	nexus.AsMutation(NewCreateAdvert, auth.Requires("ROLE_CREATE_ADVERT"))
func Requires(perms ...string) nexus.MiddlewareOption {
	name := "auth:requires"
	if len(perms) > 0 {
		name = "auth:requires:" + joinPerms(perms)
	}
	return nexus.Use(middleware.Middleware{
		Name:        name,
		Description: "Requires one or more permissions on the identity",
		Kind:        middleware.KindBuiltin,
		Gin:         ginRequires(perms),
		Graph:       graphRequires(perms),
	})
}

// Optional is a no-op bundle that exists purely as dashboard signal —
// it labels the endpoint as auth-aware without enforcing presence.
// Useful for public endpoints that still personalize when a user is
// logged in, so the UI surfaces "this endpoint reads identity".
func Optional() nexus.MiddlewareOption {
	noop := func(c *gin.Context) { c.Next() }
	graphNoop := func(next graph.FieldResolveFn) graph.FieldResolveFn {
		return func(p graph.ResolveParams) (any, error) { return next(p) }
	}
	return nexus.Use(middleware.Middleware{
		Name:        "auth:optional",
		Description: "Reads identity when present; does not enforce it",
		Kind:        middleware.KindBuiltin,
		Gin:         noop,
		Graph:       graphNoop,
	})
}

// --- per-op enforcement primitives --------------------------------------

func ginRequired(c *gin.Context) {
	if _, ok := IdentityFrom(c.Request.Context()); !ok {
		rejectUnauthenticated(c, ErrUnauthenticated)
		return
	}
	c.Next()
}

func graphRequired(next graph.FieldResolveFn) graph.FieldResolveFn {
	return func(p graph.ResolveParams) (any, error) {
		if _, ok := IdentityFrom(p.Context); !ok {
			return nil, wrapGraphErr(p.Context, ErrUnauthenticated)
		}
		return next(p)
	}
}

func ginRequires(perms []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := IdentityFrom(c.Request.Context())
		if !ok {
			rejectUnauthenticated(c, ErrUnauthenticated)
			return
		}
		if !checkPermissions(c.Request.Context(), id, perms) {
			rejectForbidden(c, ErrForbidden)
			return
		}
		c.Next()
	}
}

func graphRequires(perms []string) graph.FieldMiddleware {
	return func(next graph.FieldResolveFn) graph.FieldResolveFn {
		return func(p graph.ResolveParams) (any, error) {
			id, ok := IdentityFrom(p.Context)
			if !ok {
				return nil, wrapGraphErr(p.Context, ErrUnauthenticated)
			}
			if !checkPermissions(p.Context, id, perms) {
				return nil, wrapGraphErr(p.Context, ErrForbidden)
			}
			return next(p)
		}
	}
}

// rejectUnauthenticated writes the 401 response. If the app supplied
// Config.OnUnauthenticated, we defer to it and force a 401 fallback
// if the hook returned without aborting (misconfigured hooks must
// not accidentally let a request through).
func rejectUnauthenticated(c *gin.Context, err error) {
	emitReject(c.Request.Context(), "unauthenticated", http.StatusUnauthorized, err)
	if s, ok := stateFrom(c.Request.Context()); ok && s.cfg.OnUnauthenticated != nil {
		s.cfg.OnUnauthenticated(c, err)
		if !c.IsAborted() {
			c.AbortWithStatus(http.StatusUnauthorized)
		}
		return
	}
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
}

func rejectForbidden(c *gin.Context, err error) {
	emitReject(c.Request.Context(), "forbidden", http.StatusForbidden, err)
	if s, ok := stateFrom(c.Request.Context()); ok && s.cfg.OnForbidden != nil {
		s.cfg.OnForbidden(c, err)
		if !c.IsAborted() {
			c.AbortWithStatus(http.StatusForbidden)
		}
		return
	}
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": err.Error()})
}

// wrapGraphErr routes the auth sentinels through Config.GraphQLErrorWrap
// when set so the GraphQL errors array carries whatever shape the app
// expects. Pass-through when no wrap is configured. Emits the same
// auth.reject trace event the Gin reject path does so the dashboard
// sees GraphQL denials too.
func wrapGraphErr(ctx context.Context, err error) error {
	status := http.StatusUnauthorized
	reason := "unauthenticated"
	if err == ErrForbidden {
		status = http.StatusForbidden
		reason = "forbidden"
	}
	emitReject(ctx, reason, status, err)
	if s, ok := stateFrom(ctx); ok && s.cfg.GraphQLErrorWrap != nil {
		return s.cfg.GraphQLErrorWrap(err)
	}
	return err
}

// emitReject publishes an auth.reject trace event on the request's
// trace bus. First preference: the bus stashed on ctx by the per-route
// trace.Middleware (carries a live span so events land on the right
// endpoint row). Fallback: the app-level bus captured on moduleState
// at Module wire time — needed because AsRest installs trace.Middleware
// AFTER the auth bundles in the handler chain, so ctx lookup misses
// on the reject path.
func emitReject(ctx context.Context, reason string, status int, err error) {
	bus, _ := trace.BusFromCtx(ctx)
	if bus == nil {
		if s, ok := stateFrom(ctx); ok {
			bus = s.bus
		}
	}
	if bus == nil {
		return
	}
	span, _ := trace.SpanFromCtx(ctx)
	ev := trace.Event{
		Kind:   "auth.reject",
		Status: status,
	}
	if span != nil {
		ev.TraceID = span.TraceID
		ev.Service = span.Service
		ev.Endpoint = span.Endpoint
	}
	if err != nil {
		ev.Error = err.Error()
	}
	meta := map[string]any{"reason": reason}
	if id, ok := IdentityFrom(ctx); ok && id != nil {
		// When we reject an authenticated identity (403), include its
		// ID so admins can tie dashboard rows back to a real user.
		// Unauthenticated rejects have no identity — meta stays lean.
		meta["identity"] = id.ID
	}
	ev.Meta = meta
	bus.Publish(ev)
}

// checkPermissions runs the configured PermissionFn if the moduleState
// is on ctx, otherwise falls back to the package default. The ctx
// fallback keeps unit tests that skip auth.Module useful.
func checkPermissions(ctx context.Context, id *Identity, perms []string) bool {
	if s, ok := stateFrom(ctx); ok && s.permissions != nil {
		return s.permissions(id, perms)
	}
	return DefaultPermissions(id, perms)
}

func joinPerms(perms []string) string {
	if len(perms) == 0 {
		return ""
	}
	out := perms[0]
	for _, p := range perms[1:] {
		out += "," + p
	}
	return out
}
