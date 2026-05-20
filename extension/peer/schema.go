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

// MethodSchema describes one AsCall registration. ArgsType /
// ReturnType keep the type-name comparison (catches method
// renames, top-level struct renames); ArgsSchema / ReturnSchema
// emit the full JSON-Schema-2020-12 shape so structural drift
// (added required field, renamed property, type swap on a field)
// surfaces too.
//
// Schemas are omitempty so a callTable populated before the
// ArgsSchema fields existed still serializes a valid document —
// the comparison code degrades gracefully to type-name-only when
// either side omits the structural schema.
type MethodSchema struct {
	Name         string  `json:"name"`
	ArgsType     string  `json:"args_type,omitempty"`     // "" for no-arg methods
	ReturnType   string  `json:"return_type,omitempty"`   // "" for void/error-only handlers
	ArgsSchema   *Schema `json:"args_schema,omitempty"`   // structural shape of the args
	ReturnSchema *Schema `json:"return_schema,omitempty"` // structural shape of the return value
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
			ms.ArgsSchema = ReflectSchema(entry.ArgsType)
		}
		if entry.RetType != nil {
			ms.ReturnType = entry.RetType.String()
			ms.ReturnSchema = ReflectSchema(entry.RetType)
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
// Returns a typed *Error on hard-failing drift; nil otherwise.
//
// Drift severity by category:
//
//   - Method missing on peer → hard error (the call cannot
//     possibly succeed; surfacing it here is strictly faster
//     than letting the wire round-trip and bounce off a
//     NOT_FOUND envelope).
//   - Type mismatch on the JSON shape of a field (e.g. caller
//     sends string, peer expects integer) → hard error. JSON
//     decoder would reject on the peer side anyway; we want the
//     fail-fast at the call site so the operator sees the cause.
//   - Required field on peer that the caller doesn't send → hard
//     error. The caller's wire output would arrive with the
//     field zero-valued or missing; the peer's decoder will
//     either reject or silently misuse it.
//   - Caller has a property the peer doesn't know about →
//     soft (allowed). encoding/json on the peer side ignores
//     unknown keys, so the call succeeds; the data is just
//     ignored. Common during rolling deploys (caller is ahead).
//   - Type-name mismatch with matching structural schema →
//     soft. Two packages defining the same shape under different
//     names is annoying but JSON-compatible.
//
// When both sides emit JSON-Schema this function compares those
// structurally. When either side omits the schema (older peer,
// pre-jsonschema deploy), it falls back to the type-name check
// the rc2 version did.
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
	// Structural check on args (caller → peer). Direction
	// matters: the caller produces JSON; the peer consumes it.
	// "Peer requires X that caller doesn't send" is a fail.
	if argsType != nil && ms.ArgsSchema != nil {
		callerSchema := ReflectSchema(argsType)
		if err := compareSchemas(callerSchema, ms.ArgsSchema, "args", schema.Identity, method); err != nil {
			return err
		}
	}
	// Return shape is server → caller. The caller's JSON decoder
	// silently ignores unknown server fields; the only real
	// danger is type mismatch on a known field (peer says int
	// where caller expects string → decode error). The
	// compareSchemas function handles direction-agnostic
	// type-mismatch as fatal, which is correct here too.
	if returnType != nil && ms.ReturnSchema != nil {
		callerSchema := ReflectSchema(returnType)
		if err := compareSchemas(ms.ReturnSchema, callerSchema, "return", schema.Identity, method); err != nil {
			return err
		}
	}
	return nil
}

// compareSchemas verifies that producer's emitted shape is
// consumable by consumer. Direction implied by argument order:
// producer fields are what's on the wire; consumer fields are
// what the receiving side expects to decode.
//
// Failure cases (return a typed *Error):
//   - consumer marks a property required, producer doesn't emit it
//   - same property exists on both sides with mismatched primitive
//     types (object vs array, string vs integer, etc.)
//
// Pass cases (return nil):
//   - producer emits extra properties consumer doesn't know
//     (forward-compat: caller is on an older version of the type)
//   - both sides agree on a property's type+optionality
//   - either side omits a schema entirely (graceful degradation
//     to the type-name-only check, already passed above)
//
// Recursion: comparison walks Properties + Items one level deep,
// resolving $refs against producer's and consumer's own $defs so
// nested struct drift surfaces too. Cycle detection is unneeded:
// the schema builder already cut cycles into $refs, and we
// resolve each $ref at most once per comparison path via the
// `visited` set.
func compareSchemas(producer, consumer *Schema, label, peer, method string) error {
	return walkCompare(producer, consumer, label, peer, method,
		producer.Defs, consumer.Defs, map[string]bool{})
}

func walkCompare(prod, cons *Schema, label, peer, method string,
	prodDefs, consDefs map[string]*Schema, visited map[string]bool,
) error {
	if prod == nil || cons == nil {
		return nil
	}
	prod = resolveRef(prod, prodDefs, visited)
	cons = resolveRef(cons, consDefs, visited)
	if prod == nil || cons == nil {
		return nil
	}
	// Type mismatch on the same property: hard error. Empty
	// type on either side means "anything" (interface{} fields)
	// and short-circuits the check — there's nothing to compare.
	if prod.Type != "" && cons.Type != "" && prod.Type != cons.Type {
		return &Error{
			Code: "SCHEMA_MISMATCH",
			Msg: fmt.Sprintf("peer %q method %q %s: type mismatch (peer=%q, caller=%q)",
				peer, method, label, prod.Type, cons.Type),
			Details: map[string]any{
				"label":  label,
				"peer":   prod.Type,
				"caller": cons.Type,
			},
		}
	}
	// Consumer requires a property that the producer doesn't
	// emit at all → hard error. The receiving decoder won't
	// see the field, validation will reject or the handler
	// will silently misbehave.
	for _, req := range cons.Required {
		if _, ok := prod.Properties[req]; !ok {
			return &Error{
				Code: "SCHEMA_MISSING_REQUIRED",
				Msg: fmt.Sprintf("peer %q method %q %s: caller requires property %q that peer doesn't emit",
					peer, method, label, req),
				Details: map[string]any{
					"label":    label,
					"property": req,
				},
			}
		}
	}
	// Walk shared properties recursively. Properties present on
	// one side but not the other are forward-compat-friendly:
	// JSON decode ignores unknowns. Only shared properties can
	// drift in a meaningful way.
	for name, consProp := range cons.Properties {
		prodProp, ok := prod.Properties[name]
		if !ok {
			continue
		}
		if err := walkCompare(prodProp, consProp, label+"."+name, peer, method,
			prodDefs, consDefs, visited); err != nil {
			return err
		}
	}
	// Array element type: recurse so a slice-of-mismatched-struct
	// surfaces too, not just top-level array-vs-something.
	if prod.Items != nil && cons.Items != nil {
		if err := walkCompare(prod.Items, cons.Items, label+"[]", peer, method,
			prodDefs, consDefs, visited); err != nil {
			return err
		}
	}
	return nil
}

// resolveRef follows a $ref one hop into the supplied defs map.
// Returns the referenced schema, or the input unchanged when
// there's no ref. The visited set guards against pathological
// $refs that point at each other in a cycle the schema builder
// shouldn't normally produce — defense in depth, not a correctness
// requirement.
func resolveRef(s *Schema, defs map[string]*Schema, visited map[string]bool) *Schema {
	if s == nil || s.Ref == "" {
		return s
	}
	const prefix = "#/$defs/"
	if len(s.Ref) <= len(prefix) || s.Ref[:len(prefix)] != prefix {
		return s
	}
	name := s.Ref[len(prefix):]
	if visited[name] {
		return s // cycle — treat the $ref as opaque
	}
	target, ok := defs[name]
	if !ok {
		return s
	}
	visited[name] = true
	return target
}
