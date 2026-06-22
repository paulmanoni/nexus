// Package decorate is the runtime side of nexus's //@-annotation system. The
// codegen (`nexus generate handlers`, run by `nexus dev`/`build`) scans handler
// functions for //@ directives and emits, per package, an init() that calls
// Register with that package's registrations:
//
//	// generated, in package "users":
//	func init() {
//	    decorate.Register("users",
//	        nexus.Provide(NewService),
//	        nexus.AsRest("GET", "/users/:id", NewGetUser),
//	        inertia.Page("GET", "/dash", "Dashboard", NewDash),  // custom decorator
//	    )
//	}
//
// Register accumulates these into a process-global registry. nexus.Boot/Run
// then AUTO-DRAINS it — decorate registers its drain as a deferred option
// source from init() below — so the app wires up with zero ceremony:
//
//	func main() { nexus.Boot() }   // no decorate.Module, no blank imports
//
// Each Register group becomes a nexus.Module named after its package, so the
// dashboard groups decorator endpoints by package automatically. decorate
// imports nexus (never the reverse), so the auto-drain hook introduces no cycle.
package decorate

import (
	"sync"

	"github.com/paulmanoni/nexus"
)

var reg struct {
	mu   sync.Mutex
	opts []nexus.Option
}

// Register records already-built nexus.Options for inclusion at Boot/Run. The
// generated init() passes a fully-formed nexus.Module per package, e.g.
//
//	decorate.Register(nexus.Module("users",
//	    nexus.Provide(NewService),
//	    nexus.AsRest("GET", "/users/:id", NewGetUser),
//	))
//
// so the registration is plain, inspectable nexus wiring — the module is right
// there in the generated code. Register just buffers it; Drain hands it to
// Boot/Run.
func Register(opts ...nexus.Option) {
	if len(opts) == 0 {
		return
	}
	reg.mu.Lock()
	reg.opts = append(reg.opts, opts...)
	reg.mu.Unlock()
}

// Drain returns every registered option and clears the registry. nexus.Boot/Run
// calls it automatically (registered as a deferred option source in init);
// exported for tests and advanced wiring.
func Drain() []nexus.Option {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	out := reg.opts
	reg.opts = nil
	return out
}

func init() { nexus.RegisterDeferredOptions(Drain) }

// Pending reports how many options are buffered (un-drained). Useful in tests.
func Pending() int {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	return len(reg.opts)
}

// Reset clears the registry without draining — for test isolation.
func Reset() {
	reg.mu.Lock()
	reg.opts = nil
	reg.mu.Unlock()
}
