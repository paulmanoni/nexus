// Package ditest provides a backend-agnostic conformance suite for the di seam.
// Any di.Backend (the builtin container, the fx adapter, or a future one) must
// pass RunConformance to be a drop-in substitute. The suite pins exact expected
// values rather than comparing two backends head-to-head, so a single backend
// can be validated in isolation — and two backends passing it are, by
// construction, observably equivalent.
package ditest

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/paulmanoni/nexus/di"
)

// RunConformance exercises the behavioral contract every di backend must honor:
// provide/resolve/invoke, laziness + singleton caching, value groups, lifecycle
// ordering (append-order start, reverse-order stop), and variadic-constructor
// tolerance. Call it from each backend's test package:
//
//	func TestBuiltin(t *testing.T) { ditest.RunConformance(t, di.Builtin()) }
//	func TestFx(t *testing.T)      { ditest.RunConformance(t, fxcontainer.New()) }
func RunConformance(t *testing.T, backend di.Backend) {
	t.Helper()
	t.Run("provide_resolve_invoke", func(t *testing.T) { testProvideResolveInvoke(t, backend) })
	t.Run("lazy_and_singleton", func(t *testing.T) { testLazyAndSingleton(t, backend) })
	t.Run("value_groups", func(t *testing.T) { testValueGroups(t, backend) })
	t.Run("lifecycle_order", func(t *testing.T) { testLifecycleOrder(t, backend) })
	t.Run("variadic_constructor", func(t *testing.T) { testVariadicConstructor(t, backend) })
	t.Run("deep_chain_once_each", func(t *testing.T) { testDeepChain(t, backend) })
}

func build(t *testing.T, backend di.Backend, opts ...di.Option) di.Instance {
	t.Helper()
	inst := backend.Build(di.Collect(opts...))
	if err := inst.Err(); err != nil {
		t.Fatalf("build: %v", err)
	}
	return inst
}

type config struct{ addr string }
type pinger struct{ name string }

func testProvideResolveInvoke(t *testing.T, backend di.Backend) {
	var got *pinger
	build(t, backend,
		di.Supply(config{addr: ":8080"}),
		di.Provide(func(c config) *pinger { return &pinger{name: c.addr} }),
		di.Invoke(func(p *pinger) { got = p }),
	)
	if got == nil || got.name != ":8080" {
		t.Fatalf("invoke saw %+v; want pinger{name:\":8080\"}", got)
	}
}

func testLazyAndSingleton(t *testing.T, backend di.Backend) {
	// Provided but never demanded -> constructor must not run.
	var lazyCalls int64
	build(t, backend,
		di.Provide(func() *pinger { atomic.AddInt64(&lazyCalls, 1); return &pinger{} }),
	)
	if lazyCalls != 0 {
		t.Fatalf("lazy: ctor ran %d times with no demand; want 0", lazyCalls)
	}

	// Demanded by two invokes -> constructor runs exactly once (singleton),
	// and both invokes observe the same instance.
	var calls int64
	var a, b *pinger
	build(t, backend,
		di.Provide(func() *pinger { atomic.AddInt64(&calls, 1); return &pinger{} }),
		di.Invoke(func(p *pinger) { a = p }),
		di.Invoke(func(p *pinger) { b = p }),
	)
	if calls != 1 {
		t.Fatalf("singleton: ctor ran %d times; want 1", calls)
	}
	if a == nil || a != b {
		t.Fatalf("singleton: invokes saw different instances %p vs %p", a, b)
	}
}

type member struct{ n int }

func testValueGroups(t *testing.T, backend di.Backend) {
	const groupN = 50
	var count, sum int
	opts := make([]di.Option, 0, groupN+1)
	for i := 0; i < groupN; i++ {
		i := i
		opts = append(opts, di.Provide(di.Annotate(
			func() member { return member{n: i} },
			di.ResultTags(`group:"g"`),
		)))
	}
	opts = append(opts, di.Invoke(di.Annotate(func(ms []member) {
		count = len(ms)
		for _, m := range ms {
			sum += m.n
		}
	}, di.ParamTags(`group:"g"`))))

	build(t, backend, opts...)

	if count != groupN {
		t.Fatalf("group count = %d; want %d", count, groupN)
	}
	// Order may differ between backends; sum is order-independent.
	if want := groupN * (groupN - 1) / 2; sum != want {
		t.Fatalf("group sum = %d; want %d", sum, want)
	}
}

func testLifecycleOrder(t *testing.T, backend di.Backend) {
	const hooks = 20
	var mu sync.Mutex
	var starts, stops []int

	inst := build(t, backend, di.Invoke(func(lc di.Lifecycle) {
		for i := 0; i < hooks; i++ {
			i := i
			lc.Append(di.Hook{
				OnStart: func(context.Context) error { mu.Lock(); starts = append(starts, i); mu.Unlock(); return nil },
				OnStop:  func(context.Context) error { mu.Lock(); stops = append(stops, i); mu.Unlock(); return nil },
			})
		}
	}))
	if err := inst.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := inst.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if len(starts) != hooks || len(stops) != hooks {
		t.Fatalf("starts=%d stops=%d; want %d each", len(starts), len(stops), hooks)
	}
	for i := 0; i < hooks; i++ {
		if starts[i] != i {
			t.Fatalf("start order broken at %d: got %d (want append order)", i, starts[i])
		}
		if stops[i] != hooks-1-i {
			t.Fatalf("stop order broken at %d: got %d (want reverse order)", i, stops[i])
		}
	}
}

func testVariadicConstructor(t *testing.T, backend di.Backend) {
	// An unannotated variadic param (zap.NewExample(...Option)-style) must be
	// tolerated: the backend supplies an empty slice rather than demanding a
	// provider for []opt.
	type opt func()
	type svc struct{ ok bool }
	var got *svc
	build(t, backend,
		di.Provide(func(opts ...opt) *svc { return &svc{ok: true} }),
		di.Invoke(func(s *svc) { got = s }),
	)
	if got == nil || !got.ok {
		t.Fatalf("variadic ctor did not run / produced %+v", got)
	}
}

var byteType = reflect.TypeFor[byte]()

func arrType(n int) reflect.Type { return reflect.ArrayOf(n, byteType) }

// chainCtor[n] takes [n-1]byte and returns [n]byte, so providers form a strict
// dependency chain only resolvable in order.
func chainCtor(n int, counter *int64) any {
	in, out := arrType(n-1), arrType(n)
	ft := reflect.FuncOf([]reflect.Type{in}, []reflect.Type{out}, false)
	return reflect.MakeFunc(ft, func([]reflect.Value) []reflect.Value {
		atomic.AddInt64(counter, 1)
		return []reflect.Value{reflect.Zero(out)}
	}).Interface()
}

func testDeepChain(t *testing.T, backend di.Backend) {
	const chainN = 30
	var calls int64
	opts := []di.Option{di.Supply(reflect.Zero(arrType(0)).Interface())}
	for i := 1; i <= chainN; i++ {
		opts = append(opts, di.Provide(chainCtor(i, &calls)))
	}

	// Without a demand the whole chain stays cold.
	build(t, backend, opts...)
	if calls != 0 {
		t.Fatalf("deep chain: %d ctors ran before demand; want 0", calls)
	}

	// Populating the tail forces the whole chain — each ctor runs exactly once.
	calls = 0
	build(t, backend, append(opts, di.Populate(reflect.New(arrType(chainN)).Interface()))...)
	if calls != chainN {
		t.Fatalf("deep chain: %d ctors ran; want %d (once each)", calls, chainN)
	}
}
