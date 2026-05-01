package users

import (
	"context"
	"time"

	"github.com/paulmanoni/nexus"
)

// REST/GraphQL handler thin-wrappers. The framework's AsRest /
// AsQuery binders need handlers shaped (svc, deps..., Params[T]) →
// (R, error); these adapt the plain methods on Service to that shape.
// No business logic lives here.
//
// Handlers that *touch* the cache take *Cache as a dep — that signals
// the dependency to the framework's per-handler scan, which records
// "users-cache" against the endpoint. The dashboard then draws a
// solid per-op edge from the row to the cache resource. (Constructor-
// level deps still produce a dashed service-level edge, but per-op
// edges read more clearly on the canvas.)

func NewGet(svc *Service, cache *Cache, p nexus.Params[GetArgs]) (*User, error) {
	_ = cache // declared so the framework records the dep edge
	return svc.Get(p.Context, p.Args)
}

func NewList(svc *Service, cache *Cache, p nexus.Params[ListArgs]) ([]*User, error) {
	_ = cache
	return svc.List(p.Context, p.Args)
}

func NewSearch(svc *Service, cache *Cache, p nexus.Params[SearchArgs]) ([]*User, error) {
	_ = cache
	return svc.Search(p.Context, p.Args)
}

// NewCreate is the createUser GraphQL mutation handler. Takes *Cache
// because a real implementation would write-through; declares it for
// the dashboard's per-op resource edge regardless. Validation tags on
// CreateArgs surface in the drawer's Arguments section.
func NewCreate(svc *Service, cache *Cache, p nexus.Params[CreateArgs]) (*User, error) {
	_ = cache
	return svc.Create(p.Context, p.Args)
}

// NewBoom always panics with a nil-pointer dereference. Wired in
// module.go as GET /boom to demo the framework's stack-capture path —
// the recoveryMiddleware catches the runtime panic, attaches the
// captured debug.Stack() to a *trace.StackError, and the dashboard
// surfaces it on:
//
//   - the op-row's red error chip count (click to open drawer)
//   - the drawer's "Last error" panel with a ▸ stack disclosure
//   - the drawer's "Recent activity" rows (one per request) with the
//     same ▸ stack toggle
//   - the bottom Activity rail rows (live feed)
//
// Real handlers wouldn't ship this; it's purely a visibility demo.
// Hit it once from the drawer's tester and refresh — every panel that
// can show a stack will have it populated.
func NewBoom(svc *Service, p nexus.Params[struct{}]) (*User, error) {
	var u *User
	// Reading u.ID below dereferences nil, producing a runtime panic
	// with a stack that includes the runtime + this handler frame —
	// representative of what real production crashes look like.
	return &User{ID: u.ID}, nil
}

// NewCounter is a long-lived background worker. Its first param is
// context.Context (cancelled at fx.Stop), and it takes *Cache as a dep
// so the dashboard draws a Worker → users-cache edge alongside the
// service-level edge from User service. No-op heartbeat keeps the
// example trivial.
func NewCounter(ctx context.Context, cache *Cache) error {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			_ = cache.Len()
		}
	}
}
