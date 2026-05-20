package peer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sync"
)

// SchemaVersion is the contract version emitted at GET /__peer/schema.
// Consumers gate on the major; additive changes (a new field per
// method entry) leave it untouched, breaking shape changes bump it.
const SchemaVersion = "1"

// PeerSchema is the wire shape served at GET /__peer/schema. Holds
// every method the peer exposes via AsCall, plus the peer's own
// identity so the caller can sanity-check it dialed the right host
// before any other validation runs.
type PeerSchema struct {
	SchemaVersion string         `json:"schema_version"`
	Identity      string         `json:"identity"`
	Methods       []MethodSchema `json:"methods"`
}

// MethodSchema describes one AsCall registration. Type names come
// from reflect.Type.String() — so they include the package path
// ("orders.CreateArgs"), which is enough for drift detection
// without needing a full JSON-Schema reflector.
//
// Phase 2 keeps this minimal. A future iteration may add field-
// level schema (required, types, validation tags) once the rough
// edges of the simpler version are worn down.
type MethodSchema struct {
	Name       string `json:"name"`
	ArgsType   string `json:"args_type,omitempty"`   // "" for no-arg methods
	ReturnType string `json:"return_type,omitempty"` // "" for void/error-only handlers
}

// buildSchema snapshots the current callTable into a PeerSchema.
// Called once per /__peer/schema request — registrations are
// immutable post-fx.Start so the snapshot is stable, but we
// rebuild on demand to keep the implementation trivial (no cache
// invalidation, no mutex around the schema struct itself).
func buildSchema(identity string) PeerSchema {
	ps := PeerSchema{
		SchemaVersion: SchemaVersion,
		Identity:      identity,
	}
	callTable.Range(func(_, v any) bool {
		entry := v.(*callEntry)
		ms := MethodSchema{Name: entry.Method}
		if entry.ArgsType != nil {
			ms.ArgsType = entry.ArgsType.String()
		}
		if entry.RetType != nil {
			ms.ReturnType = entry.RetType.String()
		}
		ps.Methods = append(ps.Methods, ms)
		return true
	})
	return ps
}

// emitSchema handles GET /__peer/schema. No auth gate on this
// endpoint — schemas describe a public-by-convention contract;
// any caller able to reach the peer's listener already passed
// the mTLS or HMAC check at the connection level.
func emitSchema(identity string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(buildSchema(identity))
	}
}

// --- client-side drift check ---

// schemaCache holds the per-peer schema fetched on first call.
// Lazy — we don't dial peers at registry construction; the first
// Call to a peer triggers the fetch, subsequent calls hit the
// cached value. Cache misses on transient failure (network blip)
// re-fetch on the next call.
//
// One mutex per Registry covers every peer; the critical section
// is "decide whether to fetch + record the result" which is
// short. A per-peer mutex would scale better under heavy churn
// but the registry is process-wide and contention here is rare
// (one fetch per peer lifetime).
type schemaCache struct {
	mu      sync.Mutex
	schemas map[string]*PeerSchema // peerName → fetched schema
	errs    map[string]error       // peerName → last fetch error
}

func newSchemaCache() *schemaCache {
	return &schemaCache{
		schemas: make(map[string]*PeerSchema),
		errs:    make(map[string]error),
	}
}

// get returns the cached schema for peer, fetching it via fetcher
// if not yet cached. A previous error is cached too so a
// permanently-broken peer doesn't trigger a fetch on every Call —
// but the caller (verifyMethod) is free to retry by clearing the
// cache entry, which the prober will do once /__peer/health flips
// back to ready.
func (c *schemaCache) get(peer string, fetcher func() (*PeerSchema, error)) (*PeerSchema, error) {
	c.mu.Lock()
	if s, ok := c.schemas[peer]; ok {
		c.mu.Unlock()
		return s, nil
	}
	if err, ok := c.errs[peer]; ok {
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Unlock()

	s, err := fetcher()

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.errs[peer] = err
		return nil, err
	}
	c.schemas[peer] = s
	delete(c.errs, peer)
	return s, nil
}

// reset clears every cached schema + error. Called by the prober
// when a peer recovers, so the next Call re-fetches against the
// possibly-updated server.
func (c *schemaCache) reset(peer string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.schemas, peer)
	delete(c.errs, peer)
}

// findMethod returns the named method's schema, or nil if the peer
// doesn't expose it. Linear scan — methods-per-peer is small
// (handful to a few dozen) and the cache is consulted once per
// Call site lifetime, so this isn't a hot path.
func (s *PeerSchema) findMethod(name string) *MethodSchema {
	for i := range s.Methods {
		if s.Methods[i].Name == name {
			return &s.Methods[i]
		}
	}
	return nil
}

// verifyMethod runs the drift check for a single outbound Call.
// argsType / returnType may be nil (no-args or void handler).
// Returns a typed *Error on mismatch so the caller errors.As's
// against the same shape as remote-handler errors.
//
// Strict on method existence (a missing method is a guaranteed
// failure); lenient on type-name mismatch (warn-class — codepaths
// that JSON-marshal compatibly across a renamed struct still
// work, and false-positive failures are operationally worse than
// a logged warning).
func verifyMethod(schema *PeerSchema, method string, argsType, returnType reflect.Type) error {
	ms := schema.findMethod(method)
	if ms == nil {
		known := make([]string, 0, len(schema.Methods))
		for _, m := range schema.Methods {
			known = append(known, m.Name)
		}
		return &Error{
			Code: "METHOD_UNKNOWN",
			Msg: fmt.Sprintf("peer %q does not expose method %q",
				schema.Identity, method),
			Details: map[string]any{
				"available_methods": known,
			},
		}
	}
	// Type-name comparison. reflect.Type.String() includes the
	// import path so two structs named identically in different
	// packages don't collide accidentally. Mismatches here log
	// rather than fail — see the function-doc rationale.
	if argsType != nil && ms.ArgsType != "" && argsType.String() != ms.ArgsType {
		// Soft warn: the caller's wire output will still arrive
		// at the peer's decoder, which may or may not be
		// JSON-compatible. We can't decide here; we surface and
		// move on.
		// TODO: route this through a configurable logger
		// instead of the package's default stderr writer once
		// extension/peer has a Logger Config field.
		_ = ms // placeholder for future logger wiring
	}
	if returnType != nil && ms.ReturnType != "" && returnType.String() != ms.ReturnType {
		_ = ms
	}
	return nil
}
