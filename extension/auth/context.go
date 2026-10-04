package auth

import (
	"context"
	"reflect"
	"strconv"

	"github.com/paulmanoni/nexus/v2"
)

type ctxKey int

const (
	ctxIdentity ctxKey = iota
	ctxState
)

// WithIdentity returns ctx carrying id — what the auth middleware does for
// every authenticated request; unit tests call it directly.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, ctxIdentity, id)
}

// Current returns the request's identity, or nil when it is anonymous —
// the same on every transport.
//
//	if me := auth.Current(ctx); me != nil { … }
func Current(ctx context.Context) *Identity {
	if ctx == nil {
		return nil
	}
	id, _ := ctx.Value(ctxIdentity).(*Identity)
	return id
}

// User returns the identity's app user (Identity.User) as a *T — whether
// it was stored as a T or a *T; false when anonymous or another type.
//
//	u, ok := auth.User[Account](ctx)
func User[T any](ctx context.Context) (*T, bool) {
	id := Current(ctx)
	if id == nil || id.User == nil {
		return nil, false
	}
	switch u := id.User.(type) {
	case *T:
		return u, u != nil
	case T:
		return &u, true
	}
	return nil, false
}

// idType is what ID parses Identity.ID into: a string or an integer type.
type idType interface {
	~string |
		~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64
}

// ID returns the request identity's ID parsed into T (a string or integer
// type); false when anonymous or the ID doesn't parse.
//
//	uid, ok := auth.ID[int64](ctx)
func ID[T idType](ctx context.Context) (T, bool) {
	var zero T
	id := Current(ctx)
	if id == nil || id.ID == "" {
		return zero, false
	}
	rv := reflect.ValueOf(&zero).Elem()
	switch rv.Kind() {
	case reflect.String:
		rv.SetString(id.ID)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(id.ID, 10, rv.Type().Bits())
		if err != nil {
			return zero, false
		}
		rv.SetInt(n)
	default:
		n, err := strconv.ParseUint(id.ID, 10, rv.Type().Bits())
		if err != nil {
			return zero, false
		}
		rv.SetUint(n)
	}
	return zero, true
}

// identityFrom is Current with an ok.
func identityFrom(ctx context.Context) (*Identity, bool) {
	id := Current(ctx)
	return id, id != nil
}

// A WebSocket connection belongs to the identity its upgrade request was
// authenticated as — what EmitToUser addresses — and its message handlers
// see that identity and this module's state on their context.
func init() {
	nexus.RegisterRequestIdentity(func(ctx context.Context) (string, bool) {
		id := Current(ctx)
		if id == nil || id.ID == "" {
			return "", false
		}
		return id.ID, true
	})
	nexus.RegisterWSCarrier(func(upgrade, conn context.Context) context.Context {
		if st, ok := stateFrom(upgrade); ok {
			conn = withState(conn, st)
		}
		if id := Current(upgrade); id != nil {
			conn = WithIdentity(conn, id)
		}
		return conn
	})
}

func withState(ctx context.Context, s *moduleState) context.Context {
	return context.WithValue(ctx, ctxState, s)
}

// stateFrom returns the module state the auth middleware put on ctx.
func stateFrom(ctx context.Context) (*moduleState, bool) {
	if ctx == nil {
		return nil, false
	}
	s, ok := ctx.Value(ctxState).(*moduleState)
	return s, ok && s != nil
}
