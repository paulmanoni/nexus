package nexus

import (
	"context"
	"reflect"

	"github.com/graphql-go/graphql"
)

// Params is the bundle a reflective resolver receives when it wants more
// than just typed args — namely the resolve context, parent source, or
// schema info. Use it as the last parameter of an AsQuery / AsMutation
// handler (or AsRest, where only Context is filled).
//
//	func NewCreateAdvert(
//	    svc *AdvertsService,
//	    dbs *DBManager,
//	    cache *CacheManager,
//	    p nexus.Params[CreateAdvertArgs],
//	) (*AdvertResponse, error) {
//	    advert := Advert{Title: p.Args.Title, EmployerName: p.Args.EmployerName}
//	    return create(p.Context, advert)
//	}
//
// The type parameter T is the args struct — its fields carry the same
// `graphql:"..."` and `validate:"..."` tags as the legacy flat-args form.
// Use Params[struct{}] for resolvers that need Context/Source/Info but
// have no user-supplied args.
//
// For simple handlers that only need a context.Context, you can still take
// that as a plain parameter; Params[T] is additive, not required.
type Params[T any] struct {
	Context context.Context
	Args    T
	Source  any
	Info    graphql.ResolveInfo
}

// isNexusParams is a marker method that lets the reflective handler walker
// recognise Params[T] without having to pattern-match on the generic type
// name. Every Params instantiation inherits it.
func (Params[T]) isNexusParams() {}

// nexusParamsMarker is the private interface the reflection walker tests
// against. Keeping it unexported prevents unrelated types from accidentally
// opting into the Params-slot treatment by defining the method.
type nexusParamsMarker interface {
	isNexusParams()
}

// paramsArgsField returns the "Args" field's type from a Params[T] type
// (t.Field for the struct). Panics if t doesn't match the shape — only
// called after isNexusParams() passes, so the shape is guaranteed.
func paramsArgsField(t reflect.Type) reflect.Type {
	f, ok := t.FieldByName("Args")
	if !ok {
		return nil
	}
	return f.Type
}

// buildParamsValue constructs a Params[T] reflect.Value with the supplied
// Context/Args/Source/Info. Used by as_graph and as_rest before calling a
// handler that takes a Params[T] parameter.
func buildParamsValue(paramsType reflect.Type, ctx context.Context, args reflect.Value, source any, info graphql.ResolveInfo) reflect.Value {
	p := reflect.New(paramsType).Elem()
	if f := p.FieldByName("Context"); f.IsValid() {
		if ctx == nil {
			ctx = context.Background()
		}
		f.Set(reflect.ValueOf(ctx))
	}
	if f := p.FieldByName("Args"); f.IsValid() && args.IsValid() {
		f.Set(args)
	}
	if f := p.FieldByName("Source"); f.IsValid() && source != nil {
		f.Set(reflect.ValueOf(source))
	}
	if f := p.FieldByName("Info"); f.IsValid() {
		f.Set(reflect.ValueOf(info))
	}
	return p
}
