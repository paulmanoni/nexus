package registry

import (
	"sync"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/resource"
)

// slowResource lets a test pin a Healthy() probe duration and
// observed value, used to verify Resources() doesn't block past the
// per-probe budget.
type slowResource struct {
	name    string
	delay   time.Duration
	healthy bool
}

func (r *slowResource) Name() string             { return r.name }
func (r *slowResource) Kind() resource.Kind      { return resource.KindOther }
func (r *slowResource) Describe() string         { return "" }
func (r *slowResource) Details() map[string]any  { return nil }
func (r *slowResource) IsDefault() bool          { return false }
func (r *slowResource) Healthy() bool {
	time.Sleep(r.delay)
	return r.healthy
}

func TestRegistry_ConcurrentRegistration(t *testing.T) {
	r := New()
	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			r.RegisterEndpoint(Endpoint{Service: "s", Name: "e", Transport: REST})
		}(i)
	}
	wg.Wait()
	if got := len(r.Endpoints()); got != n {
		t.Fatalf("expected %d endpoints, got %d", n, got)
	}
}

func TestRegistry_ServiceDescriptionPreservedOnReRegister(t *testing.T) {
	r := New()
	r.RegisterService(Service{Name: "pets", Description: "Pet inventory"})
	r.RegisterService(Service{Name: "pets"}) // second register without description
	svcs := r.Services()
	if len(svcs) != 1 {
		t.Fatalf("expected 1 service, got %d", len(svcs))
	}
	if svcs[0].Description != "Pet inventory" {
		t.Fatalf("description was overwritten: %q", svcs[0].Description)
	}
}

func TestRegistry_EndpointsByService(t *testing.T) {
	r := New()
	r.RegisterEndpoint(Endpoint{Service: "a", Name: "1"})
	r.RegisterEndpoint(Endpoint{Service: "b", Name: "2"})
	r.RegisterEndpoint(Endpoint{Service: "a", Name: "3"})
	if got := len(r.EndpointsByService("a")); got != 2 {
		t.Fatalf("expected 2 endpoints in service a, got %d", got)
	}
}

// TestRegistry_OnChangeFiresOnMutations is the contract test for
// push-on-change: every mutating method must invoke the hook so the
// dashboard's snapshot stream sees state updates without waiting on
// a heartbeat. A regression here means the dashboard appears stuck
// for up to 5s after a worker flips status.
func TestRegistry_OnChangeFiresOnMutations(t *testing.T) {
	r := New()
	var calls int
	var mu sync.Mutex
	r.OnChange(func() {
		mu.Lock()
		calls++
		mu.Unlock()
	})

	mutators := []struct {
		name string
		fn   func()
	}{
		{"RegisterService", func() { r.RegisterService(Service{Name: "svc-1"}) }},
		{"RegisterEndpoint", func() { r.RegisterEndpoint(Endpoint{Service: "svc-1", Name: "ep-1"}) }},
		{"RegisterWorker", func() { r.RegisterWorker(Worker{Name: "w-1"}) }},
		{"UpdateWorkerStatus", func() { r.UpdateWorkerStatus("w-1", "running", "") }},
		{"SetServiceDeps", func() { r.SetServiceDeps("svc-1", []string{"db"}, nil) }},
		{"AttachResource", func() { r.AttachResource("svc-1", "db") }},
		{"RegisterMiddleware", func() { r.RegisterMiddleware(middleware.Info{Name: "auth"}) }},
		{"EnsureMiddleware", func() { r.EnsureMiddleware("rate-limit") }},
		{"RegisterGlobalMiddleware", func() { r.RegisterGlobalMiddleware("global-auth") }},
		{"SetEndpointMiddlewares", func() { r.SetEndpointMiddlewares("svc-1", REST, []string{"auth"}) }},
		{"SetEndpointResources", func() { r.SetEndpointResources("svc-1", "ep-1", []string{"db"}) }},
		{"SetEndpointServiceDeps", func() { r.SetEndpointServiceDeps("svc-1", "ep-1", []string{"svc-2"}) }},
		{"SetEndpointModule", func() { r.SetEndpointModule("svc-1", "ep-1", "mod") }},
		{"SetEndpointDeployment", func() { r.SetEndpointDeployment("svc-1", "ep-1", "monolith") }},
		{"SetEndpointServiceAutoRouted", func() { r.SetEndpointServiceAutoRouted("svc-1", "ep-1") }},
		{"SetEndpointRateLimit", func() { r.SetEndpointRateLimit("svc-1", "ep-1", RateLimitInfo{}) }},
	}
	for _, m := range mutators {
		mu.Lock()
		before := calls
		mu.Unlock()
		m.fn()
		mu.Lock()
		after := calls
		mu.Unlock()
		if after <= before {
			t.Errorf("%s did not invoke OnChange hook (calls before=%d after=%d)", m.name, before, after)
		}
	}
}

// TestResources_ParallelHealthProbe pins that Resources() runs
// Healthy() probes in parallel with a per-probe timeout — wedged
// or merely slow probes can't pin the snapshot past the budget,
// which is what made the dashboard's first /__nexus/live frame
// feel sluggish under apps with multiple DB-pinging resources.
func TestResources_ParallelHealthProbe(t *testing.T) {
	r := New()
	// Three resources: one fast healthy, one fast unhealthy, one
	// slow enough to exceed the probe budget. Total wall-clock for
	// Resources() must approach the BUDGET — not 3 × the slow probe.
	r.RegisterResource(&slowResource{name: "fast-ok", delay: 10 * time.Millisecond, healthy: true})
	r.RegisterResource(&slowResource{name: "fast-bad", delay: 10 * time.Millisecond, healthy: false})
	r.RegisterResource(&slowResource{name: "wedged", delay: 5 * time.Second, healthy: true})

	start := time.Now()
	out := r.Resources()
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("Resources() took %v; want < 2s (probe budget cap)", elapsed)
	}
	byName := map[string]ResourceSnapshot{}
	for _, s := range out {
		byName[s.Name] = s
	}
	if !byName["fast-ok"].Healthy {
		t.Error("fast-ok should report Healthy=true on first probe")
	}
	if byName["fast-bad"].Healthy {
		t.Error("fast-bad should report Healthy=false on first probe")
	}
	// wedged probe didn't return in time → no cached value yet → false.
	if byName["wedged"].Healthy {
		t.Error("wedged should fall back to false (no cache) on first probe")
	}
}

// TestResources_HealthCacheServesStaleOnTimeout proves the cache
// path: once a probe has reported, subsequent timeouts surface the
// last-known value instead of false.
func TestResources_HealthCacheServesStaleOnTimeout(t *testing.T) {
	r := New()
	// flaky reports healthy=true once quickly, then takes much
	// longer on subsequent probes — simulating a backend that
	// becomes intermittently unresponsive after a successful health
	// check. The cache should keep showing healthy.
	flaky := &flakyResource{name: "flaky"}
	r.RegisterResource(flaky)

	// First probe completes within the budget — cache populated.
	flaky.SetDelay(10 * time.Millisecond, true)
	first := r.Resources()
	if !first[0].Healthy {
		t.Fatal("first probe should report healthy=true")
	}

	// Subsequent probes take longer than the budget; Resources()
	// must serve the cached value rather than falling back to
	// false.
	flaky.SetDelay(5*time.Second, true)
	second := r.Resources()
	if !second[0].Healthy {
		t.Error("cache should serve last-observed Healthy=true on timeout")
	}
}

// flakyResource exposes a tunable Healthy() so the cache test can
// flip between fast and slow probes between calls.
type flakyResource struct {
	name string
	mu   sync.Mutex
	d    time.Duration
	h    bool
}

func (r *flakyResource) Name() string            { return r.name }
func (r *flakyResource) Kind() resource.Kind     { return resource.KindOther }
func (r *flakyResource) Describe() string        { return "" }
func (r *flakyResource) Details() map[string]any { return nil }
func (r *flakyResource) IsDefault() bool         { return false }
func (r *flakyResource) Healthy() bool {
	r.mu.Lock()
	d, h := r.d, r.h
	r.mu.Unlock()
	time.Sleep(d)
	return h
}
func (r *flakyResource) SetDelay(d time.Duration, h bool) {
	r.mu.Lock()
	r.d, r.h = d, h
	r.mu.Unlock()
}

// TestRegistry_OnChangeNilHookSafe pins the contract that a registry
// without a hook installed (the default — tests, non-dashboard apps)
// is fully usable. A regression here would crash boot for any caller
// that hadn't called OnChange.
func TestRegistry_OnChangeNilHookSafe(t *testing.T) {
	r := New()
	r.RegisterService(Service{Name: "x"})
	r.RegisterEndpoint(Endpoint{Service: "x", Name: "e"})
	r.UpdateWorkerStatus("nope", "running", "") // no-op for unknown worker
	if got := len(r.Services()); got != 1 {
		t.Fatalf("services lost: %d", got)
	}
}