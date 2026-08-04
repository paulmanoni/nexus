package graph

import (
	"reflect"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"

	"github.com/paulmanoni/nexus/internal/maskhook"
)

// MaskedID is the scalar that stands in for Int on ID fields when the
// maskid extension is installed. GraphQL is the one transport where
// masking can't be a post-hoc rewrite of the response: graphql-go coerces
// every field through its declared type, so a masked string on an Int
// field would serialize to null. Declaring the field as MaskedID instead
// moves the conversion into the type system, which has the pleasant side
// effect of making the SDL honest — clients see `id: MaskedID`, not an
// Int that mysteriously arrives as a string.
//
// Both directions live here, so arguments typed MaskedID are already
// plain integers by the time a resolver's args struct is bound.
var MaskedID = graphql.NewScalar(graphql.ScalarConfig{
	Name: "MaskedID",
	Description: "An opaque, non-sequential identifier. Encodes an integer " +
		"primary key; treat it as a string and pass it back unmodified.",
	Serialize:    serializeMaskedID,
	ParseValue:   parseMaskedID,
	ParseLiteral: parseMaskedIDLiteral,
})

func serializeMaskedID(value any) any {
	n, ok := intOf(reflect.ValueOf(value))
	if !ok {
		return nil
	}
	s, ok := maskhook.Encode(n)
	if !ok {
		return n
	}
	return s
}

func parseMaskedID(value any) any {
	switch v := value.(type) {
	case string:
		if n, ok := maskhook.Decode(v); ok {
			return int(n)
		}
		return nil
	case int:
		// A raw integer is accepted so an internal client (or a test)
		// can keep passing unmasked ids.
		return v
	default:
		if n, ok := intOf(reflect.ValueOf(value)); ok {
			return int(n)
		}
		return nil
	}
}

func parseMaskedIDLiteral(valueAST ast.Value) any {
	switch v := valueAST.(type) {
	case *ast.StringValue:
		return parseMaskedID(v.Value)
	case *ast.IntValue:
		return parseMaskedID(v.Value)
	}
	return nil
}

func intOf(v reflect.Value) (int64, bool) {
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return 0, false
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(v.Uint()), true
	case reflect.String:
		// Already masked (or a string id) — leave it to the caller.
		return 0, false
	default:
		return 0, false
	}
}

// MaskType swaps Int for MaskedID on an ID field of an in-scope object,
// preserving NonNull and List wrappers. Any other type — String, a nested
// object, a custom scalar — is returned unchanged, so an `id string` field
// or an already-opaque key is never touched.
//
// objectName is the GraphQL object the field belongs to; it is what a
// scoped policy matches on, which is how an app masks the types that stay
// inside it while leaving alone the ones whose IDs travel elsewhere.
//
// A no-op unless the maskid extension is installed. Output types only:
// arguments keep their Int declaration, because a masked value is already
// converted back to an integer when the request body is bound, and
// changing argument types would churn the SDL and the generated client
// for every caller, in scope or not.
func MaskType(objectName, field string, goType reflect.Type, t graphql.Type) graphql.Type {
	if t == nil || !maskhook.Enabled() ||
		!maskhook.TypeAllowed(objectName) ||
		!maskhook.IsIDKey(field) || !isIntKind(goType) {
		return t
	}
	return swapInt(t)
}

func swapInt(t graphql.Type) graphql.Type {
	switch v := t.(type) {
	case *graphql.NonNull:
		if inner := swapInt(v.OfType); inner != v.OfType {
			return graphql.NewNonNull(inner)
		}
	case *graphql.List:
		if inner := swapInt(v.OfType); inner != v.OfType {
			return graphql.NewList(inner)
		}
	case *graphql.Scalar:
		if v == graphql.Int {
			return MaskedID
		}
	}
	return t
}

// isIntKind reports whether the Go type behind a field is an integer,
// looking through pointers and slices. Masking is only ever applied to
// integer keys — a string or UUID id is already opaque.
func isIntKind(t reflect.Type) bool {
	if t == nil {
		return false
	}
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	default:
		return false
	}
}
