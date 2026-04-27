package nexus

import (
	"context"

	"github.com/paulmanoni/nexus/transport/gql"
)

// SetGraphStatus overrides the HTTP status code for the current
// GraphQL request. Call from a graph.FieldMiddleware (the Graph
// realization of a middleware.Middleware bundle) or from a
// resolver to translate a decision into a non-200 response code:
//
//	authMw := middleware.Middleware{
//	    Name: "auth",
//	    Graph: func(p graphql.ResolveParams, next graphql.FieldResolveFn) (any, error) {
//	        if !authed(p.Context) {
//	            nexus.SetGraphStatus(p.Context, http.StatusUnauthorized)
//	            return nil, errors.New("unauthorized")
//	        }
//	        return next(p)
//	    },
//	}
//
// Without this call the framework returns 200 OK with errors in the
// GraphQL response body — the GraphQL-spec default. When called
// multiple times within one request, the LAST value wins.
//
// No-op when ctx didn't pass through the framework's GraphQL
// adapter — useful for resolver code under test with a bare
// graphql.Do call.
//
// Re-export of gql.SetStatusCode so user code stays on the
// `nexus.` import without pulling in the transport package.
func SetGraphStatus(ctx context.Context, code int) {
	gql.SetStatusCode(ctx, code)
}
