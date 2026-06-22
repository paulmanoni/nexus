package fxcontainer

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/paulmanoni/nexus/di"
)

// The adapter's whole job is to be observably identical to the builtin
// container for any Spec nexus produces. These tests build the SAME graph
// shape through both backends and assert matching results — deep chains
// (lazy + once-each), large value groups (complete + correct), and lifecycle
// ordering (append order start, reverse stop).

var byteType = reflect.TypeFor[byte]()

func arrType(n int) reflect.Type { return reflect.ArrayOf(n, byteType) }

func chainCtor(n int, counter *int64) any {
	in, out := arrType(n-1), arrType(n)
	ft := reflect.FuncOf([]reflect.Type{in}, []reflect.Type{out}, false)
	return reflect.MakeFunc(ft, func([]reflect.Value) []reflect.Value {
		atomic.AddInt64(counter, 1)
		return []reflect.Value{reflect.Zero(out)}
	}).Interface()
}

type member struct{ n int }

// scenario builds one self-contained graph (fresh counters) so it can be run
// independently against each backend. It returns the options plus the
// observation sinks the test reads after Build/Start/Stop.
type obs struct {
	chainCalls int64
	groupCount int
	groupSum   int
	starts     []int
	stops      []int
	mu         sync.Mutex
}

func scenario(chainN, groupN, hooks int) ([]di.Option, *obs) {
	o := &obs{}
	opts := []di.Option{di.Supply(reflect.Zero(arrType(0)).Interface())}
	for i := 1; i <= chainN; i++ {
		opts = append(opts, di.Provide(chainCtor(i, &o.chainCalls)))
	}
	opts = append(opts, di.Populate(reflect.New(arrType(chainN)).Interface()))

	for i := 0; i < groupN; i++ {
		i := i
		opts = append(opts, di.Provide(di.Annotate(func() member { return member{n: i} },
			di.ResultTags(`group:"g"`))))
	}
	opts = append(opts, di.Invoke(di.Annotate(func(ms []member) {
		o.groupCount = len(ms)
		for _, m := range ms {
			o.groupSum += m.n
		}
	}, di.ParamTags(`group:"g"`))))

	opts = append(opts, di.Invoke(func(lc di.Lifecycle) {
		for i := 0; i < hooks; i++ {
			i := i
			lc.Append(di.Hook{
				OnStart: func(context.Context) error { o.mu.Lock(); o.starts = append(o.starts, i); o.mu.Unlock(); return nil },
				OnStop:  func(context.Context) error { o.mu.Lock(); o.stops = append(o.stops, i); o.mu.Unlock(); return nil },
			})
		}
	}))
	return opts, o
}

func runScenario(t *testing.T, backend di.Backend, chainN, groupN, hooks int) *obs {
	t.Helper()
	opts, o := scenario(chainN, groupN, hooks)
	inst := backend.Build(di.Collect(opts...))
	if err := inst.Err(); err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := inst.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := inst.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	return o
}

func TestParity_BuiltinVsFx(t *testing.T) {
	const (
		chainN = 250
		groupN = 1500
		hooks  = 400
	)
	b := runScenario(t, di.Builtin(), chainN, groupN, hooks)
	f := runScenario(t, New(), chainN, groupN, hooks)

	// Deep chain: every ctor ran exactly once on both.
	if b.chainCalls != chainN || f.chainCalls != chainN {
		t.Fatalf("chain calls: builtin=%d fx=%d want %d", b.chainCalls, f.chainCalls, chainN)
	}
	// Value group: complete and correct on both (order may differ between
	// backends, so compare count + sum, which are order-independent).
	wantSum := groupN * (groupN - 1) / 2
	if b.groupCount != groupN || f.groupCount != groupN {
		t.Fatalf("group count: builtin=%d fx=%d want %d", b.groupCount, f.groupCount, groupN)
	}
	if b.groupSum != wantSum || f.groupSum != wantSum {
		t.Fatalf("group sum: builtin=%d fx=%d want %d", b.groupSum, f.groupSum, wantSum)
	}
	// Lifecycle: identical ordering guarantees on both backends.
	assertSeq(t, "builtin", b, hooks)
	assertSeq(t, "fx", f, hooks)
}

func assertSeq(t *testing.T, name string, o *obs, hooks int) {
	t.Helper()
	if len(o.starts) != hooks || len(o.stops) != hooks {
		t.Fatalf("%s: starts=%d stops=%d want %d", name, len(o.starts), len(o.stops), hooks)
	}
	for i := 0; i < hooks; i++ {
		if o.starts[i] != i {
			t.Fatalf("%s: start order broken at %d: %d", name, i, o.starts[i])
		}
		if o.stops[i] != hooks-1-i {
			t.Fatalf("%s: stop not reversed at %d: %d", name, i, o.stops[i])
		}
	}
}

func TestParity_LazinessMatches(t *testing.T) {
	// Neither backend should construct anything that nothing demands.
	build := func(backend di.Backend) int64 {
		var calls int64
		opts := []di.Option{di.Supply(reflect.Zero(arrType(0)).Interface())}
		for i := 1; i <= 300; i++ {
			opts = append(opts, di.Provide(chainCtor(i, &calls)))
		}
		// No Populate/Invoke demands the chain.
		inst := backend.Build(di.Collect(opts...))
		if err := inst.Err(); err != nil {
			t.Fatalf("build: %v", err)
		}
		return calls
	}
	if b, f := build(di.Builtin()), build(New()); b != 0 || f != 0 {
		t.Fatalf("laziness mismatch: builtin=%d fx=%d, want 0/0", b, f)
	}
}
