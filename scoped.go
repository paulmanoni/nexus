package nexus

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/paulmanoni/nexus/di"
	"github.com/paulmanoni/nexus/httpx"
)

// Compute is the per-request derivation behind a Scoped value.
type Compute[T any] func(context.Context) (T, error)

// NewScoped declares a REQUEST-SCOPED derived value: a fact computed from
// the request (identity + DB, tenant state, a feature evaluation) at most
// once per request, on first ask, and shared by every handler, service and
// prop that asks after. The ctor's params are DI-injected; its single
// return is the Compute:
//
//	var delegatedScope = nexus.NewScoped[Scope](func(svc *services.UserMgmtService) nexus.Compute[Scope] {
//	    return func(ctx context.Context) (Scope, error) {
//	        id, restricted := svc.DelegatedHrScope(ctx)
//	        return Scope{EmployerID: id, Restricted: restricted}, nil
//	    }
//	})
//
//	nexus.Boot(delegatedScope, ...)          // the handle is an Option
//
//	scope, err := delegatedScope.Get(ctx)    // anywhere, any transport
//
// Semantics — deliberately narrow:
//   - Lazy: a request whose handlers never Get pays only the (pooled)
//     store install; apps with no Scoped registered pay nothing at all.
//   - Once per request: concurrent Gets (GraphQL resolvers) share one
//     compute; the ERROR memoizes too, so a failed derivation is one
//     consistent answer for the whole request, not N retries.
//   - Per-request only: no TTL, no cross-request cache, no invalidation.
//     A fact that tolerates staleness belongs on the identity (resolve-
//     time enrichment rides the auth cache); a fact that outlives requests
//     belongs in extension/cache. One lifetime keeps the model exact.
//
// The handle also AUTO-PROVIDES itself into the DI graph, so a handler can
// declare the fact it reads as an ordinary dep instead of touching the
// package var: `func NewUsersPage(ctx context.Context, scope *nexus.Scoped[Scope], ...)`.
// Distinct fact types are distinct DI slots (*Scoped[A] and *Scoped[B]
// coexist); TWO handles of the SAME T in one app trip the container's
// duplicate-provider boot error — mark the extras .NoProvide(), or give
// each fact its own named type.
//
// A handle binds to ONE app at a time (the package-level-var + single-app
// pattern; a later boot rebinds it). Do not Get the same handle from
// inside its own Compute — that self-wait deadlocks the slot. In unit
// tests, inject a value with nexus.WithScopedValue instead of booting.
// (T is spelled explicitly — the ctor is `any` so Go cannot infer it.)
func NewScoped[T any](ctor any) *Scoped[T] {
	s := &Scoped[T]{idx: -1}
	s.invokeOpt, s.ctorErr = s.buildOption(ctor)
	return s
}

// Scoped is the typed handle NewScoped returns — the only door to the
// value, so a fact nobody registered cannot be asked for by accident.
type Scoped[T any] struct {
	idx       int // slot in the app's per-request store; -1 until bound
	compute   Compute[T]
	invokeOpt di.Option
	ctorErr   error
	noProvide bool
}

// NoProvide disables the handle's DI auto-provide. Needed only when ONE app
// registers TWO handles of the same T — the DI container is type-addressed,
// so the second *Scoped[T] provider is a boot error ("provided more than
// once"); mark all but one NoProvide, or better, give each fact its own
// named type. Chainable at var init:
//
//	var apiScope = nexus.NewScoped[Scope](...).NoProvide()
func (s *Scoped[T]) NoProvide() *Scoped[T] {
	s.noProvide = true
	return s
}

func (s *Scoped[T]) nexusOption() di.Option {
	if s.ctorErr != nil {
		return di.Error(s.ctorErr)
	}
	if s.noProvide {
		return s.invokeOpt
	}
	// The handle auto-provides itself, so a handler can DECLARE the fact it
	// reads as an ordinary dep instead of reaching for the package var:
	//
	//	func NewUsersPage(ctx context.Context, scope *nexus.Scoped[Scope], ...)
	//
	// Providers are lazy singletons — an app where nothing injects the
	// handle never runs this constructor, so the request path is untouched
	// either way.
	return di.Options(
		di.Provide(func() *Scoped[T] { return s }),
		s.invokeOpt,
	)
}

// buildOption reflects the ctor (func(deps...) Compute[T]) into a di.Invoke
// that resolves the deps at boot, binds the compute, and registers the
// handle's slot on the app.
func (s *Scoped[T]) buildOption(ctor any) (di.Option, error) {
	cv := reflect.ValueOf(ctor)
	want := reflect.TypeOf((*Compute[T])(nil)).Elem()
	if !cv.IsValid() || cv.Kind() != reflect.Func {
		return nil, fmt.Errorf("nexus: NewScoped ctor must be a func returning %s, got %T", want, ctor)
	}
	ct := cv.Type()
	if ct.NumOut() != 1 || ct.Out(0) != want {
		return nil, fmt.Errorf("nexus: NewScoped ctor must return exactly %s, got %s", want, ct)
	}
	in := make([]reflect.Type, 0, ct.NumIn()+1)
	in = append(in, reflect.TypeOf((*App)(nil)))
	for i := 0; i < ct.NumIn(); i++ {
		in = append(in, ct.In(i))
	}
	invoke := reflect.MakeFunc(reflect.FuncOf(in, nil, false), func(args []reflect.Value) []reflect.Value {
		app := args[0].Interface().(*App)
		out := cv.Call(args[1:])
		s.compute = out[0].Interface().(Compute[T])
		s.idx = app.registerScoped(func(ctx context.Context) (any, error) { return s.compute(ctx) })
		return nil
	})
	return di.Invoke(invoke.Interface()), nil
}

// Get returns the request's value, computing it on the first ask. Callable
// from any depth — the store rides the request context every transport
// already carries.
func (s *Scoped[T]) Get(ctx context.Context) (T, error) {
	var zero T
	st, _ := ctx.Value(scopedCtxKey{}).(*scopedStore)
	if st == nil {
		return zero, fmt.Errorf("nexus: Scoped.Get outside a request-scoped context — register the handle with the app, or use nexus.WithScopedValue in tests")
	}
	if st.overrides != nil {
		if e, ok := st.overrides[s]; ok {
			if e.err != nil {
				return zero, e.err
			}
			return e.val.(T), nil
		}
	}
	if s.idx < 0 || s.idx >= len(st.slots) {
		return zero, fmt.Errorf("nexus: Scoped handle is not registered with this app")
	}
	sl := &st.slots[s.idx]
	sl.mu.Lock()
	if !sl.done {
		sl.val, sl.err = st.computes[s.idx](ctx)
		sl.done = true
	}
	val, err := sl.val, sl.err
	sl.mu.Unlock()
	if err != nil {
		return zero, err
	}
	if val == nil {
		return zero, nil
	}
	return val.(T), nil
}

// WithScopedValue pre-fills a handle's value on a context — the unit-test
// door, so a handler that reads a Scoped fact is testable without booting
// an app or touching the real compute. Copy-on-write: the returned context
// carries a fresh store layered over any existing one, so pre-filling
// never races a concurrent Get on the original.
func WithScopedValue[T any](ctx context.Context, s *Scoped[T], val T) context.Context {
	parent, _ := ctx.Value(scopedCtxKey{}).(*scopedStore)
	st := &scopedStore{overrides: map[any]scopedOverride{}}
	if parent != nil {
		st.slots = parent.slots
		st.computes = parent.computes
		for k, v := range parent.overrides {
			st.overrides[k] = v
		}
	}
	st.overrides[s] = scopedOverride{val: val}
	return context.WithValue(ctx, scopedCtxKey{}, st)
}

type scopedCtxKey struct{}

type scopedOverride struct {
	val any
	err error
}

type scopedSlot struct {
	mu   sync.Mutex
	done bool
	val  any
	err  error
}

// scopedStore is the per-request memo: one slot per registered handle,
// index-addressed (no map, no per-Get allocation). Deliberately NOT pooled:
// a handler goroutine that outlives the response while holding the request
// context would otherwise Get a recycled store and read another request's
// values — two small allocations per request (only in apps that registered
// a Scoped) buy that impossibility.
type scopedStore struct {
	slots     []scopedSlot
	computes  []func(context.Context) (any, error)
	overrides map[any]scopedOverride
}

// registerScoped assigns the handle its slot and, on the first handle,
// installs the store middleware — apps with no Scoped values never see it.
func (a *App) registerScoped(compute func(context.Context) (any, error)) int {
	a.scopedMu.Lock()
	defer a.scopedMu.Unlock()
	a.scopedComputes = append(a.scopedComputes, compute)
	if len(a.scopedComputes) == 1 {
		a.engine.Use(a.scopedMiddleware())
	}
	return len(a.scopedComputes) - 1
}

func (a *App) scopedMiddleware() httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		st := &scopedStore{
			slots:    make([]scopedSlot, len(a.scopedComputes)),
			computes: a.scopedComputes,
		}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), scopedCtxKey{}, st))
		c.Next()
	}
}
