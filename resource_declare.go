package nexus

import (
	"fmt"
	"reflect"

	"github.com/paulmanoni/nexus/resource"
)

// Shared plumbing for the declarative resource binders (Database[T],
// Cache[T], and pubsub.Broker[T]'s sibling). Each binder asks T to
// embed a manager/transport type; these helpers locate and set that
// embedded field, and carry the common dashboard-resource knobs.

// resourceConfig holds the dashboard-resource knobs common to every
// declarative binder. Distinct option types (DatabaseOption,
// CacheOption, …) all configure this so you can't pass a CacheOption to
// Database, while the binders share one implementation.
type resourceConfig struct {
	description string
	details     map[string]any
	asDefault   bool
}

func (c resourceConfig) resourceOpts() []resource.Option {
	if c.asDefault {
		return []resource.Option{resource.AsDefault()}
	}
	return nil
}

// embeddedFieldIndex returns the index of structT's anonymous field of
// exactly wantType (e.g. *db.Manager, *cache.Manager). Only embedded
// (anonymous) fields count, so an incidental named field of the same
// type doesn't accidentally qualify.
func embeddedFieldIndex(structT, wantType reflect.Type) (int, error) {
	if structT == nil || structT.Kind() != reflect.Struct {
		return 0, fmt.Errorf("T must be a struct embedding %v, got %v", wantType, structT)
	}
	for i := 0; i < structT.NumField(); i++ {
		f := structT.Field(i)
		if f.Anonymous && f.Type == wantType {
			return i, nil
		}
	}
	return 0, fmt.Errorf("T (%s) must embed %v, e.g. `type %s struct{ %v }`",
		structT, wantType, structT.Name(), wantType)
}

// mustEmbeddedField resolves the embedded field index for T at wiring
// time, panicking with a binder-qualified message if T doesn't embed
// wantType. Fail-fast: misuse surfaces before boot, never at runtime.
func mustEmbeddedField[T any](wantType reflect.Type, binder string) int {
	idx, err := embeddedFieldIndex(reflect.TypeFor[T](), wantType)
	if err != nil {
		panic(binder + ": " + err.Error())
	}
	return idx
}

// requireResourceArgs validates the name + build args common to every
// binder, panicking at wiring time on misuse.
func requireResourceArgs(binder, name string, nilBuild bool) {
	if name == "" {
		panic(binder + ": name must not be empty")
	}
	if nilBuild {
		panic(binder + ": build func must not be nil")
	}
}

// newHandle allocates *T and sets its embedded manager/transport field
// (pre-resolved fieldIdx) to mgr. This is the one reflective step in
// the whole mechanism and runs exactly once per resource, at
// construction — never on the request path, where *T's promoted methods
// are plain direct calls identical to a hand-written handle.
func newHandle[T any](fieldIdx int, mgr any) *T {
	h := new(T)
	reflect.ValueOf(h).Elem().Field(fieldIdx).Set(reflect.ValueOf(mgr))
	return h
}
