package di

import (
	"context"
	"errors"
	"testing"
)

type pinger struct{ name string }
type config struct{ addr string }

func TestProvideResolveAndInvoke(t *testing.T) {
	var got *pinger
	app := New(
		Supply(config{addr: ":8080"}),
		Provide(func(c config) *pinger { return &pinger{name: c.addr} }),
		Invoke(func(p *pinger) { got = p }),
	)
	if err := app.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if got == nil || got.name != ":8080" {
		t.Fatalf("invoke saw %+v", got)
	}
}

func TestConstructorIsLazyAndSingleton(t *testing.T) {
	calls := 0
	app := New(
		Provide(func() *pinger { calls++; return &pinger{} }),
		// pinger is provided but never demanded -> must not run.
	)
	if err := app.Err(); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("lazy: ctor ran %d times before demand", calls)
	}

	calls = 0
	New(
		Provide(func() *pinger { calls++; return &pinger{} }),
		Invoke(func(*pinger) {}),
		Invoke(func(*pinger) {}),
	)
	if calls != 1 {
		t.Fatalf("singleton: ctor ran %d times for two consumers, want 1", calls)
	}
}

func TestErrorReturnAbortsBuild(t *testing.T) {
	boom := errors.New("boom")
	app := New(
		Provide(func() (*pinger, error) { return nil, boom }),
		Invoke(func(*pinger) {}),
	)
	if !errors.Is(app.Err(), boom) {
		t.Fatalf("want boom, got %v", app.Err())
	}
}

func TestOptionalAndDefault(t *testing.T) {
	type params struct {
		In
		Cfg   config
		Cache *pinger `optional:"true"`
	}
	var seen params
	app := New(
		Supply(config{addr: "x"}),
		Invoke(func(p params) { seen = p }),
	)
	if err := app.Err(); err != nil {
		t.Fatalf("optional-absent should not error: %v", err)
	}
	if seen.Cfg.addr != "x" || seen.Cache != nil {
		t.Fatalf("got %+v", seen)
	}
}

func TestMissingRequiredErrors(t *testing.T) {
	app := New(Invoke(func(*pinger) {}))
	if app.Err() == nil {
		t.Fatal("expected error for unprovided required dep")
	}
}

// hook mirrors how nexus tags GraphQL fields into a value group.
type hook struct{ id int }

func TestValueGroupViaAnnotate(t *testing.T) {
	type collector struct {
		In
		Hooks []hook `group:"hooks"`
	}
	var collected []hook
	app := New(
		Provide(Annotate(func() hook { return hook{id: 1} }, ResultTags(`group:"hooks"`))),
		Provide(Annotate(func() hook { return hook{id: 2} }, ResultTags(`group:"hooks"`))),
		Invoke(func(c collector) { collected = c.Hooks }),
	)
	if err := app.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(collected) != 2 {
		t.Fatalf("group collected %d, want 2: %+v", len(collected), collected)
	}
	sum := collected[0].id + collected[1].id
	if sum != 3 {
		t.Fatalf("group members wrong: %+v", collected)
	}
}

func TestLifecycleOrder(t *testing.T) {
	var order []string
	app := New(
		Invoke(func(lc Lifecycle) {
			lc.Append(Hook{
				OnStart: func(context.Context) error { order = append(order, "start-A"); return nil },
				OnStop:  func(context.Context) error { order = append(order, "stop-A"); return nil },
			})
			lc.Append(Hook{
				OnStart: func(context.Context) error { order = append(order, "start-B"); return nil },
				OnStop:  func(context.Context) error { order = append(order, "stop-B"); return nil },
			})
		}),
	)
	if err := app.Err(); err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"start-A", "start-B", "stop-B", "stop-A"}
	if len(order) != 4 {
		t.Fatalf("order=%v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order=%v want %v", order, want)
		}
	}
}

func TestStartFailureRollsBack(t *testing.T) {
	var stopped []string
	app := New(
		Invoke(func(lc Lifecycle) {
			lc.Append(Hook{
				OnStart: func(context.Context) error { return nil },
				OnStop:  func(context.Context) error { stopped = append(stopped, "A"); return nil },
			})
			lc.Append(Hook{
				OnStart: func(context.Context) error { return errors.New("nope") },
			})
		}),
	)
	if err := app.Start(context.Background()); err == nil {
		t.Fatal("expected start failure")
	}
	if len(stopped) != 1 || stopped[0] != "A" {
		t.Fatalf("rollback stopped=%v, want [A]", stopped)
	}
}

func TestInvokeOrderPreservedProvideOrderNot(t *testing.T) {
	var order []string
	app := New(
		Invoke(func(*pinger) { order = append(order, "first") }),
		// pinger provided AFTER the invoke that needs it — must still resolve.
		Provide(func() *pinger { return &pinger{} }),
		Invoke(func(*pinger) { order = append(order, "second") }),
	)
	if err := app.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("invoke order=%v", order)
	}
}

func TestDuplicateProvideErrors(t *testing.T) {
	app := New(
		Provide(func() *pinger { return &pinger{} }),
		Provide(func() *pinger { return &pinger{} }),
		Invoke(func(*pinger) {}),
	)
	if app.Err() == nil {
		t.Fatal("expected duplicate-provide error")
	}
}

func TestCycleDetected(t *testing.T) {
	type a struct{ x int }
	type b struct{ y int }
	app := New(
		Provide(func(*b) *a { return &a{} }),
		Provide(func(*a) *b { return &b{} }),
		Invoke(func(*a) {}),
	)
	if app.Err() == nil {
		t.Fatal("expected cycle error")
	}
}

func TestErrorOptionAborts(t *testing.T) {
	sentinel := errors.New("bad registration")
	app := New(
		Error(sentinel),
		Provide(func() *pinger { return &pinger{} }),
	)
	if !errors.Is(app.Err(), sentinel) {
		t.Fatalf("want sentinel, got %v", app.Err())
	}
}

func TestParamTagsGroupAndOptionalOnInvoke(t *testing.T) {
	// Mirrors how nexus consumes the GraphQL field group + the optional
	// default gate: annotated invokes with ParamTags, no In-struct.
	var fields []hook
	var gate *config
	app := New(
		Provide(Annotate(func() hook { return hook{id: 7} }, ResultTags(`group:"f"`))),
		Provide(Annotate(func() hook { return hook{id: 8} }, ResultTags(`group:"f"`))),
		Invoke(Annotate(func(fs []hook) { fields = fs }, ParamTags(`group:"f"`))),
		Invoke(Annotate(func(g *config) { gate = g }, ParamTags(`optional:"true"`))),
	)
	if err := app.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(fields) != 2 {
		t.Fatalf("group param got %d fields", len(fields))
	}
	if gate != nil {
		t.Fatalf("optional param should be nil, got %+v", gate)
	}
}

func TestPopulate(t *testing.T) {
	var got *pinger
	app := New(
		Provide(func() *pinger { return &pinger{name: "populated"} }),
		Populate(&got),
	)
	if err := app.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if got == nil || got.name != "populated" {
		t.Fatalf("populate got %+v", got)
	}
}

func TestBuiltinBackend(t *testing.T) {
	var got *pinger
	inst := Builtin().Build(Collect(
		Provide(func() *pinger { return &pinger{name: "via-backend"} }),
		Populate(&got),
	))
	if err := inst.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if got == nil || got.name != "via-backend" {
		t.Fatalf("backend got %+v", got)
	}
}

func TestOutStructSpread(t *testing.T) {
	type results struct {
		Out
		P *pinger
		C config
	}
	var p *pinger
	var c config
	app := New(
		Provide(func() results { return results{P: &pinger{name: "z"}, C: config{addr: "q"}} }),
		Invoke(func(pp *pinger, cc config) { p, c = pp, cc }),
	)
	if err := app.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if p == nil || p.name != "z" || c.addr != "q" {
		t.Fatalf("out spread: %+v %+v", p, c)
	}
}
