package middleware

import "fmt"

// legacyBundle adapts the existing Middleware struct to the Handler interface.
// It's a wrapper (not methods on the struct) because the struct's Name FIELD
// would collide with a Name() METHOD — see
// docs/design/middleware-impl-steps-1-2.md §0.2.
type legacyBundle struct{ mw Middleware }

// AsHandler wraps a legacy bundle as a Handler so existing nexus.Use(...)
// bundles flow through the new pipeline and participate in fail-closed (step 3).
func AsHandler(mw Middleware) Handler { return legacyBundle{mw: mw} }

func (b legacyBundle) Name() string { return b.mw.Name }

// Transports infers the set from which realizations are present: Gin backs
// REST + the WS upgrade route; Graph backs GraphQL (redesign §3.1, §9 step 2).
func (b legacyBundle) Transports() TransportSet {
	var s TransportSet
	if b.mw.Gin != nil {
		s |= bit(TransportREST) | bit(TransportWebSocket)
	}
	if b.mw.Graph != nil {
		s |= bit(TransportGraphQL)
	}
	return s
}

// Handle is unreachable today: the only consumer of AsHandler
// (app_use.go) reads Transports() and runs the legacy realizations
// natively per transport. Fail loudly if a future chain builder
// starts calling it — the previous stub silently DROPPED a legacy
// graph.FieldMiddleware, which is worse than an error.
func (b legacyBundle) Handle(rc *RequestCtx, _ Next) error {
	return fmt.Errorf("nexus: legacyBundle %q: Handle is not wired; run the legacy realization natively for %s", b.mw.Name, rc.Transport)
}
