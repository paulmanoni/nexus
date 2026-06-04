package ratelimit

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/paulmanoni/nexus/middleware"
)

// WithClientIP returns ctx carrying ip. Transports (gin REST handler,
// gql adapter, WS upgrade) call this so middleware that scopes buckets
// per IP can find it.
//
// Delegates to middleware.WithClientIP — the canonical key now lives in the
// neutral middleware package so RequestCtx-backed carriers and this extension
// share one key without an import cycle. These wrappers stay for back-compat.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return middleware.WithClientIP(ctx, ip)
}

// ClientIPFromCtx returns the caller's IP a transport put in ctx, or
// empty when absent.
func ClientIPFromCtx(ctx context.Context) string {
	return middleware.ClientIPFromCtx(ctx)
}

// NewMiddleware returns a transport-agnostic middleware bundle that
// enforces rate limits against store under key. The same bundle can be
// attached to any transport via nexus.Use: gin-based enforcement for
// REST + WS upgrades, graph.FieldMiddleware for GraphQL resolvers.
//
// The declared Limit is registered with the store at middleware-create
// time so the dashboard can show it as a baseline; operators can tune
// the effective limit live via the Rate limits tab.
//
//	rl := ratelimit.NewMiddleware(
//	    store,
//	    "adverts.createAdvert",
//	    ratelimit.Limit{RPM: 30, Burst: 5},
//	)
//	fx.Provide(
//	    nexus.AsMutation(NewCreateAdvert, nexus.Use(rl)),
//	    nexus.AsRest("POST", "/quick", NewQuick, nexus.Use(rl)),
//	)
func NewMiddleware(store Store, key string, limit Limit) middleware.Middleware {
	store.Declare(key, limit)
	desc := fmt.Sprintf("%d rpm, burst %d", limit.RPM, limit.EffectiveBurst())
	if limit.PerIP {
		desc += ", per-IP"
	}

	// One implementation across every transport (redesign §1.2 / §3): the
	// caller IP comes from rc.ClientIP() (no more c.ClientIP vs
	// ClientIPFromCtx split), and denial sets the Retry-After header — a
	// no-op on GraphQL — then rejects. FromHandler generates the per-
	// transport realizations the bundle still carries.
	mw := middleware.FromHandler(middleware.NewFunc("rate-limit", middleware.AllTransports,
		func(rc *middleware.RequestCtx, next middleware.Next) error {
			scope := ""
			if limit.PerIP {
				scope = rc.ClientIP()
			}
			ok, retry := store.Allow(rc.Context, key, scope)
			if !ok {
				secs := int(retry.Round(time.Second) / time.Second)
				if secs < 1 {
					secs = 1
				}
				rc.SetHeader("Retry-After", strconv.Itoa(secs))
				return rc.Reject(http.StatusTooManyRequests,
					fmt.Errorf("rate limit exceeded — retry after %s", retry.Round(10_000_000)))
			}
			return next(rc)
		}))
	mw.Description = desc
	mw.Kind = middleware.KindBuiltin
	return mw
}
