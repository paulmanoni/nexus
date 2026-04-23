package graph

import (
	"reflect"
	"strings"

	"github.com/graphql-go/graphql"
)

// NewResolverFromType is the non-generic sibling of NewResolver[T]. Use it
// when the return type is only known at runtime — typically driven by
// reflection on a handler function signature. The resulting resolver behaves
// identically to NewResolver[T]: same chainable API, same Serve path, same
// FieldInfo introspection.
//
// The framework-facing use is nexus.AsQuery / AsMutation, which extract the
// return type from the handler function and call this to avoid forcing the
// caller to repeat `[T]` at the registration site.
//
// returnType should be the concrete Go type the resolver returns. Pointer
// return types (*T) are dereferenced automatically. Slice return types ([]T)
// are detected and flagged isList.
func NewResolverFromType(name string, returnType reflect.Type) *UnifiedResolver[any] {
	// Deref a *T or **T so the internal type registry keys on the concrete
	// element. Slices stay slices — UnifiedResolver's isList path handles them.
	t := returnType
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	r := &UnifiedResolver[any]{
		name:            name,
		objectName:      objectNameFromType(t),
		typeOverride:    t,
		fieldOverrides:  make(map[string]graphql.FieldResolveFn),
		fieldMiddleware: make(map[string][]FieldMiddleware),
		customFields:    make(graphql.Fields),
	}
	if t != nil && t.Kind() == reflect.Slice {
		r.isList = true
		r.isListManuallyAssigned = true
	}
	return r
}

// WithRawResolver sets the resolver directly without the typed (*T, error)
// wrapper. Used by nexus.AsQuery / AsMutation when the handler is invoked
// via reflection and results are already `any`-typed.
//
// Prefer WithResolver when you have a statically-typed handler — you lose
// nothing and the signature documents the return shape.
func (r *UnifiedResolver[T]) WithRawResolver(resolver func(ResolveParams) (any, error)) *UnifiedResolver[T] {
	r.resolver = func(p graphql.ResolveParams) (interface{}, error) {
		return resolver(ResolveParams(p))
	}
	return r
}

// objectNameFromType produces the GraphQL object name from a runtime
// reflect.Type. Mirrors GetTypeName[T]() which works from a compile-time
// type parameter: for a slice it returns "ListElement", for a generic
// wrapper like Response[User] it returns "Response_User", otherwise the
// plain Go type name with package path and pointer markers stripped.
func objectNameFromType(t reflect.Type) string {
	if t == nil {
		return "interface"
	}
	// Pointers were already stripped by the caller, but be defensive.
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	isSlice := t.Kind() == reflect.Slice
	if isSlice {
		t = t.Elem()
	}

	name := t.String()
	if idx := strings.Index(name, "["); idx >= 0 {
		base := name[:idx]
		param := name[idx:]
		if dot := strings.LastIndex(base, "."); dot >= 0 {
			base = base[dot+1:]
		}
		param = strings.ReplaceAll(param, "[", " [ ")
		param = strings.ReplaceAll(param, "]", " ] ")
		parts := strings.Fields(param)
		for i, p := range parts {
			if p == "[" || p == "]" {
				continue
			}
			if dot := strings.LastIndex(p, "."); dot >= 0 {
				parts[i] = p[dot+1:]
			}
		}
		name = base + strings.Join(parts, "")
	} else if dot := strings.LastIndex(name, "."); dot >= 0 {
		name = name[dot+1:]
	}
	name = strings.ReplaceAll(name, "[]", "")
	name = strings.ReplaceAll(name, "*", "")
	name = strings.ReplaceAll(name, "[", "_")
	name = strings.ReplaceAll(name, "]", "")
	name = strings.ReplaceAll(name, " ", "_")
	if isSlice {
		name = "List" + name
	}
	return sanitizeTypeName(name)
}
