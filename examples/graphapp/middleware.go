package main

import (
	"errors"

	graph "github.com/paulmanoni/nexus/graph"
)

// AuthMiddleware is the shape every resolver attaches. In oats it validates a
// bearer token against the API_GATEWAY and caches the principal for 30 min.
// Here we only demonstrate the signature.
func AuthMiddleware(next graph.FieldResolveFn) graph.FieldResolveFn {
	return func(p graph.ResolveParams) (any, error) {
		// real code: tok := p.Context.Value("token"); validate; attach principal
		return next(p)
	}
}

// PermissionMiddleware enforces that the current principal holds at least one
// of the given roles. Same shape as oats's `middlewares.PermissionMiddleware`.
func PermissionMiddleware(perms []string) graph.FieldMiddleware {
	return func(next graph.FieldResolveFn) graph.FieldResolveFn {
		return func(p graph.ResolveParams) (any, error) {
			if len(perms) == 0 {
				return nil, errors.New("no permissions configured")
			}
			// real code: principal := p.Context.Value("principal").(*Principal)
			// for _, want := range perms { if principal.HasRole(want) return next(p) }
			// return nil, ErrForbidden
			return next(p)
		}
	}
}
