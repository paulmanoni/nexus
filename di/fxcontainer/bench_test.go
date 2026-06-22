package fxcontainer

import (
	"context"
	"testing"

	"github.com/paulmanoni/nexus/di"
)

// Head-to-head: the SAME graph (deep chain + large value group + lifecycle
// hooks) built through the builtin container vs the fx adapter. The adapter
// translates di.Spec→fx, so this measures the real cost of choosing fx.
//
//	go test -bench=. -benchmem ./di/fxcontainer/
//
// Representative-ish nexus app size; bump via the consts to probe scaling.
const (
	benchChain = 100
	benchGroup = 400
	benchHooks = 150
)

func benchBackends() []struct {
	name string
	be   di.Backend
} {
	return []struct {
		name string
		be   di.Backend
	}{
		{"builtin", di.Builtin()},
		{"fx", New()},
	}
}

// BenchmarkBuild measures Collect+Build: graph registration plus eager invokes
// (the bulk of boot-time DI work). Lifecycle is not started here.
func BenchmarkBuild(b *testing.B) {
	for _, c := range benchBackends() {
		b.Run(c.name, func(b *testing.B) {
			opts, _ := scenario(benchChain, benchGroup, benchHooks)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				inst := c.be.Build(di.Collect(opts...))
				if err := inst.Err(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkBuildStartStop measures the full boot+shutdown lifecycle cost.
func BenchmarkBuildStartStop(b *testing.B) {
	ctx := context.Background()
	for _, c := range benchBackends() {
		b.Run(c.name, func(b *testing.B) {
			opts, _ := scenario(benchChain, benchGroup, benchHooks)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				inst := c.be.Build(di.Collect(opts...))
				if err := inst.Err(); err != nil {
					b.Fatal(err)
				}
				if err := inst.Start(ctx); err != nil {
					b.Fatal(err)
				}
				if err := inst.Stop(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
