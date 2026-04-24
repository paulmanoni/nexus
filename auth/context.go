package auth

import "context"

// ctxKey is private so only this package can stash/read values. Two
// separate keys: one for the Identity itself, one for the moduleState
// (so Required / Requires bundles can read custom PermissionFn without
// a package-level singleton).
type ctxKey int

const (
	ctxIdentity ctxKey = iota
	ctxState
)

// WithIdentity returns a new context with the Identity attached. The
// global middleware calls this after a successful resolve; tests and
// custom transports can call it directly.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, ctxIdentity, id)
}

// IdentityFrom returns the Identity on ctx, if any. Returns (nil, false)
// for anonymous requests — Required() is what turns that into a 401.
func IdentityFrom(ctx context.Context) (*Identity, bool) {
	if ctx == nil {
		return nil, false
	}
	id, ok := ctx.Value(ctxIdentity).(*Identity)
	if !ok || id == nil {
		return nil, false
	}
	return id, true
}

// User is the typed convenience accessor: pulls the Identity from ctx
// and type-asserts Extra to T. Returns (zero, false) if either step
// fails — a single check at the top of a resolver suffices.
//
//	user, ok := auth.User[MyUser](ctx)
//	if !ok { return nil, fmt.Errorf("no user") }
func User[T any](ctx context.Context) (*T, bool) {
	id, ok := IdentityFrom(ctx)
	if !ok || id.Extra == nil {
		return nil, false
	}
	u, ok := id.Extra.(*T)
	if ok {
		return u, true
	}
	// Value-typed Extra — dereference into a pointer so callers always
	// get the same shape whether they stored a pointer or a value.
	if v, vok := id.Extra.(T); vok {
		return &v, true
	}
	return nil, false
}

// withState stashes the moduleState for later read by per-op bundles.
// Internal — callers never construct a moduleState themselves.
func withState(ctx context.Context, s *moduleState) context.Context {
	return context.WithValue(ctx, ctxState, s)
}

// stateFrom returns the moduleState installed by the global middleware.
// (nil, false) when auth.Module isn't wired — in that case Required()
// falls back to defaults so unit tests that skip the global middleware
// still behave sensibly.
func stateFrom(ctx context.Context) (*moduleState, bool) {
	if ctx == nil {
		return nil, false
	}
	s, ok := ctx.Value(ctxState).(*moduleState)
	return s, ok && s != nil
}