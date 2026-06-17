package auth

import (
	"context"
	"reflect"
	"strconv"
)

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

// Principal lets a wrapper Extra expose the underlying domain user. Resolvers
// commonly store a richer value in Identity.Extra than the bare user — e.g. a
// struct that also carries roles/permissions for a shared "me" payload. By
// implementing Principal on that wrapper, User[T] can still reach the typed
// user inside it:
//
//	type MeData struct{ User *User; Roles []Role /* … */ }
//	func (m *MeData) Principal() any { return m.User }
//
//	// resolver: Extra: &MeData{User: u, …}
//	user, ok := auth.User[User](ctx) // unwraps MeData → *User
//
// Principal may return another Principal; User[T] unwraps repeatedly.
type Principal interface{ Principal() any }

// User is the typed convenience accessor: pulls the Identity from ctx and
// type-asserts Extra to T. If Extra isn't a T (or *T) but implements Principal,
// its Principal() is unwrapped and retried. Returns (zero, false) if every step
// fails — a single check at the top of a resolver suffices.
//
//	user, ok := auth.User[MyUser](ctx)
//	if !ok { return nil, fmt.Errorf("no user") }
func User[T any](ctx context.Context) (*T, bool) {
	id, ok := IdentityFrom(ctx)
	if !ok || id.Extra == nil {
		return nil, false
	}
	return asType[T](id.Extra)
}

// asType resolves v to *T: a direct *T, a value T (returned as a pointer so
// callers get one shape either way), or — for wrapper Extras — the result of
// unwrapping Principal(). The visited guard stops a self-referential Principal
// from looping forever.
func asType[T any](v any) (*T, bool) {
	for range 64 { // bounded unwrap depth; guards against Principal cycles
		if u, ok := v.(*T); ok {
			return u, true
		}
		if u, ok := v.(T); ok {
			return &u, true
		}
		w, ok := v.(Principal)
		if !ok || w == nil {
			return nil, false
		}
		next := w.Principal()
		if next == nil || next == v {
			return nil, false
		}
		v = next
	}
	return nil, false
}

// SubjectID is the set of types Subject / SubjectPtr can parse an
// Identity.ID into: any string-kind or integer-kind type. Identity.ID
// stays a string (it's the cache + invalidation key) — these helpers
// hand you the typed value your domain actually uses without the
// per-call-site parse + nil-pointer dance.
type SubjectID interface {
	~string |
		~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64
}

// Subject returns the authenticated identity's ID parsed into T (a string
// or integer type), and false when the request is anonymous or the ID
// doesn't parse into T.
//
//	uid, ok := auth.Subject[uint](ctx)
func Subject[T SubjectID](ctx context.Context) (T, bool) {
	var zero T
	id, ok := IdentityFrom(ctx)
	if !ok || id.ID == "" {
		return zero, false
	}
	rv := reflect.ValueOf(&zero).Elem()
	switch rv.Kind() {
	case reflect.String:
		rv.SetString(id.ID)
		return zero, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(id.ID, 10, 64)
		if err != nil {
			return zero, false
		}
		rv.SetInt(n)
		return zero, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(id.ID, 10, 64)
		if err != nil {
			return zero, false
		}
		rv.SetUint(n)
		return zero, true
	default:
		return zero, false
	}
}

// SubjectPtr is the nullable form of Subject, for audit columns and
// optional foreign keys: it returns a pointer to the typed ID, or nil
// when the request is anonymous (or the ID doesn't parse). Collapses the
// classic six-line "if identity, parse, guard, take address" prelude to
// one call:
//
//	actorID := auth.SubjectPtr[uint](ctx) // *uint, nil if anonymous
func SubjectPtr[T SubjectID](ctx context.Context) *T {
	if v, ok := Subject[T](ctx); ok {
		return &v
	}
	return nil
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