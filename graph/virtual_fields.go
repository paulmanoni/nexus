package graph

import (
	"reflect"
	"sync"

	"github.com/graphql-go/graphql"
)

// Virtual fields are graphql fields that don't correspond to a Go
// struct field on the parent type — they're declared externally
// (e.g. by nexus.LoadField) and merged into the type's field map at
// schema-build time. The use case is DataLoader-style batched
// resolvers: `User.bankDetail` is computed by joining against
// another store, not by reading a `BankDetail` field on the User
// struct.
//
// Registration is keyed by the parent Go type's name (the same name
// graphql-go uses for the SDL Object type). generateFieldsAt
// consults this registry after walking the struct so user
// registrations always win against a same-named struct field —
// which lets you override a sluggish reflection-driven field with
// a batched one.

var (
	virtualMu     sync.RWMutex
	virtualByType = map[string]graphql.Fields{}
)

// RegisterVirtualField installs field as a GraphQL field on the
// Object type derived from parentTypeName. fieldName is the SDL
// field name (camelCase). The framework's nexus.LoadField is the
// canonical caller; raw graphql-go users can call this directly.
//
// Idempotent on (parentTypeName, fieldName): later registrations
// overwrite. Concurrent-safe.
func RegisterVirtualField(parentTypeName, fieldName string, field *graphql.Field) {
	if parentTypeName == "" || fieldName == "" || field == nil {
		return
	}
	virtualMu.Lock()
	defer virtualMu.Unlock()
	bag, ok := virtualByType[parentTypeName]
	if !ok {
		bag = graphql.Fields{}
		virtualByType[parentTypeName] = bag
	}
	bag[fieldName] = field
}

// virtualFieldsFor returns a snapshot of virtual fields registered
// for parentTypeName, or nil if none. Used inside generateFieldsAt;
// callers don't see this directly.
func virtualFieldsFor(parentTypeName string) graphql.Fields {
	if parentTypeName == "" {
		return nil
	}
	virtualMu.RLock()
	bag, ok := virtualByType[parentTypeName]
	virtualMu.RUnlock()
	if !ok || len(bag) == 0 {
		return nil
	}
	out := make(graphql.Fields, len(bag))
	for k, v := range bag {
		out[k] = v
	}
	return out
}

// OutputType maps a Go reflect.Type to a graphql.Output. Mostly a
// thin wrapper over the package-internal type generator so external
// registrations (LoadField) can build typed fields without
// rebuilding the reflective machinery.
//
// Returns nil for unsupported kinds (channels, funcs, etc.). The
// caller should treat nil as "don't register this field."
func OutputType(t reflect.Type) graphql.Output {
	gen := NewFieldGenerator[any]()
	return gen.getBaseGraphQLType(t, nil)
}

// ResetVirtualFieldsForTest wipes the registry. Tests that build
// schemas through nexus must call this between runs so stale
// LoadField registrations don't bleed across cases.
func ResetVirtualFieldsForTest() {
	virtualMu.Lock()
	virtualByType = map[string]graphql.Fields{}
	virtualMu.Unlock()
}
