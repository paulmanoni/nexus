package fxcontainer

import (
	"context"
	"testing"

	"github.com/paulmanoni/nexus/di"
)

type svc struct{ name string }
type hook struct{ id int }

// TestAdapterRunsGraphOnFx exercises every translated primitive — Provide,
// Supply, Invoke, value-group ResultTags/ParamTags, optional ParamTags, and the
// di.Lifecycle→fx.Lifecycle bridge — through the fx backend, asserting the same
// behavior the builtin container gives.
func TestAdapterRunsGraphOnFx(t *testing.T) {
	var (
		injected  *svc
		group     []hook
		optSeen   = true
		started   bool
		stopped   bool
		lifecycle di.Lifecycle
	)

	spec := di.Collect(
		di.Supply("cfg-value"),
		di.Provide(func(s string) *svc { return &svc{name: s} }),
		di.Provide(di.Annotate(func() hook { return hook{id: 1} }, di.ResultTags(`group:"h"`))),
		di.Provide(di.Annotate(func() hook { return hook{id: 2} }, di.ResultTags(`group:"h"`))),
		di.Invoke(func(s *svc) { injected = s }),
		di.Invoke(di.Annotate(func(hs []hook) { group = hs }, di.ParamTags(`group:"h"`))),
		di.Invoke(di.Annotate(func(missing *int) { optSeen = missing != nil }, di.ParamTags(`optional:"true"`))),
		di.Invoke(func(lc di.Lifecycle) {
			lifecycle = lc
			lc.Append(di.Hook{
				OnStart: func(context.Context) error { started = true; return nil },
				OnStop:  func(context.Context) error { stopped = true; return nil },
			})
		}),
	)

	inst := New().Build(spec)
	if err := inst.Err(); err != nil {
		t.Fatalf("fx build: %v", err)
	}
	if injected == nil || injected.name != "cfg-value" {
		t.Fatalf("provide/supply: got %+v", injected)
	}
	if len(group) != 2 || group[0].id+group[1].id != 3 {
		t.Fatalf("value group: got %+v", group)
	}
	if optSeen {
		t.Fatal("optional param should be nil under fx")
	}
	if lifecycle == nil {
		t.Fatal("di.Lifecycle was not bridged/injected")
	}

	if err := inst.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !started {
		t.Fatal("OnStart hook did not run via the fx lifecycle bridge")
	}
	if err := inst.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !stopped {
		t.Fatal("OnStop hook did not run via the fx lifecycle bridge")
	}
}
