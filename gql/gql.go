// Package gql holds the types nexus's GraphQL surface is written in. They
// name no GraphQL engine: nexus executes queries with graphql-go today, and
// the engine sits behind these types so a later release can replace it
// without breaking handlers or middleware.
//
// A handler reads the field it serves through nexus.Params[T].Info; a
// GraphQL-only middleware is a Middleware, attached with
// nexus.GraphMiddleware or as the Graph realization of a
// middleware.Middleware bundle.
package gql

import "context"

// Info describes the field being resolved.
type Info struct {
	// FieldName is the field's name in the schema ("createUser").
	FieldName string
	// ParentType is the object type the field belongs to ("Query",
	// "Mutation", "User").
	ParentType string
	// Operation is the kind of the operation being executed: "query",
	// "mutation" or "subscription".
	Operation string
	// OperationName is the client's name for the operation, empty for an
	// anonymous one.
	OperationName string
}

// Field is one field resolution as a middleware sees it. A middleware may
// change Context, Args or Source before calling next; the resolver sees the
// values it passes on.
type Field struct {
	Context context.Context
	// Args are the field's arguments as the engine coerced them: scalars,
	// []any for lists and map[string]any for input objects.
	Args map[string]any
	// Source is the parent object's value (nil for a root field).
	Source any
	Info   Info
}

// Resolver resolves a field.
type Resolver func(f Field) (any, error)

// Middleware wraps a field's resolver. Return next(f) to continue, or an
// error to fail the field without resolving it.
//
//	func Audit(next gql.Resolver) gql.Resolver {
//	    return func(f gql.Field) (any, error) {
//	        log.Printf("resolving %s.%s", f.Info.ParentType, f.Info.FieldName)
//	        return next(f)
//	    }
//	}
type Middleware func(next Resolver) Resolver
