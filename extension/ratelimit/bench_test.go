package ratelimit

import (
	"context"
	"fmt"
	"testing"
)

// Benchmarks for the Store hot path. Allow() runs on every request
// that passes through a rate-limited middleware; Configure/Reset run
// when an operator edits a limit from the dashboard (rare but should
// not stall).

func BenchmarkMemoryStore_Allow_SingleKey(b *testing.B) {
	s := NewMemoryStore()
	s.Declare("svc.op", Limit{RPM: 6000000, Burst: 1000000}) // effectively open
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Allow(ctx, "svc.op", "")
	}
}

// Denial path: burst exhausted. Measures the "compute retryAfter"
// math since that's what fires when the app is under load.
func BenchmarkMemoryStore_Allow_Deny(b *testing.B) {
	s := NewMemoryStore()
	s.Declare("svc.op", Limit{RPM: 1, Burst: 1})
	ctx := context.Background()
	// Exhaust the single token.
	_, _ = s.Allow(ctx, "svc.op", "")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Allow(ctx, "svc.op", "")
	}
}

// Parallel allow: the real load shape. Every goroutine shares the
// bucket via the per-key entry mutex; contention is the cost we
// watch here.
func BenchmarkMemoryStore_Allow_Parallel(b *testing.B) {
	s := NewMemoryStore()
	s.Declare("svc.op", Limit{RPM: 60_000_000, Burst: 1_000_000}) // never throttle under bench
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = s.Allow(ctx, "svc.op", "")
		}
	})
}

// PerIP: different scopes share a key but need distinct buckets.
// Bucket creation hits a write lock once per new (key, scope) pair.
func BenchmarkMemoryStore_Allow_PerIP(b *testing.B) {
	s := NewMemoryStore()
	s.Declare("svc.op", Limit{RPM: 60_000_000, Burst: 1_000_000, PerIP: true})
	ctx := context.Background()
	ips := make([]string, 64)
	for i := range ips {
		ips[i] = fmt.Sprintf("10.0.0.%d", i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Allow(ctx, "svc.op", ips[i%len(ips)])
	}
}

// Configure: operator override from the dashboard. Includes the
// bucket invalidation for matching keys so the perf picture reflects
// the full "apply override" round-trip.
func BenchmarkMemoryStore_Configure(b *testing.B) {
	s := NewMemoryStore()
	s.Declare("svc.op", Limit{RPM: 60})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Configure(ctx, "svc.op", Limit{RPM: 100 + i%50})
	}
}

// Snapshot: dashboard Rate-limits tab polls this every few seconds.
// Cost scales with declared-endpoint count.
func BenchmarkMemoryStore_Snapshot(b *testing.B) {
	s := NewMemoryStore()
	for i := 0; i < 50; i++ {
		s.Declare(fmt.Sprintf("svc.op%d", i), Limit{RPM: 60})
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Snapshot(ctx)
	}
}
