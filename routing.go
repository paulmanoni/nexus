package nexus

import (
	"context"
	"hash/fnv"
)

// routeKeyContextKey is the typed key used to stash a routing key on
// the call context. Unexported so callers must go through WithRouteKey
// / routeKeyFromContext — keeps the internal representation private.
type routeKeyContextKey struct{}

// WithRouteKey decorates ctx with a routing key. When the call lands
// on a multi-replica RemoteCaller, the key is hashed to a stable
// replica index so calls with the same key land on the same replica.
// This is the load-bearing primitive for stateful workloads — WS
// hubs, per-user caches, idempotency stores — that need affinity
// across a split deployment.
//
// Empty key is treated as no routing — falls back to round-robin.
//
// Compose with the existing call site: a generated client method or
// hand-written caller wraps ctx before calling the RemoteCaller:
//
//	ctx = nexus.WithRouteKey(ctx, args.UserID)
//	return client.Call(ctx, "POST", "/messages", args, &out)
//
// When the chosen replica is currently ejected (recent failure), the
// caller falls back to round-robin so the call still has a chance to
// land — affinity is best-effort, not a hard guarantee.
func WithRouteKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, routeKeyContextKey{}, key)
}

// routeKeyFromContext returns the routing key set by WithRouteKey, or
// "" when none was set. RemoteCaller.pickFor consults this on every
// call.
func routeKeyFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(routeKeyContextKey{}).(string); ok {
		return v
	}
	return ""
}

// hashRouteKey reduces a routing key to a non-negative index modulo
// the replica count. FNV-1a is fast and good enough for distribution
// across a handful of replicas — no crypto requirement, just
// deterministic mapping.
func hashRouteKey(key string, n int) int {
	if n <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(n))
}
