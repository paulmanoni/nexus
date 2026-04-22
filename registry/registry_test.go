package registry

import (
	"sync"
	"testing"
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