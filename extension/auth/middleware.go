package auth

import (
	"context"
	"net/http"

	"github.com/paulmanoni/nexus/httpx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/trace"
)

// authMiddleware is the global auth middleware installed by Module on the
// app's httpx.Router (router-agnostic; not gin-specific despite the old
// name it replaced). Per request:
//   - Stash moduleState on ctx so per-op bundles read the right config
//   - Extract token (if any). Absent token → anonymous request; let
//     Required/Requires at the per-op layer decide whether that's OK.
//   - Resolve (and cache) → attach Identity to ctx.
//   - On resolver failure we do NOT 401 here — that's per-op Required's
//     job. A public endpoint on the same app should stay accessible
//     even if a bogus Authorization header comes along.
func authMiddleware(state *moduleState) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		ctx := withState(c.Request.Context(), state)

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
	return nexus.Use(requiredMiddleware())
}

// requiredMiddleware builds the "authenticated identity present" gate as a
// raw middleware.Middleware. Required() wraps it for per-op attachment; the
// deny-by-default path (Authorization.Default = Authenticated()) supplies it
// to the framework as the default EndpointGate.
func requiredMiddleware() middleware.Middleware {
	return builtin("auth:required",
		"Requires an authenticated identity on ctx",
		func(rc *middleware.RequestCtx, next middleware.Next) error {
			if _, ok := IdentityFrom(rc.Context); !ok {
				return rejectAuth(rc, ErrUnauthenticated)
			}
			return next(rc)
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

// rejectAuth renders an auth denial uniformly across transports: it emits
// the auth.reject trace event, then hands off to the module's ErrorHandler
// (Config.OnError, or the default), which owns the per-transport response.
// Status follows the sentinel: ErrForbidden → 403, otherwise 401.
func rejectAuth(rc *middleware.RequestCtx, err error) error {
	eh := errorHandlerFrom(rc.Context)
	if err == ErrForbidden {
		emitReject(rc.Context, "forbidden", http.StatusForbidden, err)
		return eh.Forbidden(rc, err)
	}
	emitReject(rc.Context, "unauthenticated", http.StatusUnauthorized, err)
	return eh.Unauthenticated(rc, err)
}

// errorHandlerFrom returns the module's ErrorHandler from ctx, or the
// default when auth.Module isn't wired (so unit tests that skip the global
// middleware still render a sensible denial).
func errorHandlerFrom(ctx context.Context) ErrorHandler {
	if s, ok := stateFrom(ctx); ok && s.errorHandler != nil {
		return s.errorHandler
	}
	return defaultErrorHandler{}
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
