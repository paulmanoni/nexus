package di

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

// These tests push the container past the shapes nexus produces in practice:
// deep dependency chains, wide DAGs with shared sub-deps, large value groups,
// thousands of invokes, and lifecycle rollback at scale. Distinct types are
// synthesized with reflect.ArrayOf([n]byte) so a graph of arbitrary size can be
// built without hand-writing N named types. Run with -race.

var byteType = reflect.TypeFor[byte]()

func arrType(n int) reflect.Type { return reflect.ArrayOf(n, byteType) }

// chainCtor returns `func([n-1]byte) [n]byte`, bumping counter when it runs.
func chainCtor(n int, counter *int64) any {
	in, out := arrType(n-1), arrType(n)
	ft := reflect.FuncOf([]reflect.Type{in}, []reflect.Type{out}, false)
	return reflect.MakeFunc(ft, func([]reflect.Value) []reflect.Value {
		atomic.AddInt64(counter, 1)
		return []reflect.Value{reflect.Zero(out)}
	}).Interface()
}

// supplyBase puts the zero [0]byte into the graph so chains can bottom out.
func supplyBase() Option { return Supply(reflect.Zero(arrType(0)).Interface()) }

// populatePtr makes a *[n]byte target for Populate.
func populatePtr(n int) any { return reflect.New(arrType(n)).Interface() }

func TestStress_DeepChainResolvesOnceEach(t *testing.T) {
	const N = 400
	var calls int64
	opts := []Option{supplyBase()}
	for i := 1; i <= N; i++ {
		opts = append(opts, Provide(chainCtor(i, &calls)))
	}
	// Demand only the top — laziness must still pull the whole chain.
	opts = append(opts, Populate(populatePtr(N)))

	app := New(opts...)
	if err := app.Err(); err != nil {
		t.Fatalf("build: %v", err)
	}
	if calls != N {
		t.Fatalf("deep chain ran %d ctors, want %d (each exactly once)", calls, N)
	}
}

func TestStress_LazyNoDemandNoConstruction(t *testing.T) {
	const N = 1000
	var calls int64
	opts := []Option{supplyBase()}
	for i := 1; i <= N; i++ {
		opts = append(opts, Provide(chainCtor(i, &calls)))
	}
	// No Populate / Invoke demanding anything.
	if err := New(opts...).Err(); err != nil {
		t.Fatalf("build: %v", err)
	}
	if calls != 0 {
		t.Fatalf("lazy violated: %d ctors ran with no demand", calls)
	}
}

// diamondCtor returns `func([i-1]byte, [i-2]byte) [i]byte` — two parents share
// deeper deps, so the shared nodes must still resolve exactly once.
func diamondCtor(i int, counter *int64) any {
	out := arrType(i)
	ft := reflect.FuncOf([]reflect.Type{arrType(i - 1), arrType(i - 2)}, []reflect.Type{out}, false)
	return reflect.MakeFunc(ft, func([]reflect.Value) []reflect.Value {
		atomic.AddInt64(counter, 1)
		return []reflect.Value{reflect.Zero(out)}
	}).Interface()
}

func TestStress_DiamondSharedDepsResolveOnce(t *testing.T) {
	const N = 300
	var calls int64
	opts := []Option{
		supplyBase(),
		Provide(chainCtor(1, &calls)), // [1]byte from [0]byte
	}
	for i := 2; i <= N; i++ {
		opts = append(opts, Provide(diamondCtor(i, &calls)))
	}
	opts = append(opts, Populate(populatePtr(N)))
	app := New(opts...)
	if err := app.Err(); err != nil {
		t.Fatalf("build: %v", err)
	}
	// ctors for levels 1..N ran once each despite each node being depended
	// on by two higher levels.
	if calls != N {
		t.Fatalf("diamond ran %d ctors, want %d", calls, N)
	}
}

type groupMember struct{ n int }

func TestStress_LargeValueGroup(t *testing.T) {
	const N = 3000
	var built int64
	opts := make([]Option, 0, N+1)
	for i := 0; i < N; i++ {
		i := i
		opts = append(opts, Provide(Annotate(func() groupMember {
			atomic.AddInt64(&built, 1)
			return groupMember{n: i}
		}, ResultTags(`group:"g"`))))
	}
	var sum, count int64
	opts = append(opts, Invoke(Annotate(func(ms []groupMember) {
		count = int64(len(ms))
		for _, m := range ms {
			sum += int64(m.n)
		}
	}, ParamTags(`group:"g"`))))

	app := New(opts...)
	if err := app.Err(); err != nil {
		t.Fatalf("build: %v", err)
	}
	if count != N || built != N {
		t.Fatalf("group: count=%d built=%d, want %d", count, built, N)
	}
	want := int64(N*(N-1)) / 2
	if sum != want {
		t.Fatalf("group sum=%d, want %d (members or ordering dropped)", sum, want)
	}
}

func TestStress_ManyInvokesPreserveOrder(t *testing.T) {
	const N = 1000
	order := make([]int, 0, N)
	opts := make([]Option, 0, N)
	for i := 0; i < N; i++ {
		i := i
		opts = append(opts, Invoke(func() { order = append(order, i) }))
	}
	if err := New(opts...).Err(); err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(order) != N {
		t.Fatalf("ran %d invokes, want %d", len(order), N)
	}
	for i := 0; i < N; i++ {
		if order[i] != i {
			t.Fatalf("invoke order broken at %d: got %d", i, order[i])
		}
	}
}

func TestStress_ErrorMidChainAbortsAndStopsDownstream(t *testing.T) {
	const N = 200
	const boomAt = 120
	boom := errors.New("mid-chain boom")
	var calls int64

	opts := []Option{supplyBase()}
	for i := 1; i <= N; i++ {
		if i == boomAt {
			// This ctor returns ([i]byte, error) with a non-nil error.
			in, out := arrType(i-1), arrType(i)
			ft := reflect.FuncOf([]reflect.Type{in}, []reflect.Type{out, errorType}, false)
			fn := reflect.MakeFunc(ft, func([]reflect.Value) []reflect.Value {
				atomic.AddInt64(&calls, 1)
				return []reflect.Value{reflect.Zero(out), reflect.ValueOf(boom)}
			}).Interface()
			opts = append(opts, Provide(fn))
			continue
		}
		opts = append(opts, Provide(chainCtor(i, &calls)))
	}
	opts = append(opts, Populate(populatePtr(N)))

	app := New(opts...)
	if !errors.Is(app.Err(), boom) {
		t.Fatalf("want boom, got %v", app.Err())
	}
	// Nodes above boomAt must never have run (resolution unwound at the error).
	if calls > boomAt {
		t.Fatalf("ran %d ctors past the failure at %d — downstream not aborted", calls, boomAt)
	}
}

func TestStress_DeepCycleDetected(t *testing.T) {
	// Build a long chain, then close it into a cycle: [N]byte depends back on
	// [1]byte's missing producer by making [1]byte depend on [N]byte.
	const N = 250
	var calls int64
	opts := make([]Option, 0, N+1)
	// level 1 depends on level N (the back-edge), levels 2..N depend on i-1.
	in1, out1 := arrType(N), arrType(1)
	ft1 := reflect.FuncOf([]reflect.Type{in1}, []reflect.Type{out1}, false)
	opts = append(opts, Provide(reflect.MakeFunc(ft1, func([]reflect.Value) []reflect.Value {
		atomic.AddInt64(&calls, 1)
		return []reflect.Value{reflect.Zero(out1)}
	}).Interface()))
	for i := 2; i <= N; i++ {
		opts = append(opts, Provide(chainCtor(i, &calls)))
	}
	opts = append(opts, Populate(populatePtr(N)))

	app := New(opts...)
	if app.Err() == nil {
		t.Fatal("expected a cycle error through the deep chain")
	}
	if !containsStr(app.Err().Error(), "cycle") {
		t.Fatalf("error should mention cycle, got %v", app.Err())
	}
}

func TestStress_LifecycleOrderAndReverseStopAtScale(t *testing.T) {
	const N = 1000
	var mu sync.Mutex
	var starts, stops []int

	app := New(Invoke(func(lc Lifecycle) {
		for i := 0; i < N; i++ {
			i := i
			lc.Append(Hook{
				OnStart: func(context.Context) error { mu.Lock(); starts = append(starts, i); mu.Unlock(); return nil },
				OnStop:  func(context.Context) error { mu.Lock(); stops = append(stops, i); mu.Unlock(); return nil },
			})
		}
	}))
	if err := app.Err(); err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(starts) != N || len(stops) != N {
		t.Fatalf("starts=%d stops=%d, want %d each", len(starts), len(stops), N)
	}
	for i := 0; i < N; i++ {
		if starts[i] != i {
			t.Fatalf("start order broken at %d: %d", i, starts[i])
		}
		if stops[i] != N-1-i {
			t.Fatalf("stop order not reversed at %d: %d", i, stops[i])
		}
	}
}

func TestStress_RollbackUnwindsExactlyStartedHooks(t *testing.T) {
	const N = 500
	const failAt = 317
	var stopped []int

	app := New(Invoke(func(lc Lifecycle) {
		for i := 0; i < N; i++ {
			i := i
			lc.Append(Hook{
				OnStart: func(context.Context) error {
					if i == failAt {
						return fmt.Errorf("start failed at %d", i)
					}
					return nil
				},
				OnStop: func(context.Context) error { stopped = append(stopped, i); return nil },
			})
		}
	}))
	if err := app.Start(context.Background()); err == nil {
		t.Fatal("expected start failure")
	}
	// Hooks 0..failAt-1 started; rollback stops them in reverse. The failing
	// hook and those after it never started, so never stop.
	if len(stopped) != failAt {
		t.Fatalf("rolled back %d hooks, want %d", len(stopped), failAt)
	}
	for i := 0; i < failAt; i++ {
		if stopped[i] != failAt-1-i {
			t.Fatalf("rollback not reverse at %d: %d", i, stopped[i])
		}
	}
}

func TestStress_SingletonSharedAcrossManyConsumers(t *testing.T) {
	const consumers = 2000
	var built int64
	opts := []Option{Provide(func() *groupMember {
		atomic.AddInt64(&built, 1)
		return &groupMember{n: 42}
	})}
	seen := make([]*groupMember, consumers)
	for i := 0; i < consumers; i++ {
		i := i
		opts = append(opts, Invoke(func(m *groupMember) { seen[i] = m }))
	}
	if err := New(opts...).Err(); err != nil {
		t.Fatalf("build: %v", err)
	}
	if built != 1 {
		t.Fatalf("singleton ctor ran %d times, want 1", built)
	}
	for i := 1; i < consumers; i++ {
		if seen[i] != seen[0] {
			t.Fatalf("consumer %d got a different instance — not a singleton", i)
		}
	}
}

func TestStress_BuiltSingletonsReadConcurrently(t *testing.T) {
	// The container builds single-threaded, but resolved values are read from
	// many goroutines afterwards (handlers). Capture one and hammer it under
	// -race to prove no write races into shared state post-build.
	var p *groupMember
	app := New(
		Provide(func() *groupMember { return &groupMember{n: 7} }),
		Populate(&p),
	)
	if err := app.Err(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				if p.n != 7 {
					panic("corrupted singleton")
				}
			}
		}()
	}
	wg.Wait()
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func BenchmarkBuildLargeGraph(b *testing.B) {
	const N = 500
	base := []Option{supplyBase()}
	var sink int64
	for i := 1; i <= N; i++ {
		base = append(base, Provide(chainCtor(i, &sink)))
	}
	base = append(base, Populate(populatePtr(N)))
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if err := New(base...).Err(); err != nil {
			b.Fatal(err)
		}
	}
}
