package nexus

import (
	"errors"
	"fmt"
	"reflect"
)

// reflectIDAccessors derives get/set functions for the "ID" field
// of T. Looks for an exported, string-typed field literally named
// "ID" — the most common convention. Returns an error when no such
// field exists; AsCRUD surfaces it at boot so the user can either
// rename the field or pass explicit accessors via MemoryResolver's
// (or the future CRUDID option's) parameters.
func reflectIDAccessors[T any]() (func(*T) string, func(*T, string), error) {
	var zero T
	rt := reflect.TypeOf(zero)
	if rt.Kind() != reflect.Struct {
		return nil, nil, fmt.Errorf("nexus: AsCRUD[%s] requires a struct type", rt)
	}
	field, ok := rt.FieldByName("ID")
	if !ok {
		return nil, nil, fmt.Errorf("nexus: AsCRUD[%s] cannot infer ID — add an exported string field named \"ID\" or pass explicit accessors", rt)
	}
	if field.Type.Kind() != reflect.String {
		return nil, nil, fmt.Errorf("nexus: AsCRUD[%s] ID field must be a string (got %s)", rt, field.Type)
	}
	idx := field.Index
	get := func(t *T) string {
		v := reflect.ValueOf(t).Elem().FieldByIndex(idx)
		return v.String()
	}
	set := func(t *T, id string) {
		v := reflect.ValueOf(t).Elem().FieldByIndex(idx)
		if v.CanSet() {
			v.SetString(id)
		}
	}
	return get, set, nil
}

// errBadResolver is returned by AsCRUD when the resolver argument is
// nil — defensive check that surfaces a clear message instead of a
// nil-deref panic on the first request.
var errBadResolver = errors.New("nexus: AsCRUD resolver is nil — pass MemoryResolver[T]() or your own func(ctx) (Store[T], error)")