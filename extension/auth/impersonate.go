package auth

import (
	"context"
	"reflect"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/extension/session"
)

// Impersonation: the credential stays the real user's — revoking them ends
// it — and also names the user they act as. Requests see that user, with
// Actor set to the real one.

const sessionActingKey = "_auth_acting_as"

// stateKey is the App.Value slot holding the auth module's state.
type stateKey struct{}

// Impersonate makes the rest of this session (or this bearer token) act
// as targetID. The signed-in user needs [auth.impersonation] permission,
// may not already be impersonating, and may not take on a user holding a
// permission they lack.
func Impersonate(ctx context.Context, targetID string) error {
	st, err := configState(ctx)
	if err != nil {
		return err
	}
	me := Current(ctx)
	if me == nil {
		return ErrUnauthenticated
	}
	if me.Actor != nil {
		return nexus.Err(nexus.Conflict, "already impersonating — stop first")
	}
	if !checkPermissions(ctx, me, []string{st.config.settings.impersonation.Permission}) {
		return nexus.Err(nexus.Forbidden, "you may not impersonate users")
	}
	target, err := st.load(ctx, targetID)
	if err != nil {
		return err
	}
	if target == nil || target.ID == me.ID {
		return nexus.Err(nexus.NotFound, "no such user to impersonate")
	}
	for _, perms := range [][]string{target.Perms, target.Roles, target.Scopes} {
		for _, p := range perms {
			if !me.Has(p) {
				return nexus.Errf(nexus.Forbidden, "you can't impersonate a user holding %q, which you lack", p)
			}
		}
	}
	return st.setActing(ctx, targetID)
}

// StopImpersonating returns the credential to the real user. A no-op when
// not impersonating.
func StopImpersonating(ctx context.Context) error {
	st, err := configState(ctx)
	if err != nil {
		return err
	}
	if me := Current(ctx); me == nil || me.Actor == nil {
		return nil
	}
	return st.setActing(ctx, "")
}

// setActing records on this request's credential whom it acts as.
func (st *moduleState) setActing(ctx context.Context, target string) error {
	p, ok := ctx.Value(ctxCredentialTok).(presented)
	if !ok {
		return ErrUnauthenticated
	}
	sc, ok := st.config.settings.scheme(p.scheme)
	if !ok {
		return ErrUnauthenticated
	}
	switch sc.Type {
	case SchemeSession:
		s := session.Get(ctx)
		if target == "" {
			s.Delete(sessionActingKey)
		} else {
			s.Set(sessionActingKey, target)
		}
		return nil
	case SchemeBearer, SchemeAPIKey:
		h := hashToken(p.token)
		t, err := st.config.tokens.Load(ctx, h)
		if err != nil || t == nil {
			return ErrUnauthenticated
		}
		t.Impersonating = target
		ttl := time.Duration(0)
		if !t.Expires.IsZero() {
			ttl = time.Until(t.Expires)
		}
		return st.config.tokens.Save(ctx, h, *t, ttl)
	}
	return nexus.Err(nexus.Forbidden, "a "+sc.Type+" credential can't impersonate")
}

// acting turns the real user's identity into the impersonated one, Actor
// set; when the target is gone, the real user carries on as themselves.
func (st *moduleState) acting(ctx context.Context, real *Identity, target string) (*Identity, error) {
	if target == "" {
		return real, nil
	}
	t, err := st.load(ctx, target)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return real, nil
	}
	cp := *t
	cp.Scheme, cp.Actor = real.Scheme, real
	return &cp, nil
}

type impersonateIn struct {
	UserID string `json:"user_id" validate:"required"`
}

type impersonating struct {
	OK bool `json:"ok"`
}

func impersonateEndpoint(ctx context.Context, in impersonateIn) (*impersonating, error) {
	return &impersonating{true}, Impersonate(ctx, in.UserID)
}

func stopImpersonatingEndpoint(ctx context.Context, _ struct{}) (*impersonating, error) {
	return &impersonating{true}, StopImpersonating(ctx)
}

// --- policies ------------------------------------------------------------

type policyKey struct{ t reflect.Type }

type policyFunc func(ctx context.Context, me *Identity, perm string, obj any) bool

// Policy registers a per-object rule for objects of type T, which Check
// and Allowed apply after the permission rule:
//
//	var OrderPolicy = auth.Policy(func(ctx context.Context, me *auth.Identity, perm string, o *Order) bool {
//	    return o.CustomerID == me.ID || me.Has("orders.*")
//	})
//
//	nexus.Boot(OrderPolicy, …)
//	if err := auth.Check(ctx, "orders.cancel", order); err != nil { return nil, err }
func Policy[T any](rule func(ctx context.Context, me *Identity, perm string, obj T) bool) nexus.Option {
	t := reflect.TypeFor[T]()
	return nexus.Invoke(func(app *nexus.App) {
		app.SetValue(policyKey{t}, policyFunc(func(ctx context.Context, me *Identity, perm string, obj any) bool {
			return rule(ctx, me, perm, obj.(T))
		}))
	})
}

// Check is the per-object gate: the request needs an identity, perm (when
// not "") through the same rule as Requires, and the Policy registered for
// obj's type, if any. Unauthenticated or Forbidden otherwise.
func Check(ctx context.Context, perm string, obj any) error {
	me := Current(ctx)
	if me == nil {
		return unauthenticated(ctx)
	}
	if perm != "" && !checkPermissions(ctx, me, []string{perm}) {
		return ErrForbidden
	}
	if obj == nil {
		return nil
	}
	st, ok := stateFrom(ctx)
	if !ok || st.app == nil {
		return nil
	}
	if v, ok := st.app.Value(policyKey{reflect.TypeOf(obj)}); ok {
		if !v.(policyFunc)(ctx, me, perm, obj) {
			return ErrForbidden
		}
	}
	return nil
}

// Allowed is Check as a bool, for UI toggles.
func Allowed(ctx context.Context, perm string, obj any) bool { return Check(ctx, perm, obj) == nil }

// --- jobs ----------------------------------------------------------------

func init() {
	// A job runs as the user who enqueued it: loaded through Users now, so
	// a changed role applies; a deleted account stops the job.
	nexus.RegisterIdentityRestorer(func(ctx context.Context, app *nexus.App, id string) (context.Context, error) {
		v, ok := app.Value(stateKey{})
		if !ok {
			return nil, nil
		}
		st := v.(*moduleState)
		ctx = withState(ctx, st)
		if st.config.settings == nil {
			return ctx, nil
		}
		ident, err := st.load(ctx, id)
		if err != nil {
			return nil, err
		}
		if ident == nil {
			return nil, nexus.Errf(nexus.Unauthenticated, "user %s no longer exists", id)
		}
		return WithIdentity(ctx, ident), nil
	})
}
