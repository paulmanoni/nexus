package dataloader

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// TestLoader_BatchesNCallsIntoOne is the headline contract: N thunks
// → exactly 1 fetch call regardless of how many Loads happened. This
// is what eliminates GraphQL's N+1.
func TestLoader_BatchesNCallsIntoOne(t *testing.T) {
	var calls atomic.Int32
	fetch := func(ctx context.Context, keys []int) (map[int]string, error) {
		calls.Add(1)
		out := make(map[int]string, len(keys))
		for _, k := range keys {
			out[k] = "v" + itoa(k)
		}
		return out, nil
	}
	l := New(fetch)

	// Enqueue 50 distinct keys before any thunk runs (the GraphQL
	// resolve-pass shape).
	thunks := make([]func() (interface{}, error), 50)
	for i := 0; i < 50; i++ {
		thunks[i] = l.Load(i)
	}

	// Dethunk phase. The first thunk fires the batch; the rest
	// read cached results.
	for i, thunk := range thunks {
		v, err := thunk()
		if err != nil {
			t.Fatalf("thunk %d: %v", i, err)
		}
		if v != "v"+itoa(i) {
			t.Fatalf("thunk %d: got %v", i, v)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("fetch called %d times, want 1", got)
	}
}

// TestLoader_DedupesKeys: requesting the same key 100 times still
// sends one key to the fetch function.
func TestLoader_DedupesKeys(t *testing.T) {
	var seen []int
	fetch := func(ctx context.Context, keys []int) (map[int]string, error) {
		seen = keys
		return map[int]string{1: "one"}, nil
	}
	l := New(fetch)
	thunks := make([]func() (interface{}, error), 100)
	for i := range thunks {
		thunks[i] = l.Load(1) // same key every time
	}
	for _, t := range thunks {
		t()
	}
	if len(seen) != 1 {
		t.Fatalf("got %d keys in batch, want 1: %v", len(seen), seen)
	}
}

// TestLoader_PropagatesErrorToAllThunks: one fetch failure makes
// every thunk return the same error, matching dataloader-spec.
func TestLoader_PropagatesErrorToAllThunks(t *testing.T) {
	boom := errors.New("boom")
	l := New(func(ctx context.Context, keys []int) (map[int]string, error) {
		return nil, boom
	})
	t1 := l.Load(1)
	t2 := l.Load(2)
	for i, th := range []func() (interface{}, error){t1, t2} {
		_, err := th()
		if !errors.Is(err, boom) {
			t.Fatalf("thunk %d err = %v, want boom", i, err)
		}
	}
}

// TestLoader_MissingKeyReturnsZero: a key absent from the fetch's
// returned map yields the zero V. Callers wanting "explicit null"
// use pointer types.
func TestLoader_MissingKeyReturnsZero(t *testing.T) {
	l := New(func(ctx context.Context, keys []int) (map[int]*string, error) {
		// Return a value only for key 1; key 2 is "not found".
		v := "found"
		return map[int]*string{1: &v}, nil
	})
	t1 := l.Load(1)
	t2 := l.Load(2)
	v1, _ := t1()
	v2, _ := t2()
	if v1 == nil || *v1.(*string) != "found" {
		t.Fatalf("key 1 = %#v", v1)
	}
	if v2 != (*string)(nil) {
		t.Fatalf("key 2 = %#v, want typed-nil *string", v2)
	}
}

// TestLoader_Prime: seeding the cache lets a known value skip the
// fetch.
func TestLoader_Prime(t *testing.T) {
	var fetched bool
	l := New(func(ctx context.Context, keys []int) (map[int]string, error) {
		fetched = true
		out := make(map[int]string, len(keys))
		for _, k := range keys {
			out[k] = "v"
		}
		return out, nil
	})
	l.Prime(1, "primed")
	v, err := l.Load(1)()
	if err != nil {
		t.Fatal(err)
	}
	if v != "primed" {
		t.Fatalf("got %v, want primed", v)
	}
	if fetched {
		t.Fatal("Prime should have skipped the fetch")
	}
}

// TestRegistry_SharedAcrossSiblings: two calls to Get with the same
// name in the same request return the same Loader, so two resolver
// chains for the same field type batch together.
func TestRegistry_SharedAcrossSiblings(t *testing.T) {
	reg := NewRegistry()
	ctx := WithRegistry(context.Background(), reg)

	var calls atomic.Int32
	fetch := func(ctx context.Context, keys []int) (map[int]string, error) {
		calls.Add(1)
		out := make(map[int]string, len(keys))
		for _, k := range keys {
			out[k] = "v"
		}
		return out, nil
	}

	l1 := Get[int, string](ctx, "byID", fetch)
	l2 := Get[int, string](ctx, "byID", fetch)

	if l1 != l2 {
		t.Fatal("two Get calls with same name returned different Loaders")
	}

	l1.Load(1)
	l2.Load(2)
	l1.Load(1)() // dispatches the batch

	if calls.Load() != 1 {
		t.Fatalf("fetch called %d times, want 1", calls.Load())
	}
}

// TestRegistry_NilContextFallsBackToFreshLoader: without the
// middleware, Get returns a one-shot Loader. Used so tests can call
// Get without wiring a registry. Production should always wire one.
func TestRegistry_NilContextFallsBackToFreshLoader(t *testing.T) {
	fetch := func(ctx context.Context, keys []int) (map[int]string, error) {
		return map[int]string{1: "v"}, nil
	}
	l1 := Get[int, string](context.Background(), "byID", fetch)
	l2 := Get[int, string](context.Background(), "byID", fetch)
	if l1 == l2 {
		t.Fatal("expected fresh Loader without registry")
	}
}

// TestRegistry_TypeMismatchPanics: using the same name with
// different K/V types is a programmer error and we want it loud.
func TestRegistry_TypeMismatchPanics(t *testing.T) {
	reg := NewRegistry()
	ctx := WithRegistry(context.Background(), reg)
	Get[int, string](ctx, "x", func(context.Context, []int) (map[int]string, error) { return nil, nil })

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on type mismatch")
		}
	}()
	Get[string, int](ctx, "x", func(context.Context, []string) (map[string]int, error) { return nil, nil })
}

// --- bench: scale the win ---

// BenchmarkLoader_BatchVsLoopFetch shows the win when the underlying
// fetch has a fixed per-call cost (the realistic case: a DB roundtrip
// pays setup + network + parse, not just per-row work). We simulate
// that with a small busywait per fetch call. 100 keys per request.
//
// loop:  pays the per-call cost 100 times
// batch: pays it once
func BenchmarkLoader_BatchVsLoopFetch(b *testing.B) {
	keys := make([]int, 100)
	for i := range keys {
		keys[i] = i
	}

	// Simulate a ~1µs-per-call overhead (DB driver setup, lock,
	// log, etc.) so the bench measures the win, not the loader's
	// own micro-cost.
	var sink int64
	fetch := func(ctx context.Context, ks []int) (map[int]string, error) {
		for i := 0; i < 200; i++ {
			sink ^= int64(i * 2654435761)
		}
		out := make(map[int]string, len(ks))
		for _, k := range ks {
			out[k] = "v"
		}
		return out, nil
	}

	b.Run("loop", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, k := range keys {
				_, _ = fetch(context.Background(), []int{k})
			}
		}
	})

	b.Run("batch", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			l := New(fetch)
			thunks := make([]func() (interface{}, error), len(keys))
			for j, k := range keys {
				thunks[j] = l.Load(k)
			}
			for _, t := range thunks {
				t()
			}
		}
	})
	_ = sink
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [12]byte
	n := len(buf)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		buf[n] = '-'
	}
	return string(buf[n:])
}
