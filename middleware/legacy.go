package middleware

import (
	"fmt"

	"github.com/paulmanoni/nexus/graph"
)

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

// Handle bridges to the legacy realization for the active transport.
//
//   - GraphQL: b.mw.Graph is already func(next) next — bridged via bridgeGraph.
//   - REST / WS: b.mw.Gin is a gin.HandlerFunc driven by gin's own c.Next();
//     the REST/WS adapter (step 3) unwraps legacyBundle and runs b.mw.Gin
//     natively in its gin chain rather than routing through Handle. So this
//     path is intentionally a guard until step 3 wires the unwrap.
func (b legacyBundle) Handle(rc *RequestCtx, next Next) error {
	switch rc.Transport {
	case TransportGraphQL:
		if b.mw.Graph == nil {
			return next(rc)
		}
		return bridgeGraph(rc, b.mw.Graph, next)
	default:
		return fmt.Errorf("nexus: legacyBundle %q has no functional realization for %s; "+
			"the gin path is run natively by the REST/WS adapter (step 3)", b.mw.Name, rc.Transport)
	}
}

// bridgeGraph runs a legacy graph.FieldMiddleware inside the functional chain.
// Full body lands with the GraphQL carrier in step 3; stubbed so legacy.go
// compiles and the GraphQL bridge has a home. See impl plan §2.1 / §6.
func bridgeGraph(rc *RequestCtx, _ graph.FieldMiddleware, next Next) error {
	return next(rc) // TODO(step 3): drive the FieldMiddleware via the GraphQL carrier
}
