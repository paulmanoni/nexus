package registry

import "testing"

// TestVisibleEndpoints_FiltersHidden verifies that endpoints tagged with
// HiddenTag (set by nexus.HideFromDashboard()) are dropped by
// VisibleEndpoints while still being returned by Endpoints() — hiding is a
// dashboard-only concern, the registry entry itself is untouched.
func TestVisibleEndpoints_FiltersHidden(t *testing.T) {
	r := New()
	r.RegisterEndpoint(Endpoint{Service: "s", Name: "public", Transport: REST})
	r.RegisterEndpoint(Endpoint{Service: "s", Name: "tagged-other", Transport: REST, Tags: map[string]string{"foo": "bar"}})
	r.RegisterEndpoint(Endpoint{Service: "s", Name: "hidden", Transport: REST, Tags: map[string]string{HiddenTag: "true"}})

	if got := len(r.Endpoints()); got != 3 {
		t.Fatalf("Endpoints() = %d, want 3 (hiding must not drop the registry entry)", got)
	}

	vis := r.VisibleEndpoints()
	if len(vis) != 2 {
		t.Fatalf("VisibleEndpoints() = %d, want 2", len(vis))
	}
	for _, e := range vis {
		if e.Name == "hidden" {
			t.Errorf("VisibleEndpoints() leaked hidden endpoint %q", e.Name)
		}
	}
}
