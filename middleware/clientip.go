package middleware

import "context"

// clientIPKey is the canonical context key transports use to stash the
// caller's IP, so transport-neutral middleware (rate limiting et al.) can
// read it via RequestCtx.ClientIP regardless of wire protocol. It lives here
// — the neutral middleware package — so every carrier and the ratelimit
// extension share one key without an import cycle. ratelimit.WithClientIP /
// ClientIPFromCtx delegate to these.
type clientIPKey struct{}

// WithClientIP returns ctx carrying ip. Transports (the gin REST handler, the
// GraphQL adapter, the WS upgrade) call this so per-IP middleware can find it.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

// ClientIPFromCtx returns the IP a transport stashed via WithClientIP, or
// empty when absent.
func ClientIPFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(clientIPKey{}).(string); ok {
		return v
	}
	return ""
}
