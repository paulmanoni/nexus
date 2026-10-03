package main

import (
	"errors"

	"github.com/paulmanoni/nexus/v2/gql"
)

// AuthMiddleware shows the shape of a GraphQL-only middleware: it wraps the
// field's resolver and decides whether to call it. A real one would
// validate a bearer token and put the principal on f.Context.
func AuthMiddleware(next gql.Resolver) gql.Resolver {
	return func(f gql.Field) (any, error) {
		return next(f)
	}
}

// PermissionMiddleware enforces that the current principal holds at least
// one of the given roles.
func PermissionMiddleware(perms []string) gql.Middleware {
	return func(next gql.Resolver) gql.Resolver {
		return func(f gql.Field) (any, error) {
			if len(perms) == 0 {
				return nil, errors.New("no permissions configured")
			}
			// real code: read the principal from f.Context, check perms,
			// return nil, nexus.Forbidden when none match.
			return next(f)
		}
	}
}
