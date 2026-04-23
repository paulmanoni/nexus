package ratelimit

import (
	"context"
	"fmt"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/graph"
	"github.com/paulmanoni/nexus/middleware"
)

// clientIPCtxKey is the canonical context-value key transports use to
// stash the caller's IP for downstream middleware (rate-limit et al.)
// to read. Lives in ratelimit because this is where the primary reader
// sits; nexus exposes friendly helpers that thread through this key.
type clientIPCtxKey struct{}

// WithClientIP returns ctx carrying ip. Transports (gin REST handler,
// gql adapter, WS upgrade) call this so middleware that scopes buckets
// per IP can find it.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPCtxKey{}, ip)
}

// ClientIPFromCtx returns the caller's IP a transport put in ctx, or
// empty when absent.
func ClientIPFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(clientIPCtxKey{}).(string); ok {
		return v
	}
	return ""
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
	return middleware.Middleware{
		Name:        "rate-limit",
		Description: desc,
		Kind:        middleware.KindBuiltin,
		Gin:         ginEnforcer(store, key, limit),
		Graph:       graphEnforcer(store, key, limit),
	}
}

// ginEnforcer is GinMiddleware with PerIP-aware scope selection — we
// respect Limit.PerIP to bucket per caller IP or share a single global
// counter. Denial aborts with 429 + Retry-After header.
func ginEnforcer(store Store, key string, limit Limit) gin.HandlerFunc {
	scopeFn := func(c *gin.Context) string {
		if limit.PerIP {
			return c.ClientIP()
		}
		return ""
	}
	return GinMiddleware(store, key, scopeFn)
}

// graphEnforcer returns a graph.FieldMiddleware that runs before the
// resolver. Denial bubbles up as a GraphQL error describing the
// retry-after — clients see a coherent "rate limit exceeded" message.
func graphEnforcer(store Store, key string, limit Limit) graph.FieldMiddleware {
	return func(next graph.FieldResolveFn) graph.FieldResolveFn {
		return func(p graph.ResolveParams) (any, error) {
			scope := ""
			if limit.PerIP {
				scope = ClientIPFromCtx(p.Context)
			}
			ok, retry := store.Allow(p.Context, key, scope)
			if !ok {
				return nil, fmt.Errorf("rate limit exceeded — retry after %s", retry.Round(10_000_000))
			}
			return next(p)
		}
	}
}

