package decorate

import (
	"testing"

	"github.com/paulmanoni/nexus"
)

func aHandler() {}
func aQuery()   {}

// TestRegisterDrainReset is the core contract: Register buffers per-package
// groups, Drain snapshots them without clearing, Reset clears.
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
	// Drain is a snapshot, not a consume: a later boot in the same binary
	// (a second InProcess in a test run) must see the same registrations.
	if Pending() != 2 {
		t.Fatalf("registry consumed by Drain: Pending() = %d, want 2", Pending())
	}
	if got := Drain(); len(got) != 2 {
		t.Fatalf("second Drain returned %d, want 2", len(got))
	}
	// Reset is the explicit clear.
	Reset()
	if got := Drain(); len(got) != 0 {
		t.Fatalf("Drain after Reset returned %d, want 0", len(got))
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
