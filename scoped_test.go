package nexus

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// nexus.NewScoped: request-scoped derived values — computed at most once
// per request, shared across handlers/services/props, error memoized,
// injectable in unit tests.

type scopeFact struct {
	N int `json:"n"`
}

func TestScopedMemoizesPerRequest(t *testing.T) {
	var computes atomic.Int32
	fact := NewScoped[scopeFact](func() Compute[scopeFact] {
		return func(ctx context.Context) (scopeFact, error) {
			return scopeFact{N: int(computes.Add(1))}, nil
		}
	})
	app, stop, err := InProcess(Config{},
		fact,
		AsRest("GET", "/twice", func(ctx context.Context) (*scopeFact, error) {
			a, err := fact.Get(ctx)
			if err != nil {
				return nil, err
			}
			b, err := fact.Get(ctx) // second ask, same request → same value
			if err != nil {
				return nil, err
			}
			if a != b {
				return nil, errors.New("memo broke within one request")
			}
			return &a, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	for i := 1; i <= 3; i++ {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest("GET", "/twice", nil))
		if w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		// One compute per request: request i sees N == i.
		if want := `{"n":` + string(rune('0'+i)) + `}`; !strings.Contains(w.Body.String(), want) {
			t.Fatalf("request %d body = %s", i, w.Body.String())
		}
	}
	if computes.Load() != 3 {
		t.Fatalf("computes = %d, want 3 (once per request)", computes.Load())
	}
}

func TestScopedConcurrentSingleflight(t *testing.T) {
	var computes atomic.Int32
	fact := NewScoped[scopeFact](func() Compute[scopeFact] {
		return func(ctx context.Context) (scopeFact, error) {
			return scopeFact{N: int(computes.Add(1))}, nil
		}
	})
	app, stop, err := InProcess(Config{},
		fact,
		AsRest("GET", "/fan", func(ctx context.Context) (*scopeFact, error) {
			var wg sync.WaitGroup
			results := make([]scopeFact, 8)
			for i := range results {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					results[i], _ = fact.Get(ctx)
				}(i)
			}
			wg.Wait()
			for _, r := range results {
				if r != results[0] {
					return nil, errors.New("concurrent gets diverged")
				}
			}
			return &results[0], nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/fan", nil))
	if w.Code != 200 || computes.Load() != 1 {
		t.Fatalf("status=%d computes=%d body=%s", w.Code, computes.Load(), w.Body.String())
	}
}

func TestScopedErrorMemoized(t *testing.T) {
	var computes atomic.Int32
	boom := errors.New("derivation failed")
	fact := NewScoped[scopeFact](func() Compute[scopeFact] {
		return func(ctx context.Context) (scopeFact, error) {
			computes.Add(1)
			return scopeFact{}, boom
		}
	})
	app, stop, err := InProcess(Config{},
		fact,
		AsRest("GET", "/err", func(ctx context.Context) (*scopeFact, error) {
			if _, err := fact.Get(ctx); err == nil {
				return nil, errors.New("want error")
			}
			if _, err := fact.Get(ctx); !errors.Is(err, boom) {
				return nil, errors.New("second get must return the memoized error")
			}
			return &scopeFact{N: 1}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/err", nil))
	if w.Code != 200 || computes.Load() != 1 {
		t.Fatalf("status=%d computes=%d body=%s", w.Code, computes.Load(), w.Body.String())
	}
}

// Two handles of the SAME Go type coexist — the handle is the key, not
// the type.
func TestScopedTwoHandlesSameType(t *testing.T) {
	a := NewScoped[scopeFact](func() Compute[scopeFact] {
		return func(ctx context.Context) (scopeFact, error) { return scopeFact{N: 1}, nil }
	})
	b := NewScoped[scopeFact](func() Compute[scopeFact] {
		return func(ctx context.Context) (scopeFact, error) { return scopeFact{N: 2}, nil }
	}).NoProvide()
	app, stop, err := InProcess(Config{},
		a, b,
		AsRest("GET", "/pair", func(ctx context.Context) (*scopeFact, error) {
			av, _ := a.Get(ctx)
			bv, _ := b.Get(ctx)
			return &scopeFact{N: av.N*10 + bv.N}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/pair", nil))
	if !strings.Contains(w.Body.String(), `{"n":12}`) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

func TestScopedGraphQLContext(t *testing.T) {
	fact := NewScoped[scopeFact](func() Compute[scopeFact] {
		return func(ctx context.Context) (scopeFact, error) { return scopeFact{N: 9}, nil }
	})
	app, stop, err := InProcess(Config{},
		fact,
		AsQuery(func(ctx context.Context) (*scopeFact, error) {
			v, err := fact.Get(ctx)
			return &v, err
		}, Op("scopeProbe")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	if got := postGraphQL(t, app, `{ scopeProbe { n } }`); !strings.Contains(got, `"n":9`) {
		t.Fatalf("gql = %s", got)
	}
}

func TestScopedTestInjection(t *testing.T) {
	fact := NewScoped[scopeFact](func() Compute[scopeFact] {
		return func(ctx context.Context) (scopeFact, error) {
			return scopeFact{}, errors.New("real compute must not run")
		}
	})
	ctx := WithScopedValue(context.Background(), fact, scopeFact{N: 42})
	v, err := fact.Get(ctx)
	if err != nil || v.N != 42 {
		t.Fatalf("injected = %v, %v", v, err)
	}

	// Outside any store: a descriptive error, not a panic.
	if _, err := fact.Get(context.Background()); err == nil || !strings.Contains(err.Error(), "WithScopedValue") {
		t.Fatalf("bare-context Get = %v", err)
	}
}

func TestScopedBadCtorFailsBoot(t *testing.T) {
	bad := NewScoped[scopeFact](func() int { return 0 })
	_, stop, err := InProcess(Config{}, bad)
	if stop != nil {
		defer func() { _ = stop(context.Background()) }()
	}
	if err == nil || !strings.Contains(err.Error(), "NewScoped ctor must return") {
		t.Fatalf("want ctor shape error, got %v", err)
	}
}

// The store middleware installs only when a Scoped is registered; Get on
// an app without one reports the miss instead of computing garbage.
func BenchmarkScopedGet(b *testing.B) {
	fact := NewScoped[scopeFact](func() Compute[scopeFact] {
		return func(ctx context.Context) (scopeFact, error) { return scopeFact{N: 7}, nil }
	})
	app, stop, err := InProcess(Config{},
		fact,
		AsRest("GET", "/s", func(ctx context.Context) (*scopeFact, error) {
			v, err := fact.Get(ctx)
			return &v, err
		}),
	)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest("GET", "/s", nil))
		if w.Code != 200 {
			b.Fatal(w.Code)
		}
	}
}

type scopeFactB struct{ M int }

// The handle auto-provides itself: a handler declares the fact it reads as
// an ordinary DI dep. Distinct fact types are distinct DI slots.
func TestScopedInjectable(t *testing.T) {
	fa := NewScoped[scopeFact](func() Compute[scopeFact] {
		return func(ctx context.Context) (scopeFact, error) { return scopeFact{N: 3}, nil }
	})
	fb := NewScoped[scopeFactB](func() Compute[scopeFactB] {
		return func(ctx context.Context) (scopeFactB, error) { return scopeFactB{M: 4}, nil }
	})
	app, stop, err := InProcess(Config{},
		fa, fb,
		AsRest("GET", "/inj", func(a *Scoped[scopeFact], b *Scoped[scopeFactB], ctx context.Context) (*scopeFact, error) {
			av, err := a.Get(ctx)
			if err != nil {
				return nil, err
			}
			bv, err := b.Get(ctx)
			if err != nil {
				return nil, err
			}
			return &scopeFact{N: av.N*10 + bv.M}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/inj", nil))
	if !strings.Contains(w.Body.String(), `{"n":34}`) {
		t.Fatalf("body = %s", w.Body.String())
	}
}

// Two same-T handles without NoProvide: the container's duplicate-provider
// check fails the boot, naming the type.
func TestScopedDuplicateTypeBootError(t *testing.T) {
	a := NewScoped[scopeFact](func() Compute[scopeFact] {
		return func(ctx context.Context) (scopeFact, error) { return scopeFact{}, nil }
	})
	b := NewScoped[scopeFact](func() Compute[scopeFact] {
		return func(ctx context.Context) (scopeFact, error) { return scopeFact{}, nil }
	})
	_, stop, err := InProcess(Config{}, a, b)
	if stop != nil {
		defer func() { _ = stop(context.Background()) }()
	}
	if err == nil || !strings.Contains(err.Error(), "provided more than once") {
		t.Fatalf("want duplicate-provider boot error, got %v", err)
	}
}
