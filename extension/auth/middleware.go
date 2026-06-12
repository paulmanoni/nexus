package auth

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus"
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
		// Install the reject hook so a per-op Required/Requires gate's
		// rc.Reject(401/403) runs this module's OnUnauthenticated/OnForbidden
		// callbacks via the gin carrier (which holds the *gin.Context).
		ctx = middleware.WithRejectHook(ctx, authRejectHook)

		id, token, err := state.authenticate(ctx, c.Request)
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
	return nexus.Use(builtin("auth:required",
		"Requires an authenticated identity on ctx",
		func(rc *middleware.RequestCtx, next middleware.Next) error {
			if _, ok := IdentityFrom(rc.Context); !ok {
				return rejectAuth(rc, ErrUnauthenticated)
			}
			return next(rc)
		}))
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
	return nexus.Use(builtin(name,
		"Requires one or more permissions on the identity",
		func(rc *middleware.RequestCtx, next middleware.Next) error {
			id, ok := IdentityFrom(rc.Context)
			if !ok {
				return rejectAuth(rc, ErrUnauthenticated)
			}
			if !checkPermissions(rc.Context, id, perms) {
				return rejectAuth(rc, ErrForbidden)
			}
			return next(rc)
		}))
}

// Optional is a no-op bundle that exists purely as dashboard signal —
// it labels the endpoint as auth-aware without enforcing presence.
// Useful for public endpoints that still personalize when a user is
// logged in, so the UI surfaces "this endpoint reads identity".
func Optional() nexus.MiddlewareOption {
	return nexus.Use(builtin("auth:optional",
		"Reads identity when present; does not enforce it",
		func(rc *middleware.RequestCtx, next middleware.Next) error { return next(rc) }))
}

// --- unified gate plumbing ----------------------------------------------

// builtin wraps a single transport-neutral Handler into a builtin bundle.
// One implementation now serves REST, WS, and GraphQL — FromHandler generates
// the per-transport realizations, replacing the old ginX/graphX pairs.
func builtin(name, desc string, fn func(*middleware.RequestCtx, middleware.Next) error) middleware.Middleware {
	mw := middleware.FromHandler(middleware.NewFunc(name, middleware.AllTransports, fn))
	mw.Description = desc
	mw.Kind = middleware.KindBuiltin
	return mw
}

// rejectAuth renders an auth denial for the active transport. GraphQL routes
// through wrapGraphErr (Config.GraphQLErrorWrap + the auth.reject event);
// REST/WS rc.Reject hands off to authRejectHook via the gin carrier, which
// runs the app's OnUnauthenticated/OnForbidden callbacks. Status follows the
// sentinel: ErrForbidden → 403, otherwise 401.
func rejectAuth(rc *middleware.RequestCtx, err error) error {
	if rc.Transport == middleware.TransportGraphQL {
		return wrapGraphErr(rc.Context, err)
	}
	status := http.StatusUnauthorized
	if err == ErrForbidden {
		status = http.StatusForbidden
	}
	return rc.Reject(status, err)
}

// authRejectHook is installed on every REST request by ginAuthMiddleware and
// invoked by the gin carrier on rc.Reject. It owns 401/403 (returning false
// for any other status so unrelated middleware — e.g. rate limiting — keep the
// carrier's default abort). Mirrors the old rejectUnauthenticated /
// rejectForbidden exactly: emit the event, run the configured hook, and force
// the status if a misconfigured hook didn't abort.
func authRejectHook(c *gin.Context, status int, err error) bool {
	reason := "unauthenticated"
	if status == http.StatusForbidden {
		reason = "forbidden"
	} else if status != http.StatusUnauthorized {
		return false
	}
	emitReject(c.Request.Context(), reason, status, err)

	var hook func(*gin.Context, error)
	if s, ok := stateFrom(c.Request.Context()); ok {
		if status == http.StatusForbidden {
			hook = s.cfg.OnForbidden
		} else {
			hook = s.cfg.OnUnauthenticated
		}
	}
	if hook != nil {
		hook(c, err)
		if !c.IsAborted() {
			c.AbortWithStatus(status)
		}
	} else {
		c.AbortWithStatusJSON(status, gin.H{"error": err.Error()})
	}
	return true
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
