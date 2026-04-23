package nexus

import (
	"context"
	"reflect"
	"testing"
)

// Benchmarks for the reflective handler path used by AsRest, AsQuery,
// and AsMutation. The baseline (Benchmark*_Direct) calls the same
// handler without reflection — subtracting the two numbers gives the
// per-call cost of the nexus wrapper.

// --- handlers used below ---

type benchSvc struct{ *Service }
type benchDeps struct{ Count int }
type benchArgs struct {
	Title string `graphql:"title"`
	Count int    `graphql:"count"`
}

// plainHandler is the user code we'd write. Benchmarked directly and
// via reflect to quantify the overhead of the reflective path.
func plainHandler(svc *benchSvc, ctx context.Context, deps *benchDeps, a benchArgs) (*string, error) {
	msg := a.Title
	_ = ctx
	_ = svc
	_ = deps
	_ = a.Count
	return &msg, nil
}

// --- benchmarks ---

// InspectHandler is called once per registration at boot. Cost is not
// per-request but regressions here mean fx.Start slows down as apps
// grow.
func BenchmarkInspectHandler(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = inspectHandler(plainHandler)
	}
}

// CallHandler is the per-request cost of invoking the user handler
// through reflect.Value.Call. Compare against CallDirect to see the
// reflection tax.
func BenchmarkCallHandler(b *testing.B) {
	sh, err := inspectHandler(plainHandler)
	if err != nil {
		b.Fatal(err)
	}
	svc := &benchSvc{}
	deps := &benchDeps{Count: 3}
	args := reflect.ValueOf(benchArgs{Title: "hi", Count: 1})
	depSlice := []reflect.Value{reflect.ValueOf(svc), reflect.ValueOf(deps)}
	ci := callInput{Ctx: context.Background()}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = sh.callHandler(ci, depSlice, args)
	}
}

// CallDirect is the baseline — same handler called with regular Go
// semantics. Anything over this + a small constant is nexus overhead.
func BenchmarkCallDirect(b *testing.B) {
	svc := &benchSvc{}
	deps := &benchDeps{Count: 3}
	args := benchArgs{Title: "hi", Count: 1}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = plainHandler(svc, ctx, deps, args)
	}
}

// BindGqlArgs is the reflection+map bind that turns p.Args into the
// typed struct on every GraphQL request with args.
func BenchmarkBindGqlArgs(b *testing.B) {
	args := map[string]any{"title": "hello", "count": int64(7)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var a benchArgs
		_ = bindGqlArgs(&a, args)
	}
}
