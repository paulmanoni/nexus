package metrics

import (
	"errors"
	"fmt"
	"testing"
)

// Benchmarks cover the hot path (Record on every request) and the
// dashboard-facing snapshot path. Compare MemoryStore to catch perf
// regressions when the internal layout shifts; future CacheStore
// benchmarks live next to its implementation.

func BenchmarkMemoryStore_Record_Success(b *testing.B) {
	s := NewMemoryStore()
	const key = "svc.op"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Record(key, "1.2.3.4", nil)
	}
}

func BenchmarkMemoryStore_Record_Error(b *testing.B) {
	s := NewMemoryStore()
	const key = "svc.op"
	err := errors.New("boom")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Record(key, "1.2.3.4", err)
	}
}

// Parallel: mimics a multi-goroutine server where every request hits
// Record. Locks are the bottleneck candidate here; if the atomic
// counters start contending we'll see it.
func BenchmarkMemoryStore_Record_Parallel(b *testing.B) {
	s := NewMemoryStore()
	const key = "svc.op"
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.Record(key, "1.2.3.4", nil)
		}
	})
}

// Record across MANY keys (simulates a wide endpoint surface).
// Catches regressions in the pick-entry path under key churn.
func BenchmarkMemoryStore_Record_ManyKeys(b *testing.B) {
	s := NewMemoryStore()
	keys := make([]string, 64)
	for i := range keys {
		keys[i] = fmt.Sprintf("svc.op%d", i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Record(keys[i%len(keys)], "1.2.3.4", nil)
	}
}

// Snapshot: dashboard polls this periodically. Cost scales with the
// endpoint count — benchmark at a realistic width.
func BenchmarkMemoryStore_Snapshot(b *testing.B) {
	s := NewMemoryStore()
	for i := 0; i < 50; i++ {
		s.Record(fmt.Sprintf("svc.op%d", i), "1.2.3.4", nil)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Snapshot()
	}
}

// Errors: dashboard calls this on a dialog open for one endpoint.
// With the ring filled to cap, this is the worst-case copy cost.
func BenchmarkMemoryStore_Errors_FullRing(b *testing.B) {
	s := NewMemoryStore()
	err := errors.New("boom")
	for i := 0; i < RecentErrorsCap+10; i++ {
		s.Record("svc.op", "1.2.3.4", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Errors("svc.op")
	}
}
