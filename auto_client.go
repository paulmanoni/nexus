package nexus

import (
	"sync"

	"go.uber.org/fx"
)

// Auto-client registry: codegen'd client files register their factory
// here at init() time so consumers don't have to manually
// `nexus.Provide(users.NewUsersClient)` for every peer they call into.
//
// At Run time, every registered factory is added to the fx graph as a
// Provide. fx is lazy — factories whose return types aren't depended
// on by any constructor are never invoked, so the cost of registering
// a client a binary doesn't use is zero.
//
// Collision rules: fx itself rejects duplicate providers of the same
// concrete return type. If a user keeps the legacy manual Provide line
// AND the codegen registers the same factory, fx will fail at boot
// with a "cannot Provide function of type X: already provided" error
// — that's the migration signal to delete the manual line.

var (
	autoClientMu        sync.Mutex
	autoClientFactories []any
)

// RegisterAutoClient stores a generated-client factory function so the
// framework can auto-Provide it at Run time. Generated client files
// (zz_*_client_gen.go) call this from their init() block; user code
// should not call it directly.
//
// fn must be a function suitable for fx.Provide — typically
// `func(*nexus.App) SomeClient`. Passing a non-function will fail at
// fx.New time, not here, since the registry stays type-erased to keep
// init() ordering simple.
func RegisterAutoClient(fn any) {
	autoClientMu.Lock()
	defer autoClientMu.Unlock()
	autoClientFactories = append(autoClientFactories, fn)
}

// autoClientOptions returns the fx.Options chain that Provides every
// registered factory. Called once from Run; safe to call repeatedly
// (each call returns the same set, fresh fx.Option values).
func autoClientOptions() fx.Option {
	autoClientMu.Lock()
	defer autoClientMu.Unlock()
	if len(autoClientFactories) == 0 {
		return fx.Options()
	}
	provides := make([]fx.Option, 0, len(autoClientFactories))
	for _, fn := range autoClientFactories {
		provides = append(provides, fx.Provide(fn))
	}
	return fx.Options(provides...)
}