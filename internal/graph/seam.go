package graph

import (
	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"

	"github.com/paulmanoni/nexus/v2/gql"
)

// InfoOf converts the engine's resolve info into the public gql.Info.
func InfoOf(info graphql.ResolveInfo) gql.Info {
	out := gql.Info{FieldName: info.FieldName}
	if info.ParentType != nil {
		out.ParentType = info.ParentType.Name()
	}
	if op, ok := info.Operation.(*ast.OperationDefinition); ok && op != nil {
		out.Operation = op.Operation
		if op.Name != nil {
			out.OperationName = op.Name.Value
		}
	}
	return out
}

// FieldOf converts one resolve call into the public gql.Field.
func FieldOf(p ResolveParams) gql.Field {
	return gql.Field{Context: p.Context, Args: p.Args, Source: p.Source, Info: InfoOf(p.Info)}
}

// Adapt runs a public gql.Middleware as an engine field middleware. The
// Context, Args and Source the middleware passes to next replace the
// engine's; the rest of the resolve call is carried through unchanged.
func Adapt(mw gql.Middleware) FieldMiddleware {
	if mw == nil {
		return nil
	}
	return func(next FieldResolveFn) FieldResolveFn {
		return func(p ResolveParams) (any, error) {
			return mw(func(f gql.Field) (any, error) {
				q := p
				q.Context, q.Args, q.Source = f.Context, f.Args, f.Source
				return next(q)
			})(FieldOf(p))
		}
	}
}
