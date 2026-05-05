package registry

import (
	"sync"
	"testing"

	"github.com/paulmanoni/nexus/middleware"
)

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