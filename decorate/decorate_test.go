package decorate

import (
	"testing"

	"github.com/paulmanoni/nexus"
)

func aHandler() {}
func aQuery()   {}

// TestRegisterDrainReset is the core contract: Register buffers per-package
// groups, Drain turns each into a nexus.Module and clears the registry.
func TestRegisterDrainReset(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	Register(nexus.Module("users",
		nexus.Provide(aHandler),
		nexus.AsRest("GET", "/users/:id", aHandler),
	))
	Register(nexus.Module("billing", nexus.AsQuery(aQuery)))

	if Pending() != 2 {
		t.Fatalf("Pending() = %d after two Register calls, want 2", Pending())
	}
	opts := Drain()
	if len(opts) != 2 {
		t.Fatalf("Drain() returned %d module options, want 2", len(opts))
	}
	for i, o := range opts {
		if o == nil {
			t.Fatalf("Drain option %d is nil", i)
		}
	}
	if Pending() != 0 {
		t.Fatalf("registry not cleared after Drain: Pending() = %d", Pending())
	}
	// Second drain on an empty registry is harmless.
	if got := Drain(); len(got) != 0 {
		t.Fatalf("second Drain returned %d, want 0", len(got))
	}
}

func TestRegisterEmptyIsNoop(t *testing.T) {
	Reset()
	t.Cleanup(Reset)
	Register() // no options
	if Pending() != 0 {
		t.Fatalf("empty Register recorded a group: Pending() = %d", Pending())
	}
}
